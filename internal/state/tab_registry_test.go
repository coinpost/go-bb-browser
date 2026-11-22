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

// A registration is the identity of one stay of a target in the registry: it
// survives re-registration of the live target and ends with its removal, even
// when the same target id and short id come back.
func TestTabRegistrationIdentity(t *testing.T) {
	r := NewTabRegistry()
	const tid = target.ID("BBBB000000000000000000000000C0DE")
	short := r.RegisterPageTarget(tid)
	reg, ok := r.Registration(short)
	if !ok || reg.TargetID != tid || reg.ShortID != short {
		t.Fatalf("Registration(%q) = %+v, %v", short, reg, ok)
	}
	if byTarget, ok := r.RegistrationOf(tid); !ok || byTarget != reg {
		t.Fatalf("RegistrationOf = %+v, %v; want %+v", byTarget, ok, reg)
	}
	// Lifecycle events re-register a live target; that is the same registration.
	r.RegisterPageTarget(tid)
	r.SyncPageTargets([]*target.Info{{TargetID: tid, Type: "page"}})
	if !r.IsCurrent(reg) {
		t.Fatal("re-registering a live target ended its registration")
	}

	if _, removed := r.RemoveTarget(tid); !removed {
		t.Fatal("RemoveTarget")
	}
	if r.IsCurrent(reg) {
		t.Fatal("registration current after removal")
	}
	// Same full id, same short id: still another registration.
	if again := r.RegisterPageTarget(tid); again != short {
		t.Fatalf("precondition: short id = %q, want %q again", again, short)
	}
	next, _ := r.Registration(short)
	if next == reg || r.IsCurrent(reg) || !r.IsCurrent(next) {
		t.Fatalf("reused ids not told apart: old %+v, new %+v", reg, next)
	}
	if r.RemoveRegistration(reg) {
		t.Fatal("stale registration removed the target's new registration")
	}
	if _, ok := r.Lookup(short); !ok {
		t.Fatal("new registration lost")
	}

	// The short id moves to another target, the full id returns under another.
	r.RemoveTarget(tid)
	const other = target.ID("AAAB000000000000000000000000C0DE")
	if got := r.RegisterPageTarget(other); got != short {
		t.Fatalf("precondition: other target short id = %q, want %q", got, short)
	}
	moved := r.RegisterPageTarget(tid)
	if moved == short {
		t.Fatal("precondition: expected a new short id")
	}
	if r.IsCurrent(next) || r.RemoveRegistration(next) {
		t.Fatal("registration current although its short id names another target")
	}
	current, _ := r.RegistrationOf(tid)
	if current.ShortID != moved || !r.RemoveRegistration(current) || r.RemoveRegistration(current) {
		t.Fatal("RemoveRegistration must remove the current registration exactly once")
	}
	if _, ok := r.Lookup(moved); ok {
		t.Fatal("removed registration still resolvable")
	}
	if got, _ := r.Lookup(short); got != other {
		t.Fatalf("other target lost its short id: %q", got)
	}
	if r.IsCurrent(TabRegistration{}) || r.IsCurrent(TabRegistration{TargetID: other, ShortID: short}) {
		t.Fatal("a registration that was never handed out is current")
	}
}
