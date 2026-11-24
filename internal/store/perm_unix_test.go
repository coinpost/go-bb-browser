//go:build unix

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// rpc.jsonl keeps caller-submitted request bodies (fill text, fetch headers,
// eval scripts), so the state directory and every log file must stay private to
// the daemon user regardless of the process umask.

const (
	permHelperUmaskEnv = "BB_STORE_PERM_HELPER_UMASK"
	permHelperDirEnv   = "BB_STORE_PERM_HELPER_DIR"
	permHelperMarker   = "store-perm-helper-ran"
)

var permTabNewBody = json.RawMessage(`{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1}`)

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Errorf("stat %s: %v", filepath.Base(path), err)
		return
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", filepath.Base(path), got, want)
	}
}

// writeFileMode writes a file and forces its mode independent of the umask.
func writeFileMode(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func managedOpenLine(t *testing.T, tab string) []byte {
	t.Helper()
	data, err := json.Marshal(LogRecord{
		Action: ActionManagedTabOpen, Body: lifecycleBody, Target: "target-" + tab, OK: true, Time: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func backupPath(dir string, n int) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d", rpcLogFile, n))
}

// TestStatePermissionsArePrivateUnderUmask re-executes the test binary so each
// umask applies to an isolated process instead of leaking into parallel tests.
func TestStatePermissionsArePrivateUnderUmask(t *testing.T) {
	if os.Getenv(permHelperUmaskEnv) != "" {
		t.Skip("running inside the umask helper process")
	}
	for _, umask := range []int{0o022, 0o000, 0o077} {
		t.Run(fmt.Sprintf("umask_%03o", umask), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestStatePermissionsHelper$", "-test.v")
			cmd.Env = append(os.Environ(),
				fmt.Sprintf("%s=%o", permHelperUmaskEnv, umask),
				permHelperDirEnv+"="+t.TempDir(),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helper failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), permHelperMarker) {
				t.Fatalf("helper did not run its checks:\n%s", out)
			}
		})
	}
}

// TestStatePermissionsHelper runs only as a child of
// TestStatePermissionsArePrivateUnderUmask, with the requested umask applied.
func TestStatePermissionsHelper(t *testing.T) {
	rawUmask := os.Getenv(permHelperUmaskEnv)
	root := os.Getenv(permHelperDirEnv)
	if rawUmask == "" || root == "" {
		t.Skip("helper for TestStatePermissionsArePrivateUnderUmask")
	}
	umask, err := strconv.ParseInt(rawUmask, 8, 32)
	if err != nil {
		t.Fatalf("bad umask %q: %v", rawUmask, err)
	}
	syscall.Umask(int(umask))
	t.Log(permHelperMarker)

	t.Run("fresh state dir and log", func(t *testing.T) {
		dir := filepath.Join(root, "fresh", "state")
		s, err := Open(OpenConfig{StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		assertPerm(t, dir, 0o700)
		assertPerm(t, filepath.Join(dir, rpcLogFile), 0o600)

		if err := s.AppendRPC(LogRecord{Action: protocol.MethodTabNew, Body: permTabNewBody, OK: true}); err != nil {
			t.Fatal(err)
		}
		assertPerm(t, filepath.Join(dir, rpcLogFile), 0o600)
	})

	t.Run("existing dir log and backups are tightened", func(t *testing.T) {
		dir := filepath.Join(root, "legacy")
		mkdirMode(t, dir, 0o755)
		writeFileMode(t, filepath.Join(dir, rpcLogFile), managedOpenLine(t, "aa11"), 0o644)
		for n := 1; n <= maxLogBackups; n++ {
			writeFileMode(t, backupPath(dir, n), managedOpenLine(t, "old"+strconv.Itoa(n)), 0o644)
		}
		// Files the store does not own are left alone.
		unrelated := filepath.Join(dir, "unrelated.txt")
		writeFileMode(t, unrelated, []byte("x"), 0o644)

		s, err := Open(OpenConfig{StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		assertPerm(t, dir, 0o700)
		assertPerm(t, filepath.Join(dir, rpcLogFile), 0o600)
		for n := 1; n <= maxLogBackups; n++ {
			assertPerm(t, backupPath(dir, n), 0o600)
		}
		assertPerm(t, unrelated, 0o644)

		// Tightening must not disturb replay of the existing log.
		managed := s.ReplayManagedTabActivity()
		if _, ok := managed["target-aa11"]; !ok || len(managed) != 1 {
			t.Errorf("managed after tightening = %#v, want only aa11", managed)
		}
	})

	t.Run("stricter existing modes are not loosened", func(t *testing.T) {
		dir := filepath.Join(root, "strict")
		mkdirMode(t, dir, 0o700)
		writeFileMode(t, filepath.Join(dir, rpcLogFile), managedOpenLine(t, "aa11"), 0o600)
		writeFileMode(t, backupPath(dir, 1), managedOpenLine(t, "old1"), 0o400)

		s, err := Open(OpenConfig{StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		assertPerm(t, dir, 0o700)
		assertPerm(t, filepath.Join(dir, rpcLogFile), 0o600)
		assertPerm(t, backupPath(dir, 1), 0o400)
	})

	t.Run("rotation keeps log and backups private", func(t *testing.T) {
		dir := filepath.Join(root, "rotate")
		s, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 512})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RegisterManagedTab("target-aa11", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		gotoBody := json.RawMessage(`{"jsonrpc":"2.0","method":"goto","params":{"tab":"aa11","url":"https://example.com/some/long/path"},"id":3}`)
		// Enough rotations to fill every backup slot and drop the oldest.
		for i := 0; i < 200; i++ {
			if err := s.AppendRPC(LogRecord{Action: protocol.MethodGoto, Body: gotoBody, Target: "target-aa11", OK: true}); err != nil {
				t.Fatal(err)
			}
		}
		assertPerm(t, dir, 0o700)
		assertPerm(t, filepath.Join(dir, rpcLogFile), 0o600)
		for n := 1; n <= maxLogBackups; n++ {
			assertPerm(t, backupPath(dir, n), 0o600)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		s2, err := Open(OpenConfig{StateDir: dir, MaxLogBytes: 512})
		if err != nil {
			t.Fatal(err)
		}
		defer s2.Close()
		if _, ok := s2.ReplayManagedTabActivity()["target-aa11"]; !ok {
			t.Errorf("managed tab lost across rotation")
		}
		assertPerm(t, filepath.Join(dir, rpcLogFile), 0o600)
	})
}

// A legacy file that cannot be tightened must fail startup instead of leaving
// readable request bodies behind.
func TestOpenFailsWhenPermissionsCannotBeTightened(t *testing.T) {
	dir := t.TempDir()
	writeFileMode(t, filepath.Join(dir, rpcLogFile), managedOpenLine(t, "aa11"), 0o600)
	writeFileMode(t, backupPath(dir, 2), managedOpenLine(t, "old2"), 0o644)

	denied := errors.New("chmod denied")
	orig := chmodPath
	chmodPath = func(name string, mode os.FileMode) error {
		if name == backupPath(dir, 2) {
			return denied
		}
		return orig(name, mode)
	}
	t.Cleanup(func() { chmodPath = orig })

	s, err := Open(OpenConfig{StateDir: dir})
	if err == nil {
		_ = s.Close()
		t.Fatal("expected Open to fail when a backup cannot be made private")
	}
	if !errors.Is(err, denied) {
		t.Fatalf("error = %v, want wrapped chmod failure", err)
	}
}

func TestInMemoryModeCreatesNoFiles(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	for _, stateDir := range []string{"-", ""} {
		s, err := Open(OpenConfig{StateDir: stateDir})
		if err != nil {
			t.Fatal(err)
		}
		if !s.InMemory() {
			t.Errorf("StateDir %q: want in-memory store", stateDir)
		}
		if err := s.AppendRPC(LogRecord{Action: protocol.MethodTabNew, Body: permTabNewBody, OK: true}); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("in-memory mode created files: %v", entries)
	}
}
