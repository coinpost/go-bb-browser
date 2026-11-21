package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// handshakeHold blocks the fake's reply to one step of the connect handshake.
type handshakeHold struct {
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
	open    sync.Once
}

func newHandshakeHold() *handshakeHold {
	return &handshakeHold{entered: make(chan struct{}), gate: make(chan struct{})}
}

func (h *handshakeHold) wait() {
	h.once.Do(func() { close(h.entered) })
	<-h.gate
}

func (h *handshakeHold) release() { h.open.Do(func() { close(h.gate) }) }

// holdCDPHandshake holds the first CDP command of the handshake that waits for
// Chrome: enabling discovery on the browser session, or (as connect used to)
// attaching to an existing page.
func holdCDPHandshake(cdp *fakeCDP, h *handshakeHold) {
	cdp.onCommand = func(method, sessionID string) {
		if method == "Target.attachToTarget" || (method == "Target.setDiscoverTargets" && sessionID == "") {
			h.wait()
		}
	}
}

func (h *handshakeHold) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("connect never reached the held handshake step")
	}
}

// Connecting attaches to no page, and attaching to the user's page later, on
// first use, can fail half way (here at Page.getFrameTree, after the attach
// itself succeeded). Neither may close the page. Connect used to attach to the
// first existing page and cancel that chromedp context on failure, and a
// cancelled chromedp context closes its target.
func TestFailedPageInitDetachesAndNeverClosesUserPage(t *testing.T) {
	cdp := newFakeCDP(t, cdpExisting)
	cdp.failFrameTree = true
	d := startLiveDaemon(t, cdp, t.TempDir(), nil)
	d.waitListening()
	if n := cdp.called("Target.attachToTarget"); n != 0 {
		t.Fatalf("connect attached to a page %d time(s); the browser session must not own one", n)
	}

	waitFor(t, "existing page to be listed", func() bool { return d.tabList()["aaaa"] })
	if result, rpcErr := d.call(protocol.MethodEval, map[string]any{"tab": "aaaa", "script": "1"}); len(rpcErr) == 0 {
		t.Fatalf("eval on a page whose init fails returned %s", result)
	}
	if n := cdp.called("Target.attachToTarget"); n == 0 {
		t.Fatal("test setup: eval did not attach to the page")
	}
	waitFor(t, "failed attach to be detached", func() bool { return cdp.called("Target.detachFromTarget") > 0 })

	d.stop()
	time.Sleep(100 * time.Millisecond) // anything chromedp sends on cancellation
	if got := cdp.closedTargets(); len(got) != 0 {
		t.Fatalf("Target.closeTarget sent for %v", got)
	}
	if !cdp.hasTarget(cdpExisting) {
		t.Fatal("user page was closed")
	}
}

// Target events are delivered to the CDP session that enabled discovery, and
// the daemon listens on the browser session. A target created and destroyed
// before the Target.createTarget reply must reach the daemon there: tab_new
// then fails without handing out a tab id, the log records creation and
// release, and the user's page that takes the short id over is left alone.
func TestCreateDestroyedBeforeReplyIsSeenOnBrowserSession(t *testing.T) {
	stateDir := t.TempDir()
	cdp := newFakeCDP(t, cdpExisting)
	cdp.nextCreate = []target.ID{cdpManaged}
	cdp.onCreateTarget = func(id target.ID) {
		cdp.destroyTarget(id)
		cdp.openTarget(cdpUser)
	}
	d := startLiveDaemon(t, cdp, stateDir, func(cfg *Config) { cfg.TabIdleTimeout = 100 * time.Millisecond })
	d.waitListening()

	result, rpcErr := d.call(protocol.MethodTabNew, map[string]any{"silent": true})
	if len(rpcErr) == 0 {
		t.Fatalf("tab_new returned %s for a target Chrome had already destroyed", result)
	}
	waitFor(t, "user page to take over the short id", func() bool {
		tid, ok := d.srv.tabs.Lookup("1234")
		return ok && tid == cdpUser
	})
	if got := d.srv.tabIdle.Snapshot(); len(got) != 0 {
		t.Fatalf("managed after a create that left no tab: %v", got)
	}
	time.Sleep(300 * time.Millisecond) // several idle sweeps
	d.stop()

	if got := cdp.closedTargets(); len(got) != 0 {
		t.Fatalf("closed targets = %v, want none", got)
	}
	if !cdp.hasTarget(cdpUser) {
		t.Fatal("user page was closed")
	}
	var actions []string
	for _, rec := range readRPCLog(t, stateDir) {
		if rec.Target == string(cdpManaged) {
			if !rec.OK {
				t.Fatalf("record %s for the created target is not ok", rec.Action)
			}
			actions = append(actions, rec.Action)
		}
	}
	if !slices.Equal(actions, []string{protocol.MethodTabNew, protocol.MethodTabClose}) {
		t.Fatalf("log for the created target = %v, want tab_new then tab_close", actions)
	}
	if got := managedOnDisk(t, stateDir); len(got) != 0 {
		t.Fatalf("managed on disk = %v", got)
	}
}

// Cancelling the daemon while the connect handshake waits for Chrome must end
// ListenAndServe at once, not when Chrome finally answers.
func TestCancelDuringConnectHandshakeStopsListenAndServe(t *testing.T) {
	for name, hold := range map[string]func(*fakeCDP, *handshakeHold){
		"websocket url lookup": func(cdp *fakeCDP, h *handshakeHold) { cdp.onVersion = h.wait },
		"cdp command":          holdCDPHandshake,
	} {
		t.Run(name, func(t *testing.T) {
			cdp := newFakeCDP(t, cdpExisting)
			h := newHandshakeHold()
			defer h.release()
			hold(cdp, h)
			d := startLiveDaemon(t, cdp, t.TempDir(), nil)
			h.awaitEntered(t)

			d.cancel()
			err, ok := d.exited(3 * time.Second)
			if !ok {
				t.Fatal("ListenAndServe still blocked in the connect handshake after cancellation")
			}
			if err != nil {
				t.Fatalf("ListenAndServe after cancellation: %v", err)
			}
			h.release()
			time.Sleep(100 * time.Millisecond) // anything chromedp sends on cancellation
			if got := cdp.closedTargets(); len(got) != 0 {
				t.Fatalf("Target.closeTarget sent for %v", got)
			}
		})
	}
}

// A handshake Chrome never answers ends at its own deadline.
func TestConnectHandshakeDeadline(t *testing.T) {
	cdp := newFakeCDP(t, cdpExisting)
	h := newHandshakeHold()
	defer h.release()
	holdCDPHandshake(cdp, h)
	d := startLiveDaemonServer(t, cdp, t.TempDir(), nil, func(srv *Server) { srv.connectTimeout = 200 * time.Millisecond })
	h.awaitEntered(t)

	err, ok := d.exited(5 * time.Second)
	if !ok {
		t.Fatal("ListenAndServe did not give up on a handshake that never completes")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ListenAndServe = %v, want a handshake deadline error", err)
	}
	h.release()
	time.Sleep(100 * time.Millisecond)
	if got := cdp.closedTargets(); len(got) != 0 {
		t.Fatalf("Target.closeTarget sent for %v", got)
	}
}

// The handshake deadline ends with the handshake: a session that connected in
// time is still usable after the deadline has passed.
func TestConnectDeadlineDoesNotOutliveHandshake(t *testing.T) {
	cdp := newFakeCDP(t, cdpExisting)
	const limit = 150 * time.Millisecond
	d := startLiveDaemonServer(t, cdp, t.TempDir(), nil, func(srv *Server) { srv.connectTimeout = limit })
	d.waitListening()
	time.Sleep(3 * limit)

	sess, ok := d.srv.tabConn().(*browser.Session)
	if !ok {
		t.Fatal("no browser session")
	}
	if err := sess.Context().Err(); err != nil {
		t.Fatalf("session context ended with the handshake deadline: %v", err)
	}
	if tabs := d.tabList(); !tabs["aaaa"] {
		t.Fatalf("tab_list after the handshake deadline = %v", tabs)
	}
	if _, exited := d.exited(0); exited {
		t.Fatal("daemon exited after the handshake deadline")
	}
	d.stop()
}

// A handshake that fails at its last step, with the user's page open, reports
// the failure and sends Chrome nothing about that page: it was never attached,
// so there is nothing to detach and nothing chromedp could close.
func TestFailedConnectHandshakeLeavesUserPageAlone(t *testing.T) {
	cdp := newFakeCDP(t, cdpExisting)
	cdp.failBrowserDiscover = true
	d := startLiveDaemon(t, cdp, t.TempDir(), nil)

	err, ok := d.exited(10 * time.Second)
	if !ok {
		t.Fatal("ListenAndServe did not return after a failed handshake")
	}
	if err == nil || !strings.Contains(err.Error(), "Target.setDiscoverTargets") {
		t.Fatalf("ListenAndServe = %v, want the discovery failure", err)
	}
	time.Sleep(100 * time.Millisecond) // anything chromedp sends on cancellation
	if n := cdp.called("Target.attachToTarget"); n != 0 {
		t.Fatalf("connect attached to a page %d time(s)", n)
	}
	if got := cdp.closedTargets(); len(got) != 0 {
		t.Fatalf("Target.closeTarget sent for %v", got)
	}
	if !cdp.hasTarget(cdpExisting) {
		t.Fatal("user page was closed")
	}
}

// Connecting to a browser with no page opens none (chromedp's own first Run
// would create about:blank), and tab_new still works from zero tabs (INV-7).
func TestConnectWithZeroTabsCreatesNoPage(t *testing.T) {
	cdp := newFakeCDP(t)
	cdp.nextCreate = []target.ID{cdpManaged}
	d := startLiveDaemon(t, cdp, t.TempDir(), nil)
	d.waitListening()
	if tabs := d.tabList(); len(tabs) != 0 {
		t.Fatalf("tab_list on an empty browser = %v", tabs)
	}
	for _, method := range []string{"Target.createTarget", "Target.attachToTarget"} {
		if n := cdp.called(method); n != 0 {
			t.Fatalf("connect sent %s %d time(s) to a browser with no page", method, n)
		}
	}

	var created protocol.TabNewResult
	d.rpc(protocol.MethodTabNew, map[string]any{"silent": true}, &created)
	if created.Tab != "1234" {
		t.Fatalf("tab_new from zero tabs = %+v", created)
	}
	if n := cdp.called("Target.createTarget"); n != 1 {
		t.Fatalf("Target.createTarget sent %d times, want 1", n)
	}
	d.stop()
	if got := cdp.closedTargets(); len(got) != 0 {
		t.Fatalf("Target.closeTarget sent for %v", got)
	}
}

// The parent context only governs the handshake. Once connected, cancelling it
// (and letting the handshake deadline pass) neither ends the session nor makes
// chromedp send anything about the user's page.
func TestSessionOutlivesConnectParentContext(t *testing.T) {
	cdp := newFakeCDP(t, cdpExisting)
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	const limit = 150 * time.Millisecond
	sess, err := browser.ConnectWithTimeout(parent, cdp.addr(), limit)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if n := cdp.called("Target.setDiscoverTargets"); n != 1 {
		t.Fatalf("Target.setDiscoverTargets sent %d times by connect, want 1", n)
	}

	cancelParent()
	time.Sleep(3 * limit)
	if err := sess.Context().Err(); err != nil {
		t.Fatalf("session context ended with the handshake context: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.PingBrowserContext(ctx); err != nil {
		t.Fatalf("ping after the handshake context was cancelled: %v", err)
	}
	pages, err := sess.PageTargetsContext(ctx)
	if err != nil || len(pages) != 1 || pages[0].TargetID != cdpExisting {
		t.Fatalf("page targets after the handshake context was cancelled = %v, %v", pages, err)
	}
	if n := cdp.called("Target.attachToTarget"); n != 0 {
		t.Fatalf("connect attached to a page %d time(s)", n)
	}
	if got := cdp.closedTargets(); len(got) != 0 {
		t.Fatalf("Target.closeTarget sent for %v", got)
	}
}
