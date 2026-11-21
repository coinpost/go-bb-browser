package state

import (
	"testing"

	"github.com/chromedp/cdproto/target"
)

// Alias contract under test (see TabRegistry):
//   - the short id of a target is a pure function of the set of live targets,
//     never of the order in which they were registered;
//   - an alias resolves only while exactly one live target claims it, so a
//     collision turns the shared alias into a hard error instead of handing it
//     to another target;
//   - without a collision the short id is still the last 4 hex digits.

const (
	aliasA = target.ID("FFFF000000000000000000000000ABCD") // created first at run time
	aliasB = target.ID("0000111111111111111111111111ABCD") // created later, sorts first
)

func shortsOf(t *testing.T, r *TabRegistry, ids ...target.ID) map[target.ID]string {
	t.Helper()
	out := make(map[target.ID]string, len(ids))
	for _, id := range ids {
		short, ok := r.ShortForTarget(id)
		if !ok {
			t.Fatalf("target %s not registered", id)
		}
		out[id] = short
	}
	return out
}

func permutations(ids []target.ID) [][]target.ID {
	if len(ids) <= 1 {
		return [][]target.ID{append([]target.ID(nil), ids...)}
	}
	var out [][]target.ID
	for i := range ids {
		rest := make([]target.ID, 0, len(ids)-1)
		rest = append(rest, ids[:i]...)
		rest = append(rest, ids[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]target.ID{ids[i]}, p...))
		}
	}
	return out
}

func TestShortIDsIndependentOfRegistrationOrder(t *testing.T) {
	runtime := NewTabRegistry() // lifecycle events / tab_new order
	runtime.RegisterPageTarget(aliasA)
	runtime.RegisterPageTarget(aliasB)

	reversed := NewTabRegistry()
	reversed.RegisterPageTarget(aliasB)
	reversed.RegisterPageTarget(aliasA)

	restarted := NewTabRegistry() // restart: one sorted enumeration
	restarted.SyncPageTargets(pageInfos(aliasA, aliasB))

	want := map[target.ID]string{aliasA: "0abcd", aliasB: "1abcd"}
	for name, r := range map[string]*TabRegistry{"runtime": runtime, "reversed": reversed, "restarted": restarted} {
		got := shortsOf(t, r, aliasA, aliasB)
		for id, short := range want {
			if got[id] != short {
				t.Errorf("%s: short id of %s = %q, want %q", name, id, got[id], short)
			}
			if tid, ok := r.Lookup(short); !ok || tid != id {
				t.Errorf("%s: Lookup(%q) = %q, %v; want %s", name, short, tid, ok, id)
			}
		}
	}
}

// The 4-digit alias held by clients (and written to rpc.jsonl) before the
// collision may only ever resolve to its original target or be rejected.
func TestCollidingShortAliasIsRejectedNotReassigned(t *testing.T) {
	r := NewTabRegistry()
	if short := r.RegisterPageTarget(aliasA); short != "abcd" {
		t.Fatalf("short id without collision = %q, want 4 digits abcd", short)
	}
	if tid, ok := r.Lookup("abcd"); !ok || tid != aliasA {
		t.Fatalf("Lookup(abcd) before collision = %q, %v", tid, ok)
	}

	r.RegisterPageTarget(aliasB)
	if tid, ok := r.Lookup("abcd"); ok {
		t.Fatalf("ambiguous alias abcd resolved to %s; want hard miss", tid)
	}
	if r.Select("abcd") {
		t.Fatal("Select accepted an ambiguous alias")
	}

	// A registry rebuilt by a restart must not resolve the old alias to B.
	rebuilt := NewTabRegistry()
	rebuilt.SyncPageTargets(pageInfos(aliasA, aliasB))
	if tid, ok := rebuilt.Lookup("abcd"); ok && tid != aliasA {
		t.Fatalf("after rebuild alias abcd (tab A before restart) resolves to %s", tid)
	}

	// Once B is gone the set is collision-free again: 4 digits come back and
	// the longer alias handed out during the collision keeps resolving to A.
	if short, ok := r.RemoveTarget(aliasB); !ok || short != "1abcd" {
		t.Fatalf("RemoveTarget(B) = %q, %v; want 1abcd", short, ok)
	}
	if short, _ := r.ShortForTarget(aliasA); short != "abcd" {
		t.Fatalf("short id of A after collision cleared = %q, want abcd", short)
	}
	for _, alias := range []string{"abcd", "0abcd", "ffff000000000000000000000000abcd"} {
		if tid, ok := r.Lookup(alias); !ok || tid != aliasA {
			t.Fatalf("Lookup(%q) = %q, %v; want A", alias, tid, ok)
		}
	}
	if _, ok := r.Lookup("1abcd"); ok {
		t.Fatal("alias of the removed target still resolves")
	}
}

func TestShortIDsOrderIndependentForNestedCollisions(t *testing.T) {
	ids := []target.ID{
		"AAAA00000000000011112222", // shares 8 digits with the next one
		"BBBB00000000000011112222",
		"CCCC00000000000099992222", // shares only 4 digits
		"DDDD00000000000000003333", // no collision
	}
	want := map[target.ID]string{
		ids[0]: "a00000000000011112222",
		ids[1]: "b00000000000011112222",
		ids[2]: "92222",
		ids[3]: "3333",
	}
	check := func(name string, r *TabRegistry) {
		t.Helper()
		got := shortsOf(t, r, ids...)
		for id, short := range want {
			if got[id] != short {
				t.Errorf("%s: short id of %s = %q, want %q", name, id, got[id], short)
			}
			if tid, ok := r.Lookup(short); !ok || tid != id {
				t.Errorf("%s: Lookup(%q) = %q, %v; want %s", name, short, tid, ok, id)
			}
		}
		for _, ambiguous := range []string{"2222", "12222", "00000000000011112222"} {
			if tid, ok := r.Lookup(ambiguous); ok {
				t.Errorf("%s: ambiguous alias %q resolved to %s", name, ambiguous, tid)
			}
		}
	}
	for _, order := range permutations(ids) {
		r := NewTabRegistry()
		for _, id := range order {
			r.RegisterPageTarget(id)
		}
		check("register", r)
	}
	synced := NewTabRegistry()
	snaps := synced.SyncPageTargets(pageInfos(ids...))
	check("sync", synced)
	for _, sn := range snaps {
		if sn.ShortID != want[sn.TargetID] {
			t.Errorf("snapshot short id of %s = %q, want %q", sn.TargetID, sn.ShortID, want[sn.TargetID])
		}
	}
}

// A later collision upgrades the alias of an already registered target; the
// selection must follow the target, not the alias string.
func TestSelectionFollowsTargetAcrossAliasChange(t *testing.T) {
	r := NewTabRegistry()
	if !r.Select(r.RegisterPageTarget(aliasA)) {
		t.Fatal("select A")
	}
	r.RegisterPageTarget(aliasB)
	focus := r.Selected()
	if tid, ok := r.Lookup(focus); !ok || tid != aliasA {
		t.Fatalf("Selected() = %q resolves to %q, %v; want A", focus, tid, ok)
	}
	r.RemoveTarget(aliasA)
	if focus := r.Selected(); focus != "" {
		t.Fatalf("Selected() after removing the selected target = %q, want empty", focus)
	}
}

// Closing and re-registering yields the alias the live set dictates, and a
// removal reported by a full sync carries the alias valid at removal time.
func TestAliasAfterRemovalAndReregistration(t *testing.T) {
	r := NewTabRegistry()
	r.SyncPageTargets(pageInfos(aliasA, aliasB))
	_, removed := r.SyncPageTargetsDetailed(pageInfos(aliasB))
	if len(removed) != 1 || removed[0].TargetID != aliasA || removed[0].ShortID != "0abcd" {
		t.Fatalf("removed = %#v, want A with alias 0abcd", removed)
	}
	if short, _ := r.ShortForTarget(aliasB); short != "abcd" {
		t.Fatalf("short id of B alone = %q, want abcd", short)
	}
	if short := r.RegisterPageTarget(aliasA); short != "0abcd" {
		t.Fatalf("short id of re-registered A = %q, want 0abcd", short)
	}
	if short, _ := r.ShortForTarget(aliasB); short != "1abcd" {
		t.Fatalf("short id of B after A returned = %q, want 1abcd", short)
	}
}

// Target ids with fewer than 4 hex digits, or whose hex digits are identical,
// cannot be told apart by a hex suffix. They must still get distinct aliases
// and must never overwrite each other's mapping.
func TestDegenerateTargetIDsNeverShareAnAlias(t *testing.T) {
	groups := [][]target.ID{
		{"tgt-1", "x1"},            // one hex digit each, the same one
		{"zz", "yy"},               // no hex digit at all
		{"ABCD1234", "abcd1234"},   // identical hex digits, different ids
		{"AB-CD-99", "ABCD99", ""}, // short, identical hex digits, plus an empty id
		{"t-zz", "zz", "t-t-zz"},   // ids that look like each other's fallbacks
		{"id=zz", "zz"},
	}
	for _, ids := range groups {
		for _, order := range permutations(ids) {
			r := NewTabRegistry()
			for _, id := range order {
				r.RegisterPageTarget(id)
			}
			seen := map[string]target.ID{}
			for _, id := range ids {
				short, ok := r.ShortForTarget(id)
				if id == "" {
					continue
				}
				if !ok || short == "" {
					t.Fatalf("%q in %q has no alias", id, ids)
				}
				if other, dup := seen[short]; dup {
					t.Fatalf("%q and %q share alias %q", other, id, short)
				}
				seen[short] = id
				if tid, ok := r.Lookup(short); !ok || tid != id {
					t.Fatalf("Lookup(%q) = %q, %v; want %q (order %q)", short, tid, ok, id, order)
				}
			}
		}
	}

	r := NewTabRegistry()
	r.RegisterPageTarget("tgt-1")
	r.RegisterPageTarget("x1")
	if tid, ok := r.Lookup("1"); ok {
		t.Fatalf("shared alias 1 resolved to %q", tid)
	}
}
