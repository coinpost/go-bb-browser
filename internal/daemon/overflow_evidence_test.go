package daemon

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/store"
)

// Regressions for lifecycle evidence lost to a full event queue. Everything
// here runs the real chromedp connection reader and the production sync loop
// (runTargetSync) against the loopback CDP stub; the schedule is fixed by the
// queue capacity, WebSocket pings (everything sent before a ping's reply has
// been read), held Target.getTargets replies, and the destruction of an
// unrelated marker tab that proves a reply was applied.
//
// A is opened once and really removed from the stub browser; B shares A's
// suffix and must stay untouched.

const (
	ovfA      target.ID = "AAAA0000000000000000000000AB1234"
	ovfB      target.ID = "BBBB0000000000000000000000AB1234"
	ovfMarker target.ID = "CCCC0000000000000000000000ABCDEF"
)

func ovfAsyncRPC(s *Server, method string) <-chan enumRPCReply {
	done := make(chan enumRPCReply, 1)
	go func() {
		r, err := enumRPC(s, method, nil)
		if err != nil {
			r.Error = map[string]any{"message": err.Error()}
		}
		done <- r
	}()
	return done
}

// ovfListed returns id as tab_list reports it.
func ovfListed(t *testing.T, s *Server, id target.ID) string {
	t.Helper()
	for _, row := range enumMustRPC(t, s, "tab_list", nil)["tabs"].([]any) {
		if tab := row.(map[string]any)["tab"].(string); tab == string(id) {
			return tab
		}
	}
	t.Fatalf("listed target missing: %s", id)
	return ""
}

func ovfRowsContain(r enumRPCReply, id target.ID) bool {
	rows, _ := r.Result["tabs"].([]any)
	for _, row := range rows {
		if row.(map[string]any)["tab"] == string(id) {
			return true
		}
	}
	return false
}

// ovfClearsRefuse asserts that the three observation clears, the cheapest
// methods that resolve a tab id, reject tab as unknown. Resolving an
// unregistered id asks the browser once (Target.getTargets), so the caller
// must not leave a held gate for that request: a clear that fails for any
// other reason (such as the operation deadline) is not a refusal.
func ovfClearsRefuse(t *testing.T, s *Server, tab string) {
	t.Helper()
	for _, method := range []string{"network_clear", "console_clear", "errors_clear"} {
		r, err := enumRPC(s, method, map[string]any{"tab": tab})
		if err != nil {
			t.Fatal(err)
		}
		if r.Error == nil {
			t.Errorf("%s accepted the id of a destroyed tab %q", method, tab)
		} else if r.Error["message"] != "unknown tab id" {
			t.Errorf("%s on destroyed tab %q failed with %v, want unknown tab id", method, tab, r.Error)
		}
	}
}

func ovfAwait(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		if ctx.Err() != nil {
			t.Fatalf("%s: %v", what, ctx.Err())
		}
		runtime.Gosched()
	}
}

// ovfFillQueue leaves the lifecycle queue full of events about B.
func ovfFillQueue(t *testing.T, ctx context.Context, st *cdpStub, sess *browser.Session) {
	t.Helper()
	for range cap(sess.TargetEvents()) {
		st.emitInfoChanged(ovfB)
	}
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sess.TargetEvents()) != cap(sess.TargetEvents()) {
		t.Fatal("lifecycle queue is not full")
	}
}

// ovfDestroyInBrowser closes id the way a user or the browser would: no daemon
// tab_close, so the registry learns nothing from a command reply; the stub then
// sends the destroyed notification, which has been read when this returns.
func ovfDestroyInBrowser(t *testing.T, ctx context.Context, st *cdpStub, sess *browser.Session, id target.ID) {
	t.Helper()
	if err := sess.CloseTargetContext(ctx, id); err != nil {
		t.Fatal(err)
	}
	st.emit("Target.targetDestroyed", map[string]any{"targetId": id})
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	if st.hasPage(id) {
		t.Fatalf("%s still open in the browser", id)
	}
}

// ovfHeldCreate delays only the delivery of a successful Target.createTarget
// reply to the tab_new handler; the round trip itself is the real one.
type ovfHeldCreate struct {
	*browser.Session
	made    chan target.ID
	release chan struct{}
}

func (c *ovfHeldCreate) CreatePageTargetContext(ctx context.Context, url string, silent bool) (target.ID, error) {
	id, err := c.Session.CreatePageTargetContext(ctx, url, silent)
	if err != nil {
		return "", err
	}
	c.made <- id
	select {
	case <-c.release:
		return id, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// The queue is full while tab_new(A) is in flight: A's created and destroyed
// notifications are both dropped and the overflow resync lists only B. A was
// never registered, so no removal was ever applied for it; the late creation
// reply must still not commit a tab the browser has destroyed — not in the
// registry, not in idle tracking, not in the persisted managed set.
func TestDroppedDestroyDuringCreate(t *testing.T) {
	dir := t.TempDir()
	st := newCDPStub(t, ovfB, ovfMarker)
	s := newReconcileRaceServer(t, newEnumConn(), dir, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	sess := ovfConnect(t, ctx, s, st, true)
	ovfDrainConnectEvents(t, ctx, s, sess)
	c := &ovfHeldCreate{Session: sess, made: make(chan target.ID, 1), release: make(chan struct{})}
	defer closeOnce(c.release)
	s.tabHook = c
	s.cfg.CDPWatchdogTimeout = 10 * time.Second
	marker := ovfListed(t, s, ovfMarker)
	awaitGetTargets(t, st, 1)
	first, second := st.holdNextGetTargets(), st.holdNextGetTargets()
	defer closeOnce(first)
	defer closeOnce(second)
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { cancel(); <-done }()

	// Park the queue consumer in a resync (snapshot: B + marker), then fill the
	// queue behind it.
	sess.MarkTargetDirty()
	st.emitInfoChanged(ovfB)
	awaitGetTargets(t, st, 2)
	ovfFillQueue(t, ctx, st, sess)

	st.queueCreateID(ovfA)
	created := ovfAsyncRPC(s, "tab_new")
	select {
	case id := <-c.made:
		if id != ovfA {
			t.Fatalf("created %q, want %q", id, ovfA)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	st.emit("Target.targetCreated", map[string]any{"targetInfo": stubTargetInfo(ovfA)})
	ovfDestroyInBrowser(t, ctx, st, sess, ovfA)
	if len(sess.TargetEvents()) != cap(sess.TargetEvents()) {
		t.Fatal("precondition: both notifications about A must have been dropped")
	}

	closeOnce(first)
	awaitGetTargets(t, st, 3) // the overflow resync; its snapshot excludes A
	closeOnce(second)
	// Witness that resync #3 was applied: wait for the queue to drain, then
	// destroy the marker; its removal is handled after everything before it.
	ovfAwait(t, ctx, "queue drained", func() bool { return len(sess.TargetEvents()) < cap(sess.TargetEvents()) })
	if err := sess.CloseTargetContext(ctx, ovfMarker); err != nil {
		t.Fatal(err)
	}
	st.emit("Target.targetDestroyed", map[string]any{"targetId": ovfMarker})
	// Known, not Lookup: the marker stops resolving the moment its notification
	// is read; it is forgotten when the consumer gets to it.
	ovfAwait(t, ctx, "marker removed", func() bool { return !s.tabs.Known(target.ID(marker)) })

	closeOnce(c.release)
	r := <-created
	if !st.hasPage(ovfB) {
		t.Fatal("B lost")
	}
	if r.Error == nil {
		id, _ := r.Result["tab"].(string)
		t.Errorf("tab_new succeeded for a tab destroyed before its reply was delivered: tab=%q", id)
		ovfClearsRefuse(t, s, id)
	} else if msg, _ := r.Error["message"].(string); !strings.Contains(msg, "tab closed before tab_new completed") {
		t.Errorf("tab_new error = %v, want the creation-refused error", r.Error)
	}
	ovfClearsRefuse(t, s, string(ovfA))
	if s.tabs.Has(ovfA) || s.tabs.Selected() == ovfA {
		t.Error("destroyed A is registered")
	}
	if _, ok := s.tabs.Lookup(string(ovfB)); !ok {
		t.Error("B no longer resolves")
	}
	_, idle := s.tabIdle.Snapshot()[ovfA]
	if err := s.closeAudit(); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if managed := managedTargetSet(replay.ManagedTabs())[ovfA]; idle || managed {
		t.Errorf("destroyed A kept as a managed tab: idle tracking=%v, replayed rpc.jsonl=%v", idle, managed)
	}
}

// ovfDiscoveryRace runs the schedule without any tab_new: an ordinary tab_list
// snapshots A (opened in the browser by someone else), A is destroyed with the
// queue full so the notification is dropped, and the overflow resync snapshots
// B only. The list reply is applied first, the resync second.
//
// readerEvidence selects when the connection reader is wired to the registry:
// true is the production order (connectBrowserLocked: before any request);
// false leaves the destroyed notification unobserved, as the review's
// diagnostic did, so only the two contradicting snapshots are left to decide.
func ovfDiscoveryRace(t *testing.T, readerEvidence bool) {
	st := newCDPStub(t, ovfB, ovfMarker)
	s := newStubSessionServer(t, st)
	defer s.closeAudit()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	sess := ovfConnect(t, ctx, s, st, readerEvidence)
	s.tabHook = sess
	ovfDrainConnectEvents(t, ctx, s, sess)
	marker := ovfListed(t, s, ovfMarker)
	awaitGetTargets(t, st, 1)
	// The queue consumer is not running yet: fill the queue, then open A.
	ovfFillQueue(t, ctx, st, sess)
	st.addPage(ovfA, true)
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	first, second := st.holdNextGetTargets(), st.holdNextGetTargets()
	defer closeOnce(first)
	defer closeOnce(second)
	list := ovfAsyncRPC(s, "tab_list")
	awaitGetTargets(t, st, 2) // snapshot A + B + marker, held
	ovfDestroyInBrowser(t, ctx, st, sess, ovfA)
	if len(sess.TargetEvents()) != cap(sess.TargetEvents()) {
		t.Fatal("precondition: the destroyed notification must have been dropped")
	}
	done := make(chan struct{})
	go func() { defer close(done); s.runTargetSync(ctx, sess) }()
	defer func() { cancel(); <-done }()
	awaitGetTargets(t, st, 3) // overflow resync, snapshot without A, held

	closeOnce(first)
	r := <-list
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	_, registered := s.tabs.Lookup(string(ovfA))
	if readerEvidence {
		// The notification was read while the list was in flight: its reply is
		// older than that and never registers A at all.
		if registered || ovfRowsContain(r, ovfA) {
			t.Errorf("tab_list registered/listed A after its destroyed notification was read (registered=%v)", registered)
		}
	} else if !registered || !ovfRowsContain(r, ovfA) {
		t.Fatal("precondition: the earlier snapshot introduces A")
	}

	// Queue the witness behind the consumer's outstanding B-only reply.
	if err := sess.CloseTargetContext(ctx, ovfMarker); err != nil {
		t.Fatal(err)
	}
	st.emit("Target.targetDestroyed", map[string]any{"targetId": ovfMarker})
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatal(err)
	}
	closeOnce(second)
	// Known, not Lookup: the marker stops resolving the moment its notification
	// is read; it is forgotten when the consumer gets to it.
	ovfAwait(t, ctx, "marker removed", func() bool { return !s.tabs.Known(target.ID(marker)) })

	if s.tabs.Has(ovfA) {
		t.Error("A is registered after the overflow resync that did not list it")
	}
	ovfClearsRefuse(t, s, string(ovfA))
	if !st.hasPage(ovfB) {
		t.Fatal("B lost")
	}
	if _, ok := s.tabs.Lookup(string(ovfB)); !ok {
		t.Error("B no longer resolves")
	}
	if !readerEvidence {
		// The contradiction is settled by an enumeration requested after it.
		awaitGetTargets(t, st, 4)
	}
}

func TestDiscoveryRacesOverflowRecovery(t *testing.T) {
	ovfDiscoveryRace(t, false)
}
