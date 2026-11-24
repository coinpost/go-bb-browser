package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// observeStep scripts one tab_list / tab_focus reply. With a gate the reply is
// held in flight until the test opens it.
type observeStep struct {
	g      *gate
	result any
	err    error
}

// observeScript replies to tab_list / tab_focus calls from steps in arrival
// order; other methods echo the tab. Tests make arrival order deterministic by
// waiting for a step's gate to be entered before sending the next call. Calls
// beyond the script get the last step's reply without a gate.
type observeScript struct {
	mu    sync.Mutex
	next  int
	steps []observeStep
	newID string // tab_new result id
}

func (s *observeScript) handle(method, tab string) (any, error) {
	switch method {
	case protocol.MethodTabNew:
		return scriptedTab(s.newID), nil
	case protocol.MethodTabList, protocol.MethodTabFocus:
		s.mu.Lock()
		i := s.next
		s.next++
		s.mu.Unlock()
		if i >= len(s.steps) {
			last := s.steps[len(s.steps)-1]
			return last.result, last.err
		}
		step := s.steps[i]
		if step.g != nil {
			step.g.wait()
		}
		return step.result, step.err
	}
	return scriptedTab(tab), nil
}

// startObserve sends method (tab_list / tab_focus) in the background.
func startObserve(p *Pool, method string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- p.Call(context.Background(), method, nil, nil) }()
	return done
}

// awaitGate waits until the call behind done is held at g. If the call
// returns first (for example the request never reached the fake daemon) the
// test fails instead of blocking forever on the gate.
func awaitGate(t *testing.T, g *gate, done <-chan error) {
	t.Helper()
	select {
	case <-g.entered:
	case err := <-done:
		t.Fatalf("call returned before reaching its gate: %v", err)
	}
}

func mustDone(t *testing.T, done <-chan error) {
	t.Helper()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func poolRemovalMarks(p *Pool, idx int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.removed[idx])
}

// FT26 review P1: an older list that no longer names a tab must not block a
// newer observation (sent later, already holding the tab) from confirming it,
// whichever reply lands first. The tab is live, so calls on it must work right
// after the newer observation returns it.
func TestPool_newerObservationRestoresTabRetiredByOlderList(t *testing.T) {
	for _, newer := range []string{protocol.MethodTabFocus, protocol.MethodTabList} {
		for _, order := range []string{"older-first", "newer-first"} {
			t.Run(newer+"/"+order, func(t *testing.T) {
				gOld, gNew := newGate(), newGate()
				defer gOld.open()
				defer gNew.open()
				var newerReply any = scriptedTab("x")
				if newer == protocol.MethodTabList {
					newerReply = scriptedList("x")
				}
				script := &observeScript{newID: "x", steps: []observeStep{
					{g: gOld, result: scriptedList()}, // older snapshot: x not there
					{g: gNew, result: newerReply},     // newer snapshot: x is live
				}}
				srv, st := newScriptedDaemon(t, script.handle)
				p := newScriptedPool(t, srv)
				ctx := context.Background()
				out, err := p.TabNew(ctx, protocol.TabNewParams{})
				if err != nil {
					t.Fatal(err)
				}
				older := startObserve(p, protocol.MethodTabList)
				awaitGate(t, gOld, older)
				newerDone := startObserve(p, newer)
				awaitGate(t, gNew, newerDone)
				if order == "older-first" {
					gOld.open()
					mustDone(t, older)
					gNew.open()
					mustDone(t, newerDone)
				} else {
					gNew.open()
					mustDone(t, newerDone)
					gOld.open()
					mustDone(t, older)
				}
				assertPoolBound(t, p, out.Tab)
				if _, err := p.Eval(ctx, protocol.EvalParams{Tab: out.Tab, Script: "1"}); err != nil {
					t.Fatalf("Eval on live tab: %v", err)
				}
				if got := st.count(protocol.MethodEval); got != 1 {
					t.Fatalf("eval RPCs: got %d want 1", got)
				}
				if got := poolRemovalMarks(p, 0); got != 0 {
					t.Fatalf("removal marks left behind: %d", got)
				}
			})
		}
	}
}

// FT26 review P2: overlapping lists are ordered by when they were sent. The
// older one must not keep a tab the newer one dropped, and must not add an id
// the pool never bound once the newer list has been adopted.
func TestPool_olderListDoesNotOverrideAdoptedNewerList(t *testing.T) {
	t.Run("older names tab, newer is empty, replies in order", func(t *testing.T) {
		gOld, gNew := newGate(), newGate()
		defer gOld.open()
		defer gNew.open()
		script := &observeScript{steps: []observeStep{
			{result: scriptedList("x")},
			{g: gOld, result: scriptedList("x")},
			{g: gNew, result: scriptedList()},
		}}
		srv, _ := newScriptedDaemon(t, script.handle)
		p := newScriptedPool(t, srv)
		mustTabList(t, p)
		assertPoolBound(t, p, "0:x")
		older := startObserve(p, protocol.MethodTabList)
		awaitGate(t, gOld, older)
		newer := startObserve(p, protocol.MethodTabList)
		awaitGate(t, gNew, newer)
		gOld.open()
		mustDone(t, older)
		assertPoolBound(t, p, "0:x")
		gNew.open()
		mustDone(t, newer)
		assertPoolUnknown(t, p, "0:x")
		if got := poolBoundCount(p); got != 0 {
			t.Fatalf("bound tabs: got %d want 0", got)
		}
	})
	t.Run("newer lands first, older names a never-bound id", func(t *testing.T) {
		for _, late := range []string{protocol.MethodTabList, protocol.MethodTabFocus} {
			t.Run(late, func(t *testing.T) {
				gOld, gNew := newGate(), newGate()
				defer gOld.open()
				defer gNew.open()
				var lateReply any = scriptedList("ghost", "keep")
				if late == protocol.MethodTabFocus {
					lateReply = scriptedTab("ghost")
				}
				script := &observeScript{steps: []observeStep{
					{g: gOld, result: lateReply},
					{g: gNew, result: scriptedList("keep")},
				}}
				srv, _ := newScriptedDaemon(t, script.handle)
				p := newScriptedPool(t, srv)
				older := startObserve(p, late)
				awaitGate(t, gOld, older)
				newer := startObserve(p, protocol.MethodTabList)
				awaitGate(t, gNew, newer)
				gNew.open()
				mustDone(t, newer)
				gOld.open()
				mustDone(t, older)
				assertPoolUnknown(t, p, "0:ghost")
				assertPoolBound(t, p, "0:keep")
				if got := poolBoundCount(p); got != 1 {
					t.Fatalf("bound tabs: got %d want 1", got)
				}
			})
		}
	})
}

// An explicit ForgetTab / TabClose takes effect when it happens, not when some
// request was sent: an observation sent afterwards re-binds a tab the daemon
// still reports even though an older one is still in flight, and the older one
// changes nothing when it lands.
func TestPool_observationSentAfterForgetRebindsDespiteOlderInFlight(t *testing.T) {
	gOld := newGate()
	defer gOld.open()
	script := &observeScript{newID: "x", steps: []observeStep{
		{g: gOld, result: scriptedList("x")},
		{result: scriptedTab("x")},
	}}
	srv, _ := newScriptedDaemon(t, script.handle)
	p := newScriptedPool(t, srv)
	ctx := context.Background()
	out, err := p.TabNew(ctx, protocol.TabNewParams{})
	if err != nil {
		t.Fatal(err)
	}
	older := startObserve(p, protocol.MethodTabList)
	awaitGate(t, gOld, older)
	p.ForgetTab(out.Tab)
	assertPoolUnknown(t, p, out.Tab)
	if _, err := p.TabFocus(ctx, protocol.TabFocusParams{}); err != nil {
		t.Fatal(err)
	}
	assertPoolBound(t, p, out.Tab)
	gOld.open()
	mustDone(t, older)
	assertPoolBound(t, p, out.Tab)
	if got := poolRemovalMarks(p, 0); got != 0 {
		t.Fatalf("removal marks left behind: %d", got)
	}
}

// FT26 review P2: removal marks are reclaimed against the earliest in-flight
// observation, so polling that always overlaps (but where every request ends)
// does not accumulate them.
func TestPool_removalMarksBoundedUnderSustainedOverlap(t *testing.T) {
	const rounds = 64
	gates := make([]*gate, rounds+1)
	steps := make([]observeStep, rounds+1)
	for i := range gates {
		gates[i] = newGate()
		defer gates[i].open()
		steps[i] = observeStep{g: gates[i], result: scriptedList("keep")}
	}
	script := &observeScript{steps: steps}
	srv, _ := newScriptedDaemon(t, script.handle)
	p := newScriptedPool(t, srv)

	prev := startObserve(p, protocol.MethodTabList)
	awaitGate(t, gates[0], prev)
	for i := 1; i <= rounds; i++ {
		// One removal per round while a list is in flight …
		p.ForgetTab("0:gone" + strconv.Itoa(i))
		// … then the next list starts before the previous one ends, so the
		// backend is never idle.
		next := startObserve(p, protocol.MethodTabList)
		awaitGate(t, gates[i], next)
		gates[i-1].open()
		mustDone(t, prev)
		prev = next
		if got := poolRemovalMarks(p, 0); got > 1 {
			t.Fatalf("round %d: %d removal marks retained, want at most 1", i, got)
		}
	}
	gates[rounds].open()
	mustDone(t, prev)
	if got := poolRemovalMarks(p, 0); got != 0 {
		t.Fatalf("idle pool keeps %d removal marks", got)
	}
	assertPoolBound(t, p, "0:keep")
	if got := poolBoundCount(p); got != 1 {
		t.Fatalf("bound tabs: got %d want 1", got)
	}
}

// FT26 review P2: only an explicit tabs array is an authoritative set. A
// successful reply whose tabs is missing, null or not an array counts as a
// failed backend: nothing is retired and nothing is merged from it.
func TestPool_tabListWithoutTabsArrayIsNotAuthoritative(t *testing.T) {
	for name, bad := range map[string]string{
		"missing": `{"seq":2}`,
		"null":    `{"seq":2,"tabs":null}`,
		"string":  `{"seq":2,"tabs":"a"}`,
		"object":  `{"seq":2,"tabs":{"tab":"a"}}`,
		"number":  `{"seq":2,"tabs":0}`,
		"result":  `null`,
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			reply := any(scriptedList("a", "b"))
			bad0 := func(method, tab string) (any, error) {
				if method != protocol.MethodTabList {
					return scriptedTab(tab), nil
				}
				mu.Lock()
				defer mu.Unlock()
				return reply, nil
			}
			good1 := func(method, tab string) (any, error) {
				if method != protocol.MethodTabList {
					return scriptedTab(tab), nil
				}
				return scriptedList("c"), nil
			}
			set := func(v any) {
				mu.Lock()
				reply = v
				mu.Unlock()
			}

			// Single backend: the malformed reply fails the whole list.
			srv, _ := newScriptedDaemon(t, bad0)
			p := newScriptedPool(t, srv)
			mustTabList(t, p)
			set(json.RawMessage(bad))
			_, err := p.TabList(context.Background(), protocol.TabListParams{})
			var all *AllFailedError
			if !errors.As(err, &all) {
				t.Fatalf("want *AllFailedError, got %v", err)
			}
			assertPoolBound(t, p, "0:a")
			assertPoolBound(t, p, "0:b")

			// Two backends: the malformed one is skipped like a down backend.
			set(scriptedList("a", "b"))
			srvA, _ := newScriptedDaemon(t, bad0)
			srvB, _ := newScriptedDaemon(t, good1)
			p2 := newScriptedPool(t, srvA, srvB)
			mustTabList(t, p2)
			set(json.RawMessage(bad))
			out := mustTabList(t, p2)
			if len(out.Tabs) != 1 || out.Tabs[0].Tab != "1:c" {
				t.Fatalf("merged tabs %+v", out.Tabs)
			}
			assertPoolBound(t, p2, "0:a")
			assertPoolBound(t, p2, "0:b")
			assertPoolBound(t, p2, "1:c")

			// An explicit empty array is authoritative.
			set(scriptedList())
			mustTabList(t, p2)
			assertPoolUnknown(t, p2, "0:a")
			assertPoolUnknown(t, p2, "0:b")
			assertPoolBound(t, p2, "1:c")
		})
	}
}
