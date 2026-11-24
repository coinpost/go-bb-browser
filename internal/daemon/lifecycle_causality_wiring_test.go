package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
)

// Wiring used by lifecycle_causality_test.go. It is kept apart from the
// schedules so that those can be replayed unchanged against an older tree
// whose reader wiring differs (see the red/green notes of FT28 round 7).

// causalConnect attaches a real browser.Session to the stub exactly as the
// production connection does (registry observing the reader from the first
// message, root-level discovery enabled by the connection itself) and consumes
// the announcements of the pre-existing targets.
func causalConnect(t *testing.T, ctx context.Context, s *Server, st *cdpStub) *browser.Session {
	t.Helper()
	sess := ovfConnect(t, ctx, s, st, true)
	ovfDrainConnectEvents(t, ctx, s, sess)
	return sess
}

// causalDropWatch is the registry as TargetObserver, with a barrier at the
// moment the reader reports that the destroyed notification of id was dropped
// by a full queue: what the notification said has been recorded, the overflow
// has not been published yet.
type causalDropWatch struct {
	reg    *state.TabRegistry
	id     target.ID
	last   target.ID // reader goroutine only
	fired  bool
	enter  chan struct{}
	resume chan struct{}
}

func (w *causalDropWatch) NoteEventRead(page, destroyed target.ID) uint64 {
	w.last = destroyed
	return w.reg.NoteEventRead(page, destroyed)
}

func (w *causalDropWatch) EventDropped() {
	if w.last == w.id && !w.fired {
		w.fired = true
		close(w.enter)
		<-w.resume
	}
	w.reg.EventDropped()
}

// causalConnectWatchingDrop is causalConnect with the reader parked (enter is
// closed; closing resume lets it go on) while it handles the dropped destroyed
// notification of id.
func causalConnectWatchingDrop(t *testing.T, ctx context.Context, s *Server, st *cdpStub, id target.ID) (sess *browser.Session, enter, resume chan struct{}) {
	t.Helper()
	w := &causalDropWatch{reg: s.tabs, id: id, enter: make(chan struct{}), resume: make(chan struct{})}
	sess = ovfConnectObserved(t, ctx, st, w)
	ovfDrainConnectEvents(t, ctx, s, sess)
	return sess, w.enter, w.resume
}

// causalKnown reports whether the registry still holds id at all. A target
// stops resolving the moment its destroyed notification is read; it is
// forgotten when that is applied, which is what a witness has to wait for.
func causalKnown(s *Server, id target.ID) bool { return s.tabs.Known(id) }

// When the DevTools host does not implement discovery the connection still
// works: the failure is reported, and tab removal is learned from
// enumerations as before.
func TestConnectionWithoutDiscoveryStillWorks(t *testing.T) {
	st := newCDPStub(t, ovfB)
	st.failDiscover = true
	s := newStubSessionServer(t, st)
	defer s.closeAudit()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	sess, err := browser.Connect(ctx, st.url(), browser.WithTargetObserver(s.tabs))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if sess.TargetDiscoveryErr() == nil {
		t.Fatal("the refused Target.setDiscoverTargets was not reported")
	}
	s.tabHook = sess
	ovfListed(t, s, ovfB)
	st.addPage(ovfA, true) // announced to nobody
	ovfListed(t, s, ovfA)
	st.removePage(ovfA, true)
	if ovfRowsContain(enumRPCReply{Result: enumMustRPC(t, s, "tab_list", nil)}, ovfA) || causalKnown(s, ovfA) {
		t.Error("an enumeration did not remove the closed tab")
	}
	causalNoClose(t, st)
}
