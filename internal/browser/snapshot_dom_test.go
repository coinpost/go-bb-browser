package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// These tests execute the production snapshot script (snapshotExpr) inside a
// JavaScript engine against testdata/fake_dom.js, then resolve refs the same
// way click / fill do (SnapshotRefSelector + querySelectorAll).

// snapshotStateProp is the document property the script keeps its bookkeeping
// in; only the bounded-tracking test looks at it.
const snapshotStateProp = "__bbSnapState"

const fixturePage = `
document.body.appendChild(h('header', {id: 'hdr'},
  h('div', {id: 'banner', text: 'Sale ends today'},
    h('button', {id: 'dismiss', text: 'Dismiss'}))));
document.body.appendChild(h('div', {id: 'danger'},
  h('button', {id: 'delete', text: 'Delete account'}),
  h('p', {id: 'note', text: 'Cannot be undone'})));
document.body.appendChild(h('form', {id: 'checkout'},
  h('input', {id: 'email', type: 'email', placeholder: 'Email'}),
  h('div', {id: 'wrap'},
    h('div', {id: 'deep'},
      h('button', {id: 'submit', text: 'Submit order'})))));
document.body.appendChild(h('div', {id: 'spacer'}));
document.body.setAttribute('id', 'body');
`

var allFixtureIDs = []string{
	"body", "hdr", "banner", "dismiss", "danger", "delete", "note",
	"checkout", "email", "wrap", "deep", "submit", "spacer",
}

type fakePage struct {
	t  *testing.T
	vm *goja.Runtime
}

func newFakePage(t *testing.T) *fakePage {
	t.Helper()
	src, err := os.ReadFile("testdata/fake_dom.js")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePage{t: t, vm: goja.New()}
	if _, err := p.vm.RunScript("fake_dom.js", string(src)); err != nil {
		t.Fatal(err)
	}
	p.run(fixturePage)
	return p
}

func (p *fakePage) run(js string) {
	p.t.Helper()
	if _, err := p.vm.RunString(js); err != nil {
		p.t.Fatalf("page script %q: %v", js, err)
	}
}

// trySnap evaluates the production expression and decodes it like SnapshotContext.
func (p *fakePage) trySnap(opts SnapshotOpts) (snapshotPayload, error) {
	p.t.Helper()
	expr, err := snapshotExpr(opts)
	if err != nil {
		p.t.Fatal(err)
	}
	v, err := p.vm.RunString("JSON.stringify(" + expr + ")")
	if err != nil {
		return snapshotPayload{}, err
	}
	var out snapshotPayload
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		p.t.Fatalf("decode snapshot: %v", err)
	}
	return out, nil
}

func (p *fakePage) snap(opts SnapshotOpts) snapshotPayload {
	p.t.Helper()
	out, err := p.trySnap(opts)
	if err != nil {
		p.t.Fatalf("snapshot: %v", err)
	}
	return out
}

type resolved struct {
	ID    string            `json:"id"`
	Tag   string            `json:"tag"`
	Label string            `json:"label"`
	Attrs map[string]string `json:"attrs"`
}

// resolve returns the elements a selector matches in the current document.
func (p *fakePage) resolve(selector string) []resolved {
	p.t.Helper()
	arg, _ := json.Marshal(selector)
	v, err := p.vm.RunString("JSON.stringify(describe(" + string(arg) + "))")
	if err != nil {
		p.t.Fatalf("resolve %s: %v", selector, err)
	}
	var out []resolved
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		p.t.Fatal(err)
	}
	return out
}

var snapshotLineRE = regexp.MustCompile(`^\s*@(\d+) \[([a-z0-9]+)[^\]]*\](?: (".*"))?$`)

// verify asserts the snapshot contract: every @N line has a refs entry equal to
// the selector click / fill use, that selector matches exactly one element in
// the document, the element is the one the line describes, and no other
// element in the document carries a ref mark. It returns element id → ref.
func (p *fakePage) verify(s snapshotPayload) map[string]string {
	p.t.Helper()
	byID := map[string]string{}
	var lines []string
	if s.Text != "" {
		parts := strings.SplitN(s.Text, "\n\n", 2)
		if len(parts) == 2 && parts[1] != "" {
			lines = strings.Split(parts[1], "\n")
		}
	}
	if len(lines) != len(s.Refs) {
		p.t.Errorf("snapshot has %d lines but %d refs", len(lines), len(s.Refs))
	}
	for _, line := range lines {
		m := snapshotLineRE.FindStringSubmatch(line)
		if m == nil {
			p.t.Errorf("unparsable snapshot line %q", line)
			continue
		}
		ref, tag := m[1], m[2]
		label := ""
		if m[3] != "" {
			if err := json.Unmarshal([]byte(m[3]), &label); err != nil {
				p.t.Errorf("line %q: bad label: %v", line, err)
			}
		}
		sel := SnapshotRefSelector(ref)
		if got, ok := s.Refs[ref]; !ok || got != sel {
			p.t.Errorf("@%s: refs entry %q (present=%v), click/fill selector %q", ref, got, ok, sel)
		}
		if alt := SnapshotRefSelector("@" + ref); alt != sel {
			p.t.Errorf("@%s: selector differs with @ prefix: %q vs %q", ref, alt, sel)
		}
		els := p.resolve(sel)
		if len(els) != 1 {
			p.t.Errorf("@%s (%s) resolves to %d elements %+v, want exactly 1", ref, strings.TrimSpace(line), len(els), els)
			continue
		}
		if els[0].Tag != tag || els[0].Label != label {
			p.t.Errorf("@%s: snapshot says [%s] %q but selector resolves to #%s [%s] %q",
				ref, tag, label, els[0].ID, els[0].Tag, els[0].Label)
		}
		if prev, dup := byID[els[0].ID]; dup {
			p.t.Errorf("element #%s has two refs @%s and @%s", els[0].ID, prev, ref)
		}
		byID[els[0].ID] = ref
	}
	if marked := p.resolve("[" + SnapshotRefAttr + "]"); len(marked) != len(s.Refs) {
		p.t.Errorf("%d elements in the document carry %s, snapshot returned %d refs: %+v",
			len(marked), SnapshotRefAttr, len(s.Refs), marked)
	}
	return byID
}

// assertDead asserts that none of the given (older) refs resolve to anything.
func (p *fakePage) assertDead(what string, refs map[string]string) {
	p.t.Helper()
	for ref, sel := range refs {
		for _, s := range []string{sel, SnapshotRefSelector(ref)} {
			if els := p.resolve(s); len(els) != 0 {
				p.t.Errorf("%s: stale @%s still resolves via %s to %+v", what, ref, s, els)
				break
			}
		}
	}
}

func assertDisjoint(t *testing.T, older, newer map[string]string) {
	t.Helper()
	for ref := range newer {
		if _, ok := older[ref]; ok {
			t.Errorf("ref @%s was reused by a later snapshot", ref)
		}
	}
}

func assertIDs(t *testing.T, got map[string]string, want []string) {
	t.Helper()
	var g []string
	for id := range got {
		g = append(g, id)
	}
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Errorf("snapshot elements = %v, want %v", g, w)
	}
}

func without(ids []string, drop ...string) []string {
	var out []string
	for _, id := range ids {
		keep := true
		for _, d := range drop {
			if id == d {
				keep = false
			}
		}
		if keep {
			out = append(out, id)
		}
	}
	return out
}

func maxRef(t *testing.T, refs map[string]string) int {
	t.Helper()
	m := 0
	for ref := range refs {
		n, err := strconv.Atoi(ref)
		if err != nil {
			t.Fatalf("ref %q is not a plain number", ref)
		}
		if n > m {
			m = n
		}
	}
	return m
}

func TestSnapshotFirstSnapshotKeepsNumericRefsFromOne(t *testing.T) {
	p := newFakePage(t)
	s := p.snap(SnapshotOpts{})
	ids := p.verify(s)
	assertIDs(t, ids, allFixtureIDs)
	if s.Title != "Fake page" || s.URL != "https://fake.test/page" {
		t.Errorf("title/url = %q / %q", s.Title, s.URL)
	}
	if !strings.HasPrefix(s.Text, "页面: Fake page\nURL: https://fake.test/page\n\n") {
		t.Errorf("unexpected header: %q", s.Text)
	}
	for i := 1; i <= len(allFixtureIDs); i++ {
		if _, ok := s.Refs[strconv.Itoa(i)]; !ok {
			t.Errorf("first snapshot of a document should number refs 1..%d, missing %d", len(allFixtureIDs), i)
		}
	}
	if sel := s.Refs["1"]; sel != `[__bb_snap_ref="1"]` {
		t.Errorf("refs[1] = %q, want the documented selector form", sel)
	}
}

// A second snapshot that does not visit everything the first one numbered must
// not leave conflicting marks behind, and the first snapshot's refs must die.
func TestSnapshotSecondSnapshotInvalidatesOldRefs(t *testing.T) {
	checkoutIDs := []string{"checkout", "email", "wrap", "deep", "submit"}
	cases := []struct {
		name   string
		mutate string
		opts   func(first snapshotPayload, firstIDs map[string]string) SnapshotOpts
		want   []string
	}{
		{
			name: "full then scope",
			opts: func(snapshotPayload, map[string]string) SnapshotOpts {
				return SnapshotOpts{SelectorScope: "#checkout"}
			},
			want: checkoutIDs,
		},
		{
			name:   "element hidden in between",
			mutate: `byId('banner').style.display = 'none';`,
			opts:   func(snapshotPayload, map[string]string) SnapshotOpts { return SnapshotOpts{} },
			want:   without(allFixtureIDs, "banner", "dismiss"),
		},
		{
			name: "pruneEmpty",
			opts: func(snapshotPayload, map[string]string) SnapshotOpts { return SnapshotOpts{PruneEmpty: true} },
			want: without(allFixtureIDs, "spacer"),
		},
		{
			name: "maxDepth",
			opts: func(snapshotPayload, map[string]string) SnapshotOpts { return SnapshotOpts{MaxDepth: 1} },
			want: []string{"body", "hdr", "danger", "checkout", "spacer"},
		},
		{
			name: "interactiveOnly",
			opts: func(snapshotPayload, map[string]string) SnapshotOpts { return SnapshotOpts{InteractiveOnly: true} },
			want: without(allFixtureIDs, "spacer", "note"),
		},
		{
			name: "scope given as a ref selector of the previous snapshot",
			opts: func(first snapshotPayload, firstIDs map[string]string) SnapshotOpts {
				return SnapshotOpts{SelectorScope: first.Refs[firstIDs["checkout"]]}
			},
			want: checkoutIDs,
		},
		{
			name:   "scope plus interactiveOnly, pruneEmpty and maxDepth after a hide",
			mutate: `byId('email').style.visibility = 'hidden';`,
			opts: func(snapshotPayload, map[string]string) SnapshotOpts {
				return SnapshotOpts{SelectorScope: "#checkout", InteractiveOnly: true, PruneEmpty: true, MaxDepth: 2}
			},
			// submit sits at depth 3, so wrap/deep have nothing interactive left.
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newFakePage(t)
			first := p.snap(SnapshotOpts{})
			firstIDs := p.verify(first)
			assertIDs(t, firstIDs, allFixtureIDs)

			if tc.mutate != "" {
				p.run(tc.mutate)
			}
			second := p.snap(tc.opts(first, firstIDs))
			secondIDs := p.verify(second)
			assertIDs(t, secondIDs, tc.want)
			p.assertDead("first snapshot", first.Refs)
			assertDisjoint(t, first.Refs, second.Refs)
		})
	}
}

// The reported incident: after full → scope, "@N = Submit order" must make
// click / fill hit the submit button, never the earlier "Delete account".
func TestSnapshotRefAfterScopeResolvesToSubmitNotDelete(t *testing.T) {
	p := newFakePage(t)
	first := p.snap(SnapshotOpts{})
	deleteRef := p.verify(first)["delete"]
	if deleteRef == "" {
		t.Fatal("delete button missing from the first snapshot")
	}

	second := p.snap(SnapshotOpts{SelectorScope: "#checkout", InteractiveOnly: true})
	submitRef := p.verify(second)["submit"]
	if submitRef == "" {
		t.Fatalf("submit button missing from the scoped snapshot:\n%s", second.Text)
	}
	if !strings.Contains(second.Text, fmt.Sprintf(`@%s [button] "Submit order"`, submitRef)) {
		t.Fatalf("snapshot text does not describe @%s as Submit order:\n%s", submitRef, second.Text)
	}
	els := p.resolve(SnapshotRefSelector("@" + submitRef))
	if len(els) != 1 || els[0].ID != "submit" {
		t.Fatalf("click @%s resolves to %+v, want only #submit", submitRef, els)
	}
	if els := p.resolve(SnapshotRefSelector(deleteRef)); len(els) != 0 {
		t.Fatalf("old @%s (Delete account) still resolves to %+v", deleteRef, els)
	}
}

// Nodes that were out of the document while a later snapshot ran must not
// bring their old refs back when they are reinserted.
func TestSnapshotDetachedThenReinsertedNodeDoesNotRestoreOldRef(t *testing.T) {
	p := newFakePage(t)
	first := p.snap(SnapshotOpts{})
	p.verify(first)

	p.run(`var parked = byId('danger'); var parkedNext = byId('checkout'); parked.remove();`)
	second := p.snap(SnapshotOpts{})
	secondIDs := p.verify(second)
	assertIDs(t, secondIDs, without(allFixtureIDs, "danger", "delete", "note"))

	p.run(`document.body.insertBefore(parked, parkedNext);`)
	p.assertDead("first snapshot after reinsertion", first.Refs)
	// The latest snapshot is still exact (labels of ancestors changed with the
	// reinsertion, so compare identities): every ref hits the same single
	// element and the reinserted subtree carries no marks.
	for id, ref := range secondIDs {
		if els := p.resolve(SnapshotRefSelector(ref)); len(els) != 1 || els[0].ID != id {
			t.Errorf("@%s should still resolve only to #%s, got %+v", ref, id, els)
		}
	}
	if marked := p.resolve("[" + SnapshotRefAttr + "]"); len(marked) != len(second.Refs) {
		t.Errorf("%d marked elements after reinsertion, want %d: %+v", len(marked), len(second.Refs), marked)
	}

	third := p.snap(SnapshotOpts{})
	assertIDs(t, p.verify(third), allFixtureIDs)
	p.assertDead("first snapshot", first.Refs)
	p.assertDead("second snapshot", second.Refs)
	assertDisjoint(t, first.Refs, third.Refs)
	assertDisjoint(t, second.Refs, third.Refs)
}

func TestSnapshotRefsAreNeverReusedWithinADocument(t *testing.T) {
	p := newFakePage(t)
	seq := []SnapshotOpts{
		{},
		{SelectorScope: "#checkout"},
		{InteractiveOnly: true},
		{PruneEmpty: true, MaxDepth: 2},
		{SelectorScope: "#danger"},
		{},
	}
	var history []map[string]string
	high := 0
	for i, opts := range seq {
		s := p.snap(opts)
		p.verify(s)
		if len(s.Refs) == 0 {
			t.Fatalf("snapshot %d unexpectedly empty", i)
		}
		for ref := range s.Refs {
			n, err := strconv.Atoi(ref)
			if err != nil || n <= high {
				t.Errorf("snapshot %d: ref %q is not above every earlier ref (max %d)", i, ref, high)
			}
		}
		for j, old := range history {
			p.assertDead(fmt.Sprintf("snapshot %d after snapshot %d", j, i), old)
		}
		high = maxRef(t, s.Refs)
		history = append(history, s.Refs)
	}
}

// Snapshots that fail or come back empty must leave a well-defined state:
// an invalid scope selector is an error that changes nothing; a snapshot that
// returns a (possibly empty) result always invalidates every earlier ref.
func TestSnapshotFailedAndEmptySnapshots(t *testing.T) {
	t.Run("invalid scope selector errors and keeps the previous snapshot usable", func(t *testing.T) {
		p := newFakePage(t)
		first := p.snap(SnapshotOpts{})
		p.verify(first)
		if _, err := p.trySnap(SnapshotOpts{SelectorScope: "div >>> ["}); err == nil {
			t.Fatal("invalid selector should fail the snapshot")
		}
		assertIDs(t, p.verify(first), allFixtureIDs)
	})

	empties := []struct {
		name     string
		mutate   string
		opts     SnapshotOpts
		wantText bool // header-only text instead of ""
	}{
		{name: "scope matches nothing", opts: SnapshotOpts{SelectorScope: "#nope"}},
		{name: "no body", mutate: `var oldBody = document.body; oldBody.remove();`, opts: SnapshotOpts{}},
		{
			name:     "scope root not visible",
			mutate:   `byId('checkout').style.display = 'none';`,
			opts:     SnapshotOpts{SelectorScope: "#checkout"},
			wantText: true,
		},
		{
			name:     "everything filtered out",
			opts:     SnapshotOpts{SelectorScope: "#spacer", PruneEmpty: true},
			wantText: true,
		},
	}
	for _, tc := range empties {
		t.Run(tc.name, func(t *testing.T) {
			p := newFakePage(t)
			first := p.snap(SnapshotOpts{})
			p.verify(first)
			if tc.mutate != "" {
				p.run(tc.mutate)
			}
			empty := p.snap(tc.opts)
			if len(empty.Refs) != 0 {
				t.Fatalf("expected no refs, got %v", empty.Refs)
			}
			wantText := ""
			if tc.wantText {
				wantText = "页面: Fake page\nURL: https://fake.test/page\n\n"
			}
			if empty.Text != wantText {
				t.Errorf("text = %q, want %q", empty.Text, wantText)
			}
			if empty.Title != "Fake page" || empty.URL != "https://fake.test/page" {
				t.Errorf("title/url = %q / %q", empty.Title, empty.URL)
			}
			p.verify(empty)
			p.assertDead("first snapshot", first.Refs)

			// Undo the mutation: old marks must not come back with the nodes.
			p.run(`if (typeof oldBody !== 'undefined') document.documentElement.appendChild(oldBody);
byId('checkout').style.display = '';`)
			p.assertDead("first snapshot after restore", first.Refs)
			next := p.snap(SnapshotOpts{})
			assertIDs(t, p.verify(next), allFixtureIDs)
			assertDisjoint(t, first.Refs, next.Refs)
		})
	}
}

// Marks the script has no bookkeeping for (left by an older daemon build, or
// copied by the page) are removed and their numbers are not handed out again.
func TestSnapshotClearsMarksItDidNotTrack(t *testing.T) {
	p := newFakePage(t)
	p.run(`byId('delete').setAttribute('__bb_snap_ref', '1');
byId('dismiss').setAttribute('__bb_snap_ref', '40');
byId('note').setAttribute('__bb_snap_ref', 'bogus');`)
	s := p.snap(SnapshotOpts{SelectorScope: "#checkout"})
	ids := p.verify(s)
	assertIDs(t, ids, []string{"checkout", "email", "wrap", "deep", "submit"})
	for ref := range s.Refs {
		if n, _ := strconv.Atoi(ref); n <= 40 {
			t.Errorf("ref @%s collides with the range of pre-existing marks (<= 40)", ref)
		}
	}
	p.assertDead("pre-existing marks", map[string]string{"1": SnapshotRefSelector("1"), "40": SnapshotRefSelector("40")})
}

// Bookkeeping is bounded by the latest snapshot: churned DOM from earlier
// snapshots is not retained.
func TestSnapshotTrackingIsBoundedByLatestSnapshot(t *testing.T) {
	p := newFakePage(t)
	var last snapshotPayload
	for i := 0; i < 40; i++ {
		p.run(fmt.Sprintf(`byId('danger').remove();
document.body.appendChild(h('div', {id: 'danger'},
  h('button', {id: 'delete', text: 'Delete account %d'}),
  h('p', {id: 'note', text: 'Cannot be undone'})));`, i))
		last = p.snap(SnapshotOpts{})
	}
	assertIDs(t, p.verify(last), allFixtureIDs)
	v, err := p.vm.RunString(`(function(){
  var st = document.` + snapshotStateProp + `;
  if (!st) return -1;
  var n = 0;
  for (var k in st) { if (st[k] && typeof st[k] === 'object' && typeof st[k].length === 'number') n += st[k].length; }
  return n;
})()`)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(v.ToInteger()); got != len(last.Refs) {
		t.Errorf("script tracks %d nodes after 40 snapshots, want exactly the %d of the latest snapshot", got, len(last.Refs))
	}
	if keys, _ := p.vm.RunString(`Object.keys(document).indexOf('` + snapshotStateProp + `')`); keys.ToInteger() != -1 {
		t.Errorf("%s should not be an enumerable document property", snapshotStateProp)
	}
}
