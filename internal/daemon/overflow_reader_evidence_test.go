package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/yiplee/go-bb-browser/internal/browser"
)

// ovfObserve wires the connection reader to the registry as
// connectBrowserLocked does, before any request is sent.
func ovfObserve(s *Server, sess *browser.Session) { s.observeTargetEvents(sess) }

// The discovery race in production order: the reader records the dropped
// destroyed notification in the list request in flight, so the list reply
// never registers A in the first place.
func TestDiscoveryRacesOverflowRecoveryWithReaderEvidence(t *testing.T) {
	ovfDiscoveryRace(t, true)
}

// The other side of the contradiction rule: the tab is alive. A resync whose
// snapshot predates a tab someone opened in the browser overlaps the tab_list
// that discovers it. The resync cannot prove the tab gone and the daemon
// cannot order the two snapshots, so it asks again — and the tab resolves
// after the enumeration requested after the conflict, with nothing closed.
func TestLiveTabSurvivesContradictingEnumeration(t *testing.T) {
	st := newCDPStub(t, ovfB)
	s := newStubSessionServer(t, st)
	defer s.closeAudit()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	sess, err := browser.Connect(ctx, st.url())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	s.tabHook = sess
	ovfObserve(s, sess)
	ovfListed(t, s, ovfB)
	awaitGetTargets(t, st, 1)
	resync, list := st.holdNextGetTargets(), st.holdNextGetTargets()
	defer closeOnce(resync)
	defer closeOnce(list)
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { cancel(); <-done }()

	sess.MarkTargetDirty()
	st.emitInfoChanged(ovfB)
	awaitGetTargets(t, st, 2) // resync snapshot: B only, held
	st.addPage(ovfA, false)   // opened in the browser, no notification
	listed := ovfAsyncRPC(s, "tab_list")
	awaitGetTargets(t, st, 3) // list snapshot: A + B, held
	closeOnce(list)
	if r := <-listed; r.Error != nil || !ovfRowsContain(r, ovfA) {
		t.Fatalf("precondition: tab_list introduces A: %+v", r)
	}
	closeOnce(resync)
	awaitGetTargets(t, st, 4) // requested after the conflict; lists A
	ovfAwait(t, ctx, "A registered again", func() bool { return s.tabs.Has(ovfA) })
	for _, method := range []string{"network_clear", "console_clear", "errors_clear"} {
		if r, err := enumRPC(s, method, map[string]any{"tab": string(ovfA)}); err != nil || r.Error != nil {
			t.Errorf("%s on live A: %v %v", method, r.Error, err)
		}
	}
	if !st.hasPage(ovfA) || !st.hasPage(ovfB) || len(st.closedTargets()) != 0 {
		t.Errorf("browser tabs changed: A=%v B=%v closed=%v", st.hasPage(ovfA), st.hasPage(ovfB), st.closedTargets())
	}
}

// Tab ids are opaque: a (synthetic) id made of whitespace only that tab_new
// returned is accepted back byte for byte; only the empty string is "missing".
func TestWhitespaceOnlyTabIDRoundTrips(t *testing.T) {
	c := newEnumConn(ovfB)
	c.nextIDs = append(c.nextIDs, " ")
	s := newEnumServer(t, c)
	defer s.closeAudit()
	tab := enumTabNew(t, s)
	if tab != " " {
		t.Fatalf("tab_new returned %q", tab)
	}
	for _, other := range []string{"  ", "\t", " \n"} {
		r, err := enumRPC(s, "network_clear", map[string]any{"tab": other})
		if err != nil || r.Error == nil || r.Error["message"] != "unknown tab id" {
			t.Errorf("tab %q: %v %v, want unknown tab id", other, r.Error, err)
		}
	}
	if r, err := enumRPC(s, "network_clear", map[string]any{"tab": ""}); err != nil || r.Error == nil || r.Error["message"] != "missing tab" {
		t.Errorf("empty tab: %v %v, want missing tab", r.Error, err)
	}
	for _, method := range []string{"network_clear", "tab_select", "tab_close"} {
		if r, err := enumRPC(s, method, map[string]any{"tab": tab}); err != nil || r.Error != nil {
			t.Errorf("%s with the id tab_new returned (%q): %v %v", method, tab, r.Error, err)
		}
	}
}
