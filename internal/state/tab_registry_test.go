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
	if r.SelectAlias("abcd", "AAAAabcd") || r.SelectAlias("", "") {
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

// SelectTarget selects by identity: a target that is gone is not selectable,
// even when another target now holds the short id it had.
func TestSelectTargetDoesNotFollowShortIDReuse(t *testing.T) {
	r := NewTabRegistry()
	const first, second = target.ID("BBBB99991234"), target.ID("AAAA00001234")
	if short := r.RegisterPageTarget(first); short != "1234" {
		t.Fatalf("short = %q", short)
	}
	if short, ok := r.SelectTarget(first); !ok || short != "1234" || r.Selected() != "1234" {
		t.Fatalf("SelectTarget = %q %v, selected %q", short, ok, r.Selected())
	}
	r.RemoveTarget(first)
	if short := r.RegisterPageTarget(second); short != "1234" {
		t.Fatalf("short = %q", short)
	}
	if _, ok := r.SelectTarget(first); ok || r.Selected() != "" {
		t.Fatalf("removed target selected; selected = %q", r.Selected())
	}
}
