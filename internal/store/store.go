package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	// rotateTmpSuffix names the private file a rotation writes its snapshot to
	// before publishing it as rpc.jsonl. It is never replayed.
	rotateTmpSuffix = ".tmp"

	// rotateKeepSuffix names the private hard link a rotation gives the active
	// log before any backup slot changes; it is renamed over rpc.jsonl.1. A
	// leftover is only a second name of a log and is removed at startup.
	rotateKeepSuffix = ".keep"

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
// target id in LogRecord.Target.
const (
	// ActionManagedTabOpen registers a daemon-created tab. It is also the
	// record used to seed a freshly rotated log with the live managed tabs.
	ActionManagedTabOpen = "managed_tab_open"
	// ActionManagedTabRetire ends a managed tab's lifecycle (closed by RPC,
	// closed by the idle sweeper, or confirmed gone from the browser).
	ActionManagedTabRetire = "managed_tab_retire"
)

// lifecycleBody is the placeholder body of lifecycle records (they are not
// JSON-RPC requests; recovery only needs Action/Target/Time).
var lifecycleBody = json.RawMessage(`{}`)

// ErrClosed is returned by writes after Close on a disk-backed store.
var ErrClosed = errors.New("rpc log store closed")

// ErrRotation wraps a log rotation that failed before its snapshot was
// published. The previous log stays the active one and the rotation is retried
// by the next append. Whether the record of the append that reports it was
// written is answered by RecordWritten, not by this sentinel.
var ErrRotation = errors.New("rpc log rotation failed")

// ErrDirSync wraps a failed state-dir fsync after a rotation was published.
// The published snapshot is the active log (the rotation is not repeated);
// only the directory entries are not confirmed durable yet. The sync alone is
// retried by every later append — whatever the log size, whether or not that
// append's own write or rotation succeeds — and by Close, and each of them
// reports it while it keeps failing. It can therefore accompany an append
// whose record was not written: ask RecordWritten about the record.
var ErrDirSync = errors.New("rpc log rotated but state dir sync failed")

// maintenanceError is the error of an append whose record was written in full
// to the open log handle while the log maintenance around it (rotation, or the
// state-dir sync a published rotation still owes) failed. It wraps ErrRotation
// and/or ErrDirSync.
type maintenanceError struct{ err error }

func (e *maintenanceError) Error() string {
	return "record written, rpc log maintenance failed: " + e.err.Error()
}

func (e *maintenanceError) Unwrap() error { return e.err }

// RecordWritten reports whether a non-nil error returned by AppendRPC,
// RegisterManagedTab or RetireManagedTarget still means that this call's record
// was written in full to the open log handle, only the maintenance around the
// write having failed. It says nothing else: an append whose own write failed
// reports false even when it also carries ErrDirSync for a sync owed by an
// earlier rotation, so test the sentinels with errors.Is and the write with
// RecordWritten. "Written" is not "recoverable after a restart": the directory
// entries may be unconfirmed (ErrDirSync) or the name rpc.jsonl may no longer
// lead to the open log (a refused rotation, ErrRotation).
func RecordWritten(err error) bool {
	var m *maintenanceError
	return errors.As(err, &m)
}

// File-system steps, replaceable in tests to inject failures.
var (
	chmodPath  = os.Chmod
	openFile   = os.OpenFile
	linkPath   = os.Link
	renamePath = os.Rename
	syncFile   = (*os.File).Sync
)

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
	managed     map[string]time.Time // keyed by full CDP target id
	logger      *slog.Logger
	inMemory    bool
	closed      bool

	// needNewline is set when the log does not end on a line boundary (complete
	// last record without '\n', or a fragment left by a short write). The next
	// write must emit a separating '\n' first so records never glue together.
	needNewline bool

	// dirSyncPending is set when a rotation was published but the state dir
	// could not be fsynced afterwards. It is retried by later appends (and
	// Close) independently of rotation, which already happened.
	dirSyncPending bool

	// writeLog overrides the raw log write (tests inject short writes/errors).
	// nil uses f.Write.
	writeLog func(f *os.File, p []byte) (int, error)
}

// LogRecord is one append-only RPC log line.
//
// Target is the complete CDP target id the record refers to (for an RPC audit
// line: the id the request was resolved to). It is the only field recovery
// reads a tab identity from: managed-tab membership and activity are matched on
// it by exact equality. Older logs also named tabs by a short suffix alias
// under "tab"; that key is no longer written or read, so a record without
// Target identifies no tab.
type LogRecord struct {
	Action   string          `json:"action"`
	Body     json.RawMessage `json:"body"`
	Target   string          `json:"target,omitempty"`
	SenderIP string          `json:"sender_ip"`
	Seq      uint64          `json:"seq,omitempty"`
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Time     time.Time       `json:"time"`
}

// Open initializes rpc.jsonl on disk, or in-memory when cfg.StateDir is "" or
// "-". A disk state dir must belong to one Store (one daemon process) at a
// time: nothing arbitrates between two writers of the same dir. A configured StateDir that cannot be created or written returns an error
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
		managed:     make(map[string]time.Time),
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
	if err := probeDirWritable(dir); err != nil {
		return nil, fmt.Errorf("state dir %q is not writable: %w", dir, err)
	}
	// Decide whether this dir can be started from before changing anything in
	// it (the probe above only ever touched its own file): a refused startup
	// leaves staging files and backup modes exactly as it found them.
	//
	// Opening follows symbolic links and creates what a dangling one points to;
	// rotation only ever works on a regular rpc.jsonl. Refuse anything else
	// here, before the open, so neither the link's destination nor rpc.jsonl.1
	// is touched.
	if fi, err := os.Lstat(logPath); err == nil && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("rpc log %q is not a regular file (%s): refusing to open it; replace it with the regular log file or move it away",
			logPath, fi.Mode().Type())
	}
	if err := restorePreviousLog(logPath, logger); err != nil {
		return nil, err
	}

	// A snapshot a previous process did not get to publish is not a log, and a
	// staging link it left is only another name of one.
	for _, suffix := range []string{rotateTmpSuffix, rotateKeepSuffix} {
		if fi, err := os.Lstat(logPath + suffix); err == nil && fi.Mode().IsRegular() {
			_ = os.Remove(logPath + suffix)
		}
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

	managed := make(map[string]time.Time)
	unidentified := 0
	needNewline, err := replayLog(f, logger, func(rec LogRecord) error {
		applyManagedUpdate(managed, rec)
		if isUnidentifiedTabNew(rec) {
			unidentified++
		}
		return nil
	})
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("replay rpc log: %w", err)
	}
	if unidentified > 0 {
		// Written by a daemon that named tabs by short alias: an alias cannot
		// prove a live tab is the one the daemon created, so these are dropped.
		logger.Warn("rpc log has tab_new records without a complete target id (written by an older daemon); those tabs are not adopted for idle cleanup",
			"records", unidentified)
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

// probeDirWritable checks that files can be created in dir. It creates its own
// exclusively named file (never an existing one) and removes only that.
func probeDirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	cerr := f.Close()
	if err := os.Remove(name); err != nil {
		return err
	}
	return cerr
}

// restorePreviousLog handles a state dir that has rpc.jsonl.1 but no rpc.jsonl:
// a rotation was interrupted after the log was moved away and before its
// successor was published (daemons before the atomic rotation could stop
// there), or the active log was removed. Creating an empty log would lose the
// managed set, so the previous generation is linked back as the active log and
// replayed as usual; the next rotation recognises the shared file and keeps a
// single copy. Only when rpc.jsonl.1 does not exist either is this a first
// start. A rpc.jsonl.1 that exists but cannot be used as a log (symlink, even a
// dangling one, directory, or one that cannot be inspected) is an error: it may
// hold the only recoverable generation, so it must not pass for absent.
func restorePreviousLog(logPath string, logger *slog.Logger) error {
	if _, err := os.Lstat(logPath); err == nil {
		// Present: Open already refused anything but a regular file.
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect rpc log %q: %w", logPath, err)
	}
	prev := logPath + ".1"
	fi, err := os.Lstat(prev)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rpc log %q is missing and its previous generation %q cannot be inspected: %w", logPath, prev, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("rpc log %q is missing and its previous generation %q is not a regular file (%s): refusing to start an empty log over it; restore it as a regular file or move it away",
			logPath, prev, fi.Mode().Type())
	}
	if err := linkPath(prev, logPath); err != nil {
		return fmt.Errorf("restore rpc log %q from %q: %w", logPath, prev, err)
	}
	logger.Warn("rpc log missing; restored the previous generation as the active log", "from", prev)
	return nil
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
		// Last chance to confirm a published rotation's directory entries.
		syncErr := s.retryDirSyncLocked()
		err := s.logFile.Close()
		s.logFile = nil
		return errors.Join(err, syncErr)
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

// RegisterManagedTab records that the daemon created targetID and
// that it was last active at `at`. Registering the same target again only
// replaces its entry.
//
// The in-memory managed set is always updated. A non-nil error means the
// lifecycle record was not written to rpc.jsonl, so the tab will not be
// restored as managed after a restart (it is then left open, never adopted
// by guesswork); callers should log it. The exception is an error for which
// RecordWritten is true: the record was written to the open log and only the
// maintenance around it failed — which still does not confirm that a restart
// reads it back (see RecordWritten).
func (s *Store) RegisterManagedTab(targetID string, at time.Time) error {
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
// should log it rather than treat the retirement as durable. An error for
// which RecordWritten is true means the record was written to the open log
// and only the maintenance around it failed (see RecordWritten).
func (s *Store) RetireManagedTarget(targetID string) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("store not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.managed[targetID]; !ok {
		return false, nil
	}
	return true, s.appendLocked(LogRecord{
		Action: ActionManagedTabRetire,
		Body:   lifecycleBody,
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
		s.managed = make(map[string]time.Time)
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
	writeErr := s.writeLinesLocked(line)
	// A directory sync owed by an earlier, already published rotation is
	// retried on its own: it neither waits for the log to fill up again nor
	// depends on what happens to this record or to the next rotation.
	syncErr := s.retryDirSyncLocked()
	if writeErr != nil {
		if syncErr != nil {
			return errors.Join(writeErr, syncErr)
		}
		return writeErr
	}
	var rotErr error
	if s.maxLogBytes > 0 && s.logBytes >= s.maxLogBytes {
		published, err := s.rotateLocked()
		switch {
		case published:
			// The rotation synced the directory itself; its outcome replaces
			// the retry above (same directory, later attempt).
			syncErr = err
		case err != nil:
			rotErr = fmt.Errorf("%w (%s still active, retried on next append): %w", ErrRotation, rpcLogFile, err)
		}
	}
	if rotErr == nil && syncErr == nil {
		return nil
	}
	// Both are kept when both are unresolved: a rotation that could not start
	// must not hide a sync that is still owed.
	return &maintenanceError{err: errors.Join(rotErr, syncErr)}
}

// retryDirSyncLocked fsyncs the state dir when a published rotation still owes
// that. Caller holds s.mu.
func (s *Store) retryDirSyncLocked() error {
	if !s.dirSyncPending {
		return nil
	}
	if err := syncDir(filepath.Dir(s.logPath)); err != nil {
		return fmt.Errorf("%w (rotated %s is active; the sync is retried by the next append and by Close): %w", ErrDirSync, rpcLogFile, err)
	}
	s.dirSyncPending = false
	return nil
}

// requireActivePathLocked checks that the name rpc.jsonl still is a regular
// file and the log this store appends to. Rotation depends on it: the previous
// generation is kept by linking that name, and startup recovers from rpc.jsonl,
// else from rpc.jsonl.1 only. With the name missing (or naming anything else)
// the backups hold the only generation a restart can find, so rotation must
// not move them. What a refusal leaves behind depends on when it is asked, so
// the caller says that (see rotateLocked). It is a check, not a lock: the name
// can still change between it and the step that relies on it. Caller holds
// s.mu.
func (s *Store) requireActivePathLocked() error {
	active, err := s.logFile.Stat()
	if err != nil {
		return fmt.Errorf("stat rpc log: %w", err)
	}
	named, err := os.Lstat(s.logPath)
	if err != nil {
		return fmt.Errorf("active rpc log path unusable: %w", err)
	}
	if !named.Mode().IsRegular() || !os.SameFile(named, active) {
		return fmt.Errorf("active rpc log path %q no longer names the open log", s.logPath)
	}
	return nil
}

// rotateLocked replaces rpc.jsonl with a snapshot of the currently managed
// tabs, so idle recovery still works after reading only the current file, and
// keeps the replaced log as rpc.jsonl.1. Caller holds s.mu.
//
// The sequence never leaves the state dir without a complete, replayable
// generation under a name startup looks at (rpc.jsonl, else rpc.jsonl.1):
//
//  1. rpc.jsonl must name the open log (requireActivePathLocked);
//  2. the snapshot is written in full to rpc.jsonl.tmp (exclusive, 0600) and
//     fsynced;
//  3. the open log becomes rpc.jsonl.1 without that name ever being free
//     (keepPreviousLocked);
//  4. rpc.jsonl is checked again — a name found removed or replaced since 1
//     is not published over — and rpc.jsonl.tmp is renamed over it, the
//     atomic publication; then the directory is fsynced. (Check and rename
//     are not one step: see the comment at the check.)
//
// published reports whether 4 renamed. Until then every failure leaves the old
// log active under its handle (appends keep working) and is returned for the
// next append past the limit to retry; a retry does not push further backups
// out. Once published the snapshot handle is the log handle — there is no
// reopen that could fail — and the only possible error is the directory sync,
// wrapped in ErrDirSync and owed alone (dirSyncPending).
func (s *Store) rotateLocked() (published bool, err error) {
	if err := s.requireActivePathLocked(); err != nil {
		return false, fmt.Errorf("rotation not started, backups left untouched: %w", err)
	}
	snapshot, err := s.managedSnapshotLocked()
	if err != nil {
		return false, err
	}
	tmpPath := s.logPath + rotateTmpSuffix
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("remove stale snapshot: %w", err)
	}
	tmp, err := openFile(tmpPath, os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_RDWR, logFilePerm)
	if err != nil {
		return false, fmt.Errorf("create snapshot: %w", err)
	}
	defer func() {
		if !published {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if len(snapshot) > 0 {
		n, err := s.rawWrite(tmp, snapshot)
		if err == nil && n < len(snapshot) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return false, fmt.Errorf("write snapshot: %w", err)
		}
	}
	if err := syncFile(tmp); err != nil {
		return false, fmt.Errorf("sync snapshot: %w", err)
	}
	if err := s.keepPreviousLocked(); err != nil {
		return false, err
	}
	// Writing the snapshot and keeping the previous generation took time: the
	// name may have gone or changed hands since the first check. The backups
	// have been shifted by now and rpc.jsonl.1 is the open log, so refusing
	// here loses nothing: appends keep reaching the file startup falls back to
	// when rpc.jsonl is absent.
	//
	// This check and the rename below are two system calls. A file put under
	// the name between them is replaced by the snapshot; nothing here can
	// close that window, and with the state dir owned by this one process
	// nothing is expected to use it.
	if err := s.requireActivePathLocked(); err != nil {
		return false, fmt.Errorf("snapshot not published, %s.1 is the open log: %w", rpcLogFile, err)
	}
	if err := renamePath(tmpPath, s.logPath); err != nil {
		return false, fmt.Errorf("publish snapshot: %w", err)
	}
	published = true

	// The switch is done in the namespace: adopt the published handle first,
	// whatever the directory sync says.
	if err := s.logFile.Close(); err != nil && s.logger != nil {
		s.logger.Warn("closing rotated rpc log failed", "err", err)
	}
	s.logFile = tmp
	s.logBytes = int64(len(snapshot))
	s.needNewline = false

	// A directory that cannot be synced only weakens the power-loss guarantee
	// (either generation is recoverable); it is reported and retried alone.
	s.dirSyncPending = true
	return true, s.retryDirSyncLocked()
}

// keepPreviousLocked makes the open log rpc.jsonl.1. The name rpc.jsonl.1 is
// what startup falls back to, so it is never free and never names anything but
// a complete generation while this runs:
//
//  1. the active log is hard linked to the private name rpc.jsonl.keep and the
//     link is checked to be the open log. Nothing has moved yet, so a link the
//     file system refuses (no space, no hard links) or a name that changed
//     hands leaves every backup where it was;
//  2. the generation currently in rpc.jsonl.1 gets the additional name
//     rpc.jsonl.2 (hard link), after the older ones moved up as far as the
//     first free slot (the oldest is replaced);
//  3. rpc.jsonl.keep is renamed over rpc.jsonl.1 — atomically replacing it.
//
// A retry after a failure at any step finds what the failed attempt left and
// discards no further generation: a rpc.jsonl.1 that already is the open log is
// reused, and a rpc.jsonl.1 that already has its second name is not shifted
// again. Caller holds s.mu.
func (s *Store) keepPreviousLocked() error {
	backup := func(k int) string { return fmt.Sprintf("%s.%d", s.logPath, k) }

	active, err := s.logFile.Stat()
	if err != nil {
		return fmt.Errorf("stat rpc log: %w", err)
	}
	prev, prevErr := os.Lstat(backup(1))
	if prevErr != nil && !errors.Is(prevErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect rpc log backup: %w", prevErr)
	}
	if prevErr == nil && os.SameFile(prev, active) {
		return nil
	}

	keepPath := s.logPath + rotateKeepSuffix
	if err := os.Remove(keepPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale rpc log link: %w", err)
	}
	if err := linkPath(s.logPath, keepPath); err != nil {
		return fmt.Errorf("keep previous rpc log: %w", err)
	}
	staged := true
	defer func() {
		if staged {
			_ = os.Remove(keepPath)
		}
	}()
	if fi, err := os.Lstat(keepPath); err != nil || !os.SameFile(fi, active) {
		return fmt.Errorf("keep previous rpc log: active rpc log path %q no longer names the open log, backups left untouched", s.logPath)
	}

	if prevErr == nil && maxLogBackups > 1 {
		if err := s.shiftBackupsLocked(prev, backup); err != nil {
			return err
		}
	}
	if err := renamePath(keepPath, backup(1)); err != nil {
		return fmt.Errorf("publish previous rpc log: %w", err)
	}
	staged = false
	return nil
}

// shiftBackupsLocked gives the generation in rpc.jsonl.1 (prev) the name
// rpc.jsonl.2 as well, moving older backups up first. rpc.jsonl.1 itself keeps
// its name; the caller replaces it atomically afterwards. Caller holds s.mu.
func (s *Store) shiftBackupsLocked(prev fs.FileInfo, backup func(int) string) error {
	if second, err := os.Lstat(backup(2)); err == nil && os.SameFile(second, prev) {
		// An earlier attempt got this far.
		return nil
	}
	free := maxLogBackups
	for k := 2; k < maxLogBackups; k++ {
		if _, err := os.Lstat(backup(k)); errors.Is(err, fs.ErrNotExist) {
			free = k
			break
		}
	}
	for k := free - 1; k >= 2; k-- {
		if err := renamePath(backup(k), backup(k+1)); err != nil {
			return fmt.Errorf("shift rpc log backup: %w", err)
		}
	}
	if !prev.Mode().IsRegular() {
		// Not a generation startup would use (it refuses such a rpc.jsonl.1),
		// and not something to hard link: move it out of the way as it is.
		if err := renamePath(backup(1), backup(2)); err != nil {
			return fmt.Errorf("shift rpc log backup: %w", err)
		}
		return nil
	}
	if maxLogBackups == 2 {
		// The only older slot is replaced; link cannot do that.
		if err := os.Remove(backup(2)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("shift rpc log backup: %w", err)
		}
	}
	if err := linkPath(backup(1), backup(2)); err != nil {
		return fmt.Errorf("shift rpc log backup: %w", err)
	}
	return nil
}

// syncDir flushes directory entries (the rotation's link and rename) to disk.
// Windows cannot sync a directory handle, so it is a no-op there.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = syncFile(d)
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// managedSnapshotLocked renders one managed_tab_open record per currently
// managed tab (sorted by target id for determinism), keeping each tab's full
// target identity and last-activity time. Retired tabs are not in s.managed and
// therefore never reseeded; an empty set yields an empty snapshot. Caller
// holds s.mu.
func (s *Store) managedSnapshotLocked() ([]byte, error) {
	targets := make([]string, 0, len(s.managed))
	for id := range s.managed {
		targets = append(targets, id)
	}
	sort.Strings(targets)

	var buf bytes.Buffer
	for _, id := range targets {
		data, err := json.Marshal(LogRecord{
			Action: ActionManagedTabOpen,
			Body:   lifecycleBody,
			Target: id,
			OK:     true,
			Time:   s.managed[id],
		})
		if err != nil {
			return nil, err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// writeLinesLocked appends p (one or more complete, newline-terminated lines)
// to the current log. It first restores the line boundary if an earlier write
// or the replayed tail left the file mid-line, and refuses to append while
// that fails. logBytes tracks the bytes actually written, and a short write
// re-arms the boundary repair for the next call. Caller holds s.mu.
func (s *Store) writeLinesLocked(p []byte) error {
	if s.needNewline {
		n, err := s.rawWrite(s.logFile, []byte{'\n'})
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
	n, err := s.rawWrite(s.logFile, p)
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

// rawWrite performs one raw write to f (the current log, or a rotation's
// snapshot file). Caller holds s.mu.
func (s *Store) rawWrite(f *os.File, p []byte) (int, error) {
	if s.writeLog != nil {
		return s.writeLog(f, p)
	}
	return f.Write(p)
}

// ManagedTab is one persisted daemon-created tab.
type ManagedTab struct {
	// TargetID is the complete CDP target id; the tab's identity.
	TargetID string
	// LastActivity is the time of its last successful tab-related RPC.
	LastActivity time.Time
}

// ManagedTabs returns the managed tabs, sorted by target id. Recovery matches
// a live tab on TargetID by exact equality.
func (s *Store) ManagedTabs() []ManagedTab {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ManagedTab, 0, len(s.managed))
	for id, at := range s.managed {
		out = append(out, ManagedTab{TargetID: id, LastActivity: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out
}

// ReplayManagedTabActivity returns the managed tabs keyed by complete CDP
// target id with their last activity (the same set as ManagedTabs).
func (s *Store) ReplayManagedTabActivity() map[string]time.Time {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.managed))
	for id, at := range s.managed {
		out[id] = at
	}
	return out
}
