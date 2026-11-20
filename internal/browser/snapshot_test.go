package browser

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every snapshot renumbers refs from 1, so marks left by the previous snapshot
// must be removed before numbering.
func TestSnapshotStubClearsStaleRefsBeforeNumbering(t *testing.T) {
	clear := strings.Index(snapshotStub, "removeAttribute('__bb_snap_ref')")
	number := strings.Index(snapshotStub, "snapshotEl(root, 0, counter)")
	if clear < 0 || number < 0 || clear > number {
		t.Fatalf("stale ref cleanup at %d, numbering at %d", clear, number)
	}
}

// A second snapshot with a narrower scope must leave each of its refs on
// exactly one node; click/fill resolve a ref to the first match in the document.
func TestSnapshotRefsAreUniqueAfterResnapshot(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the snapshot script needs a JS engine")
	}
	script := filepath.Join(t.TempDir(), "snapshot.js")
	if err := os.WriteFile(script, []byte(fmt.Sprintf(snapshotStub, "__OPTS__")), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, filepath.Join("testdata", "snapshot_refs.js"), script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 refs for the scoped snapshot, got:\n%s", out)
	}
	for _, line := range lines {
		ref, ids, _ := strings.Cut(line, " ")
		if ids == "" || strings.Contains(ids, ",") {
			t.Errorf("ref @%s is carried by nodes %q, want exactly one", ref, ids)
		}
	}
}
