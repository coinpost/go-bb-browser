package state

import (
	"testing"

	"github.com/chromedp/cdproto/target"
)

func TestShortIDFromSuffix(t *testing.T) {
	r := NewTabRegistry()
	infos := []*target.Info{
		{TargetID: "ABCDEF123456", Type: "page", Title: "a", URL: "https://a"},
	}
	snap := r.SyncPageTargets(infos)
	if len(snap) != 1 {
		t.Fatalf("snap len %d", len(snap))
	}
	// Last 4 hex chars of id: ...3456
	if snap[0].ShortID != "3456" {
		t.Fatalf("short id %q want 3456", snap[0].ShortID)
	}
}

func TestShortIDCollisionLengthens(t *testing.T) {
	r := NewTabRegistry()
	// Two ids that share the same last 4 hex digits
	infos := []*target.Info{
		{TargetID: "1111AAAA2222BBBB", Type: "page", Title: "1", URL: ""},
		{TargetID: "9999CCCC2222BBBB", Type: "page", Title: "2", URL: ""},
	}
	snap := r.SyncPageTargets(infos)
	if len(snap) != 2 {
		t.Fatalf("snap len %d", len(snap))
	}
	shorts := map[string]struct{}{}
	for _, s := range snap {
		shorts[s.ShortID] = struct{}{}
	}
	if len(shorts) != 2 {
		t.Fatalf("expected distinct short ids, got %#v", snap)
	}
}

func TestLookupInvalidTab(t *testing.T) {
	r := NewTabRegistry()
	_, ok := r.Lookup("nope")
	if ok {
		t.Fatal("expected miss")
	}
}

func TestSelectUnknownTab(t *testing.T) {
	r := NewTabRegistry()
	if r.Select("abcd") {
		t.Fatal("select should fail for unknown tab")
	}
}

func TestShortIDCollisionUsesMoreDigits(t *testing.T) {
	r := NewTabRegistry()
	infos := []*target.Info{
		{TargetID: "AAA11115678", Type: "page"},
		{TargetID: "BBB22225678", Type: "page"},
	}
	snap := r.SyncPageTargets(infos)
	if len(snap) != 2 {
		t.Fatalf("snap len %d", len(snap))
	}
	shorts := map[string]struct{}{}
	for _, s := range snap {
		shorts[s.ShortID] = struct{}{}
	}
	if len(shorts) != 2 {
		t.Fatalf("want distinct shorts, got %#v", snap)
	}
}

func TestSyncPageTargetsSinceKeepsTargetsRegisteredAfterEpoch(t *testing.T) {
	r := NewTabRegistry()
	r.RegisterPageTarget("AAAA1111")
	epoch := r.Epoch() // enumeration requested here
	r.RegisterPageTarget("BBBB2222")

	// The reply lists neither target: the old one is gone, the new one was
	// created after the browser answered.
	tabs, removed, kept := r.SyncPageTargetsSince(nil, epoch)
	if len(tabs) != 1 || tabs[0].TargetID != "BBBB2222" || tabs[0].ShortID != "2222" {
		t.Fatalf("tabs = %#v, want the kept target", tabs)
	}
	if len(removed) != 1 || removed[0].TargetID != "AAAA1111" {
		t.Fatalf("removed = %#v", removed)
	}
	if len(kept) != 1 || kept[0] != "BBBB2222" {
		t.Fatalf("kept = %#v", kept)
	}
	if _, ok := r.Lookup("2222"); !ok {
		t.Fatal("target registered after the epoch was removed")
	}

	// An enumeration requested after the registration is authoritative.
	_, removed, kept = r.SyncPageTargetsSince(nil, r.Epoch())
	if len(removed) != 1 || removed[0].TargetID != "BBBB2222" || len(kept) != 0 {
		t.Fatalf("removed = %#v kept = %#v", removed, kept)
	}
	if _, removed := r.SyncPageTargetsDetailed(nil); len(removed) != 0 {
		t.Fatalf("registry not empty: %#v", removed)
	}
}

func TestCouldHaveShort(t *testing.T) {
	for _, tc := range []struct {
		id    target.ID
		short string
		want  bool
	}{
		{"AAAA00001234", "1234", true},
		{"AAAA00001234", "01234", true},
		{"AAAA00001234", "aaaa00001234", true},
		{"AAAA00001234", "234", false},  // shorter than any assigned short id
		{"AAAA00001234", "9234", false}, // not a suffix
		{"AAAA00001234", "", false},
		{"", "1234", false},
		{"xyz-!", "xyz-!", true}, // no hex digits: the id itself
		{"xyz-!", "xyz", false},
		{"no-hex-!!", "e", true}, // fewer than four hex digits: all of them
	} {
		if got := CouldHaveShort(tc.id, tc.short); got != tc.want {
			t.Errorf("CouldHaveShort(%q, %q) = %v, want %v", tc.id, tc.short, got, tc.want)
		}
	}
	// Whatever the registry assigns is something CouldHaveShort accepts.
	r := NewTabRegistry()
	for _, id := range []target.ID{"AAAA00001234", "BBBB99991234", "CCCC99991234", "no-hex-!!"} {
		if short := r.RegisterPageTarget(id); !CouldHaveShort(id, short) {
			t.Errorf("registry gave %q the short id %q, which CouldHaveShort rejects", id, short)
		}
	}
}
