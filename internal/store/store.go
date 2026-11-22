package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rpcLogFile = "rpc.jsonl"

	// defaultMaxLogBytes triggers rotation of rpc.jsonl once it grows past this size.
	defaultMaxLogBytes int64 = 8 << 20 // 8 MiB

	// maxLogBackups is how many rotated rpc.jsonl.N files are kept for audit.
	maxLogBackups = 3

	// rpc.jsonl keeps caller-submitted request bodies (fill text, fetch headers,
	// eval scripts), so the state dir and its log files are owner-only.
	stateDirPerm os.FileMode = 0o700
	logFilePerm  os.FileMode = 0o600
)

// Managed-tab lifecycle records. They are written synchronously by the store
// (never through the asynchronous RPC audit path) and are the only records that
// add or remove persisted managed-tab membership. Both carry the full CDP
// target id in LogRecord.Target; the short tab id is informational only.
const (
	// ActionManagedTabOpen registers a daemon-created tab. It is also the
	// record used to seed a freshly rotated log with the live managed tabs.
	ActionManagedTabOpen = "managed_tab_open"
	// ActionManagedTabRetire ends a managed tab's lifecycle (closed by RPC,
	// closed by the idle sweeper, or confirmed gone from the browser).
	ActionManagedTabRetire = "managed_tab_retire"
)

// lifecycleBody is the placeholder body of lifecycle records (they are not
// JSON-RPC requests; recovery only needs Action/Tab/Target/Time).
var lifecycleBody = json.RawMessage(`{}`)

// ErrClosed is returned by writes after Close on a disk-backed store.
var ErrClosed = errors.New("rpc log store closed")

// chmodPath is os.Chmod, replaceable in tests to simulate a failing chmod.
var chmodPath = os.Chmod

// RPCLogFile returns the on-disk RPC log filename under StateDir.
func RPCLogFile() string { return rpcLogFile }

// OpenConfig selects disk or in-memory RPC log storage.
type OpenConfig struct {
	// StateDir is the daemon state directory. "-" forces in-memory mode.
	StateDir string
	Logger   *slog.Logger
	// MaxLogBytes rotates rpc.jsonl once it exceeds this size. <=0 uses defaultMaxLogBytes.
	MaxLogBytes int64
}

// Store persists tab-related RPC lines and allocates global seq (INV-4).
type Store struct {
	mu          sync.Mutex
	logFile     *os.File
	logPath     string
	logBytes    int64
	maxLogBytes int64
	seq         atomic.Uint64
	managed     map[string]managedEntry // keyed by full CDP target id
	logger      *slog.Logger
	inMemory    bool
	closed      bool

	// needNewline is set when the log does not end on a line boundary (complete
	// last record without '\n', or a fragment left by a short write). The next
	// write must emit a separating '\n' first so records never glue together.
	needNewline bool

	// writeLog overrides the raw log write (tests inject short writes/errors).
	// nil uses f.Write.
	writeLog func(f *os.File, p []byte) (int, error)
}

// LogRecord is one append-only RPC log line.
type LogRecord struct {
	Action string          `json:"action"`
	Body   json.RawMessage `json:"body"`
	Tab    string          `json:"tab,omitempty"`
	// Target is the full CDP target id the record refers to. Managed-tab
	// membership and activity are matched on it, never on the short Tab alias.
	Target   string    `json:"target,omitempty"`
	SenderIP string    `json:"sender_ip"`
	Seq      uint64    `json:"seq,omitempty"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	Time     time.Time `json:"time"`
}

// Open initializes rpc.jsonl on disk, or in-memory when cfg.StateDir is "" or
// "-". A configured StateDir that cannot be created or written returns an error
// rather than silently falling back to in-memory. The state dir, rpc.jsonl and
// its rotated backups are restricted to the owner (0700 / 0600) before the log
// is replayed; pre-existing wider modes are tightened, and a failure to do so
// is an error. The global seq is seeded from
// the current wall-clock nanosecond so it stays monotonic across restarts
// (INV-4) without persisting a counter.
func Open(cfg OpenConfig) (*Store, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dir := strings.TrimSpace(cfg.StateDir)
	useMemory := dir == "" || dir == "-"

	maxLogBytes := cfg.MaxLogBytes
	if maxLogBytes <= 0 {
		maxLogBytes = defaultMaxLogBytes
	}
	s := &Store{
		logger:      logger,
		inMemory:    useMemory,
		managed:     make(map[string]managedEntry),
		maxLogBytes: maxLogBytes,
	}
	s.seq.Store(uint64(time.Now().UnixNano()))

	if useMemory {
		return s, nil
	}

	dir = filepath.Clean(dir)
	logPath := filepath.Join(dir, rpcLogFile)
	if err := os.MkdirAll(dir, stateDirPerm); err != nil {
		return nil, fmt.Errorf("create state dir %q: %w", dir, err)
	}
	if err := tightenPath(dir, stateDirPerm); err != nil {
		return nil, fmt.Errorf("restrict state dir %q: %w", dir, err)
	}
	if !dirWritable(dir) {
		return nil, fmt.Errorf("state dir %q is not writable", dir)
	}
	for k := 1; k <= maxLogBackups; k++ {
		backup := fmt.Sprintf("%s.%d", logPath, k)
		fi, err := os.Lstat(backup)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := tightenPath(backup, logFilePerm); err != nil {
			return nil, fmt.Errorf("restrict rpc log backup %q: %w", backup, err)
		}
	}

	f, err := openPrivateLog(logPath, os.O_APPEND|os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("open rpc log %q: %w", logPath, err)
	}

	managed := make(map[string]managedEntry)
	legacy := make(map[string]struct{})
	needNewline, err := replayLog(f, logger, func(rec LogRecord) error {
		applyManagedUpdate(managed, rec)
		trackLegacyManaged(legacy, rec)
		return nil
	})
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("replay rpc log: %w", err)
	}
	if len(legacy) > 0 {
		// Written before target identity was persisted: a short id alone cannot
		// prove a live tab is the one the daemon created, so these are dropped.
		logger.Warn("rpc log has managed tabs without target identity; not restoring them for idle cleanup", "count", len(legacy))
	}

	s.logFile = f
	s.logPath = logPath
	s.managed = managed
	s.needNewline = needNewline
	if fi, err := f.Stat(); err == nil {
		s.logBytes = fi.Size()
	}
	return s, nil
}

// tightenPath removes permission bits outside max from an existing path. Modes
// already within max are left untouched (never loosened). Windows has no POSIX
// permission bits, so it is a no-op there.
func tightenPath(path string, max os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	perm := fi.Mode().Perm()
	if perm&^max == 0 {
		return nil
	}
	return chmodPath(path, perm&max)
}

// openPrivateLog opens (or creates) the RPC log owner-only. A pre-existing file
// keeps its old mode through O_CREATE, so it is tightened on the open handle
// before anything is read from or written to it.
func openPrivateLog(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag, logFilePerm)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		return f, nil
	}
	fi, err := f.Stat()
	if err == nil {
		if perm := fi.Mode().Perm(); perm&^logFilePerm != 0 {
			err = f.Chmod(perm & logFilePerm)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("restrict permissions: %w", err)
	}
	return f, nil
}

func dirWritable(dir string) bool {
	testPath := filepath.Join(dir, ".write-test")
	if err := os.WriteFile(testPath, []byte("1"), logFilePerm); err != nil {
		return false
	}
	_ = os.Remove(testPath)
	return true
}

// replayLog reads the full log from the start, invoking fn per record, then
// leaves the file positioned at the end for subsequent appends.
//
// Records are separated by physical newlines. A newline-terminated line that
// does not parse is logged and skipped (left on disk); independent records
// after it are still replayed. No attempt is made to split glued records.
//
// The final line without a trailing newline is either a complete record whose
// newline never reached disk — it is replayed and needNewline is returned true
// so the next append restores the line boundary first — or an incomplete
// (crash-truncated) record, which is truncated away.
func replayLog(f *os.File, logger *slog.Logger, fn func(LogRecord) error) (needNewline bool, err error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}

	var offset int64 // start of the line being read
	rd := bufio.NewReader(f)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil && err != io.EOF {
			_, _ = f.Seek(0, io.SeekEnd)
			return false, err
		}
		terminated := err == nil
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var rec LogRecord
			if jerr := json.Unmarshal(trimmed, &rec); jerr != nil {
				if !terminated {
					if logger != nil {
						logger.Warn("rpc log ends with incomplete record; truncating it", "err", jerr, "offset", offset)
					}
					if terr := f.Truncate(offset); terr != nil {
						return false, fmt.Errorf("truncate incomplete rpc log tail at %d: %w", offset, terr)
					}
					_, seekErr := f.Seek(0, io.SeekEnd)
					return false, seekErr
				}
				if logger != nil {
					logger.Warn("rpc log line corrupt; skipping it", "err", jerr, "offset", offset)
				}
			} else if fnErr := fn(rec); fnErr != nil {
				_, _ = f.Seek(0, io.SeekEnd)
				return false, fnErr
			}
		}
		offset += int64(len(line))
		if !terminated {
			_, seekErr := f.Seek(0, io.SeekEnd)
			return len(line) > 0, seekErr
		}
	}
}

// InMemory reports whether the store uses in-memory mode (no disk persistence).
func (s *Store) InMemory() bool {
	if s == nil {
		return true
	}
	return s.inMemory
}

// Close releases store resources.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.logFile != nil {
		err := s.logFile.Close()
		s.logFile = nil
		return err
	}
	return nil
}

// NextSeq returns the next globally monotonic sequence number (INV-4). It is
// seeded from the wall-clock nanosecond at Open, so values keep increasing
// across restarts without persisting a counter.
func (s *Store) NextSeq() (uint64, error) {
	if s == nil {
		return 0, fmt.Errorf("store not initialized")
	}
	return s.seq.Add(1), nil
}

// AppendRPC appends one RPC audit line. Audit lines never add or remove
// managed-tab membership (see RegisterManagedTab / RetireManagedTarget); a
// successful tab-related line only refreshes the last-activity time of the
// managed tab whose full target id equals rec.Target.
// In-memory mode validates the record and updates activity the same way, but
// discards the line instead of retaining it.
func (s *Store) AppendRPC(rec LogRecord) error {
	if s == nil {
		return fmt.Errorf("store not initialized")
	}
	if rec.Action == "" {
		return fmt.Errorf("log record action required")
	}
	if isLifecycleAction(rec.Action) {
		return fmt.Errorf("log record action %q is reserved for managed-tab lifecycle", rec.Action)
	}
	if len(rec.Body) == 0 {
		return fmt.Errorf("log record body required")
	}
	if rec.Time.IsZero() {
		rec.Time = time.Now().UTC()
	}
	rec.Body = compactRPCLogBody(rec.Body)

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(rec)
}

// RegisterManagedTab records that the daemon created targetID (short id tab)
// and that it was last active at `at`. Registering the same target again only
// replaces its entry.
//
// The in-memory managed set is always updated. A non-nil error means the
// lifecycle record was not written to rpc.jsonl, so the tab will not be
// restored as managed after a restart (it is then left open, never adopted
// through its alias); callers should log it.
func (s *Store) RegisterManagedTab(tab, targetID string, at time.Time) error {
	if s == nil {
		return fmt.Errorf("store not initialized")
	}
	if targetID == "" {
		return fmt.Errorf("managed tab target id required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(LogRecord{
		Action: ActionManagedTabOpen,
		Body:   lifecycleBody,
		Tab:    tab,
		Target: targetID,
		OK:     true,
		Time:   at,
	})
}

// RetireManagedTarget removes targetID from the managed set. It reports false
// and writes nothing when targetID is not managed, so it is safe to call for
// every removed target.
//
// The in-memory managed set is always updated, and later rotation snapshots no
// longer contain the tab. A non-nil error means the retirement record was not
// written to rpc.jsonl: the entry may reappear in the persisted set after a
// restart, where it is harmless (recovery requires the exact target id to be
// live) and is retired again once an enumeration confirms it is gone. Callers
// should log it rather than treat the retirement as durable.
func (s *Store) RetireManagedTarget(targetID string) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("store not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ent, ok := s.managed[targetID]
	if !ok {
		return false, nil
	}
	return true, s.appendLocked(LogRecord{
		Action: ActionManagedTabRetire,
		Body:   lifecycleBody,
		Tab:    ent.tab,
		Target: targetID,
		OK:     true,
		Time:   time.Now().UTC(),
	})
}

// appendLocked folds rec into the managed set and writes it. Memory and disk
// are updated under the same lock, so the on-disk order of lifecycle records
// always matches the in-memory order. Caller holds s.mu.
func (s *Store) appendLocked(rec LogRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line := append(data, '\n')

	if s.managed == nil {
		s.managed = make(map[string]managedEntry)
	}
	applyManagedUpdate(s.managed, rec)

	if s.inMemory {
		// No history is kept: nothing reads it, and retaining every record
		// (and its body) would grow without bound under long-running RPC.
		return nil
	}
	if s.logFile == nil {
		if s.closed {
			return ErrClosed
		}
		return fmt.Errorf("log file not open")
	}
	if err := s.writeLinesLocked(line); err != nil {
		return err
	}
	if s.maxLogBytes > 0 && s.logBytes >= s.maxLogBytes {
		if err := s.rotateLocked(); err != nil && s.logger != nil {
			s.logger.Warn("rpc log rotation failed", "err", err)
		}
	}
	return nil
}

// rotateLocked rolls rpc.jsonl into numbered backups and starts a fresh log,
// seeding it with a snapshot of currently managed tabs so idle recovery still
// works after reading only the current file. Caller holds s.mu.
func (s *Store) rotateLocked() error {
	if s.logFile == nil {
		return nil
	}
	if err := s.logFile.Close(); err != nil && s.logger != nil {
		s.logger.Warn("closing rpc log before rotation failed", "err", err)
	}
	s.logFile = nil

	if maxLogBackups <= 0 {
		_ = os.Remove(s.logPath)
	} else {
		_ = os.Remove(fmt.Sprintf("%s.%d", s.logPath, maxLogBackups))
		for k := maxLogBackups - 1; k >= 1; k-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", s.logPath, k), fmt.Sprintf("%s.%d", s.logPath, k+1))
		}
		_ = os.Rename(s.logPath, fmt.Sprintf("%s.1", s.logPath))
	}

	f, err := openPrivateLog(s.logPath, os.O_APPEND|os.O_CREATE|os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return err
	}
	s.logFile = f
	s.logBytes = 0
	s.needNewline = false
	return s.writeManagedSnapshotLocked()
}

// writeManagedSnapshotLocked writes one managed_tab_open record per currently
// managed tab (sorted by target id for determinism) to the current log, keeping
// each tab's full target identity and last-activity time. Retired tabs are not
// in s.managed and therefore never reseeded. Caller holds s.mu.
func (s *Store) writeManagedSnapshotLocked() error {
	if len(s.managed) == 0 {
		return nil
	}
	targets := make([]string, 0, len(s.managed))
	for id := range s.managed {
		targets = append(targets, id)
	}
	sort.Strings(targets)

	var buf bytes.Buffer
	for _, id := range targets {
		ent := s.managed[id]
		data, err := json.Marshal(LogRecord{
			Action: ActionManagedTabOpen,
			Body:   lifecycleBody,
			Tab:    ent.tab,
			Target: id,
			OK:     true,
			Time:   ent.at,
		})
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return s.writeLinesLocked(buf.Bytes())
}

// writeLinesLocked appends p (one or more complete, newline-terminated lines)
// to the current log. It first restores the line boundary if an earlier write
// or the replayed tail left the file mid-line, and refuses to append while
// that fails. logBytes tracks the bytes actually written, and a short write
// re-arms the boundary repair for the next call. Caller holds s.mu.
func (s *Store) writeLinesLocked(p []byte) error {
	if s.needNewline {
		n, err := s.rawWriteLocked([]byte{'\n'})
		if n > 0 {
			s.logBytes += int64(n)
			s.needNewline = false
		} else if err == nil {
			err = io.ErrShortWrite
		}
		if err != nil {
			return fmt.Errorf("restore rpc log line boundary: %w", err)
		}
	}
	n, err := s.rawWriteLocked(p)
	if n > len(p) {
		n = len(p)
	}
	if n > 0 {
		s.logBytes += int64(n)
		s.needNewline = p[n-1] != '\n'
	}
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return err
}

// rawWriteLocked performs one raw write to the current log. Caller holds s.mu.
func (s *Store) rawWriteLocked(p []byte) (int, error) {
	if s.writeLog != nil {
		return s.writeLog(s.logFile, p)
	}
	return s.logFile.Write(p)
}

// ManagedTab is one persisted daemon-created tab.
type ManagedTab struct {
	// Tab is the short id the tab had when it was registered (informational).
	Tab string
	// TargetID is the full CDP target id; the tab's identity.
	TargetID string
	// LastActivity is the time of its last successful tab-related RPC.
	LastActivity time.Time
}

// ManagedTabs returns the managed tabs with their full target identity, sorted
// by target id. Recovery must match on TargetID, not on the short alias.
func (s *Store) ManagedTabs() []ManagedTab {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ManagedTab, 0, len(s.managed))
	for id, ent := range s.managed {
		out = append(out, ManagedTab{Tab: ent.tab, TargetID: id, LastActivity: ent.at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out
}

// ReplayManagedTabActivity returns the short ids and last activity of the
// managed tabs (see ManagedTabs). Short ids are aliases, not identities: use
// ManagedTabs to decide whether a live tab is the one the daemon created.
func (s *Store) ReplayManagedTabActivity() map[string]time.Time {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.managed))
	for _, ent := range s.managed {
		if prev, ok := out[ent.tab]; !ok || ent.at.After(prev) {
			out[ent.tab] = ent.at
		}
	}
	return out
}
