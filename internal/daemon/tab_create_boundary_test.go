package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/timeout"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Regressions for the boundaries of the creation settlement (review of
// a34fc3b): the round trip's own deadline against the RPC's 60s, the explicit
// notice a forced HTTP stop gives a creation whose request context it cannot
// cancel, the cancel/commit boundary inside lifeMu, a request that is over
// before anything was sent, a buffered reply against the shutdown's abort, and
// what the warnings claim.

// settleLogBuf is a log sink safe for the daemon's goroutines.
type settleLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *settleLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *settleLogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *settleLogBuf) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// createSteps turns Server.createStepHook into channels: one signal per step
// reached, never blocking the daemon.
type createSteps struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func (c *createSteps) at(step string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch == nil {
		c.ch = make(map[string]chan struct{})
	}
	if c.ch[step] == nil {
		c.ch[step] = make(chan struct{}, 16)
	}
	return c.ch[step]
}

func (c *createSteps) hook(step string) {
	select {
	case c.at(step) <- struct{}{}:
	default:
	}
}

func (c *createSteps) await(t *testing.T, step string, limit time.Duration) {
	t.Helper()
	select {
	case <-c.at(step):
	case <-time.After(limit):
		t.Fatalf("the creation never reached step %q within %v", step, limit)
	}
}

// ctxCreateConn is a settleConn whose creation takes the context, as the
// production session does, and records it.
type ctxCreateConn struct {
	*settleConn
	cmu      sync.Mutex
	calls    int
	deadline time.Time
	bounded  bool
}

func (c *ctxCreateConn) CreatePageTargetContext(ctx context.Context, url string, silent bool) (target.ID, error) {
	c.cmu.Lock()
	c.calls++
	c.deadline, c.bounded = ctx.Deadline()
	c.cmu.Unlock()
	return c.settleConn.CreatePageTarget(url, silent)
}

func (c *ctxCreateConn) seen() (calls int, deadline time.Time, bounded bool) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.calls, c.deadline, c.bounded
}

// The creation round trip of a production RPC (handleV1's timeout.Operation,
// production settlement constant) is handed a deadline of its own, the
// settlement later than the RPC's — not the RPC's and not a fresh
// timeout.Operation. browser.TestCreateTargetContextHonoursTheCallerDeadline
// pins that the session keeps it.
func TestCreateRoundTripDeadlineIsTheRPCBudgetPlusTheSettlement(t *testing.T) {
	c := &ctxCreateConn{settleConn: newSettleConn(barrierB)}
	close(c.createRelease)
	srv := newSettleServer(t, c.settleConn, t.TempDir())
	srv.tabHook = c

	before := time.Now()
	res := enumMustRPC(t, srv, protocol.MethodTabNew, nil)
	after := time.Now()
	if res["tab"] != string(barrierA) {
		t.Fatalf("tab_new = %v", res)
	}
	calls, deadline, bounded := c.seen()
	if calls != 1 || !bounded {
		t.Fatalf("creation calls = %d, bounded = %v; want one bounded round trip", calls, bounded)
	}
	if earliest := before.Add(timeout.Operation + createSettleTimeout); deadline.Before(earliest) {
		t.Errorf("the creation round trip ends %v after the request started; the %v RPC budget leaves no %v settlement",
			deadline.Sub(before), timeout.Operation, createSettleTimeout)
	}
	if latest := after.Add(timeout.Operation + createSettleTimeout + createDeadlineMargin); deadline.After(latest) {
		t.Errorf("the creation round trip may run %v, beyond budget + settlement + margin", deadline.Sub(after))
	}
	// The response still fits the HTTP write timeout: settlement, the wait for
	// a cancelled round trip and one rollback stay inside the flush margin.
	if worst := createSettleTimeout + createCancelGrace + createRollbackTimeout; worst >= flushMargin {
		t.Errorf("settlement + rollback = %v does not fit the %v flush margin of the write timeout", worst, flushMargin)
	}
}

// A request that is already over asks the browser for nothing: there is
// nothing to settle, and no page that would then be left unmanaged.
func TestTabNewWhoseRequestIsAlreadyOverSendsNothing(t *testing.T) {
	dir := t.TempDir()
	c := &ctxCreateConn{settleConn: newSettleConn(barrierB)}
	close(c.createRelease)
	srv := newSettleServer(t, c.settleConn, dir)
	srv.tabHook = c
	srv.createSettle = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := settleAwaitReply(t, settleRequest(ctx, srv, protocol.MethodTabNew), enumTestWait)

	msg, hint := settleErrText(r)
	if msg != "failed to create tab" || !strings.Contains(hint, "no tab was requested from the browser") {
		t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
	}
	if calls, _, _ := c.seen(); calls != 0 {
		t.Errorf("Target.createTarget was sent %d times for a request that was already over", calls)
	}
	if c.has(barrierA) {
		t.Error("the browser was made to create a page")
	}
	if n := srv.tabs.OpenWindows(); n != 0 {
		t.Errorf("%d request windows left open", n)
	}
	if n := srv.creates.inFlight(); n != 0 {
		t.Errorf("%d creations still counted in flight", n)
	}
	idle := srv.tabIdle.Snapshot()
	lines := settleClosedLog(t, srv, dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 || created[0].OK || created[0].Target != "" {
		t.Errorf("tab_new audit lines = %+v, want one failed line without a target", created)
	}
	assertSettleNoManagedTrace(t, lines, dir, idle)
}

// The cancel/commit boundary lies inside lifeMu. The reply has arrived and the
// handler waits for the lock; the request ends; the lock is released. Nothing
// is committed: the tab is rolled back and no managed record is written.
func TestTabNewCancelledWhileWaitingForTheCommitLockIsRolledBack(t *testing.T) {
	dir := t.TempDir()
	c := newSettleConn(barrierB)
	srv := newSettleServer(t, c, dir)
	steps := &createSteps{}
	srv.createStepHook = steps.hook

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
	enumAwait(t, c.createEntered, "create reply parked")

	srv.lifeMu.Lock()
	close(c.createRelease)
	// The handler has the reply and is about to wait for (or already waits
	// for) lifeMu; whichever, it gets the lock only after the cancellation.
	steps.await(t, createStepCommit, enumTestWait)
	cancel()
	srv.lifeMu.Unlock()
	r := settleAwaitReply(t, newTab, enumTestWait)

	msg, hint := settleErrText(r)
	if msg != "tab_new interrupted before completion" || !strings.Contains(hint, "cleanup: closed") {
		t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
	}
	if log := c.closeLog(); len(log) != 1 || log[0] != barrierA {
		t.Errorf("Target.closeTarget calls = %v, want exactly one for A", log)
	}
	if c.has(barrierA) || srv.tabs.Has(barrierA) || srv.tabs.Selected() != barrierB {
		t.Error("the tab was left open, registered or selected")
	}
	if n := srv.tabs.OpenWindows(); n != 0 {
		t.Errorf("%d request windows left open", n)
	}
	idle := srv.tabIdle.Snapshot()
	lines := settleClosedLog(t, srv, dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 || created[0].OK || created[0].Target != string(barrierA) {
		t.Errorf("tab_new audit lines = %+v, want one failed line naming A", created)
	}
	assertSettleNoManagedTrace(t, lines, dir, idle)
}

// The other side of the boundary: once the commit section is left, the end of
// the request changes nothing — the tab stays a managed tab and is not closed.
func TestTabNewCancelledAfterTheCommitKeepsItsManagedTab(t *testing.T) {
	dir := t.TempDir()
	c := newSettleConn(barrierB)
	srv := newSettleServer(t, c, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.createStepHook = func(step string) {
		if step == createStepCommitted {
			cancel() // on the handler's goroutine: before it goes on
		}
	}

	newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
	enumAwait(t, c.createEntered, "create reply parked")
	close(c.createRelease)
	settleAwaitReply(t, newTab, enumTestWait)

	if log := c.closeLog(); len(log) != 0 {
		t.Errorf("Target.closeTarget sent for %v after the commit", log)
	}
	if !c.has(barrierA) || !srv.tabs.Has(barrierA) {
		t.Error("the committed tab was closed or unregistered")
	}
	if _, ok := srv.tabIdle.Snapshot()[barrierA]; !ok {
		t.Error("the committed tab is not idle-tracked")
	}
	lines := settleClosedLog(t, srv, dir)
	if n := len(settleLines(lines, "managed_tab_open")); n != 1 {
		t.Errorf("%d managed_tab_open records, want the one of the committed tab", n)
	}
	if n := len(settleLines(lines, "managed_tab_retire")); n != 0 {
		t.Errorf("%d managed_tab_retire records for a tab that is still open", n)
	}
}

// The reply is in the handler's buffer and shutdown stops waiting at the same
// moment. Both cases of the wait are ready then; the complete id that is
// already known must never lose: every run rolls back exactly A and accounts
// for it (the select used to drop the reply about every second time).
func TestBufferedCreateReplyIsNotDroppedWhenShutdownStopsWaiting(t *testing.T) {
	for i := range 32 {
		dir := t.TempDir()
		c := newSettleConn(barrierB)
		srv := newSettleServer(t, c, dir)
		replied := make(chan struct{}, 1)
		srv.createStepHook = func(step string) {
			switch step {
			case createStepReplied:
				replied <- struct{}{}
			case createStepSettling:
				// On the handler's goroutine, before it waits: the reply gets
				// buffered, then shutdown gives up waiting.
				close(c.createRelease)
				<-replied
				srv.creates.stopWaiting()
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
		enumAwait(t, c.createEntered, "create reply parked")
		cancel()
		r := settleAwaitReply(t, newTab, enumTestWait)

		if msg, hint := settleErrText(r); msg != "tab_new interrupted before completion" || !strings.Contains(hint, "cleanup: closed") {
			t.Fatalf("run %d: the buffered reply was dropped: tab_new answered %v / %v", i, r.Result, r.Error)
		}
		if log := c.closeLog(); len(log) != 1 || log[0] != barrierA {
			t.Fatalf("run %d: Target.closeTarget calls = %v, want exactly one for A", i, log)
		}
		lines := settleClosedLog(t, srv, dir)
		if created := settleLines(lines, protocol.MethodTabNew); len(created) != 1 || created[0].OK || created[0].Target != string(barrierA) {
			t.Fatalf("run %d: tab_new audit lines = %+v, want one failed line naming A", i, created)
		}
	}
}

// A creation that failed without the browser's answer is not a refusal: the
// command may have been executed. The answer and the warning say that the
// outcome is not known; a refusal by the browser claims no such thing.
func TestFailedCreateSeparatesARefusalFromALostRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		unknown bool
	}{
		{name: "browser refused", err: &cdproto.Error{Code: -32000, Message: "Not allowed"}},
		{name: "round trip cancelled with the connection", err: fmt.Errorf("Target.createTarget: %w", context.Canceled), unknown: true},
		{name: "round trip deadline", err: fmt.Errorf("Target.createTarget: %w", context.DeadlineExceeded), unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := newSettleConn(barrierB)
			c.reply = func(target.ID) (target.ID, error) { return "", tc.err }
			close(c.createRelease)
			srv := newSettleServer(t, c, dir)
			logs := &settleLogBuf{}
			srv.logger = logs.logger()

			r, err := enumRPC(srv, protocol.MethodTabNew, nil)
			if err != nil {
				t.Fatal(err)
			}
			msg, hint := settleErrText(r)
			if msg != "failed to create tab" {
				t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
			}
			const boundary = "whether it created a tab is not known"
			const warned = "a tab may have been created, its id is not known"
			if got := strings.Contains(hint, boundary); got != tc.unknown {
				t.Errorf("hint = %q; says the outcome is unknown: %v, want %v", hint, got, tc.unknown)
			}
			if got := strings.Contains(logs.String(), warned); got != tc.unknown {
				t.Errorf("log = %q; warns about a possible unmanaged tab: %v, want %v", logs.String(), got, tc.unknown)
			}
			if log := c.closeLog(); len(log) != 0 {
				t.Errorf("Target.closeTarget sent for %v without an id", log)
			}
			if n := srv.tabs.OpenWindows(); n != 0 {
				t.Errorf("%d request windows left open", n)
			}
			lines := settleClosedLog(t, srv, dir)
			if created := settleLines(lines, protocol.MethodTabNew); len(created) != 1 || created[0].OK || created[0].Target != "" {
				t.Errorf("tab_new audit lines = %+v, want one failed line without a target", created)
			}
		})
	}
}

// A rollback that fails is not reported as a rollback: the warning says the tab
// may still be open, and names it.
func TestFailedRollbackOfAnInterruptedTabNewIsNotLoggedAsRolledBack(t *testing.T) {
	dir := t.TempDir()
	c := newSettleConn(barrierB)
	c.closeErr = func(target.ID) error { return errors.New("Target.closeTarget: browser busy") }
	srv := newSettleServer(t, c, dir)
	logs := &settleLogBuf{}
	srv.logger = logs.logger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
	enumAwait(t, c.createEntered, "create reply parked")
	cancel()
	close(c.createRelease)
	r := settleAwaitReply(t, newTab, enumTestWait)

	if _, hint := settleErrText(r); !strings.Contains(hint, "cleanup: not closed (") || !strings.Contains(hint, "may still be open") {
		t.Errorf("hint = %q, want the failed cleanup and that the tab may still be open", hint)
	}
	out := logs.String()
	if strings.Contains(out, "was rolled back") {
		t.Errorf("the log claims a rollback although the close failed:\n%s", out)
	}
	if !strings.Contains(out, "the rollback failed; the tab may still be open and is not managed") || !strings.Contains(out, string(barrierA)) {
		t.Errorf("the log does not report the failed rollback of A:\n%s", out)
	}
	if !c.has(barrierA) {
		t.Error("precondition: the close failed, the page is still there")
	}
}

// pipelinedTabNew sends a tab_new on a raw connection and, once the browser has
// executed the creation, a second request on the same connection. net/http's
// background read takes the first byte of that second request and stops
// reading, so it does not notice when the server closes the connection: the
// context of the tab_new handler is not cancelled by a forced stop.
func pipelinedTabNew(t *testing.T, d *shutdownDaemon) (release func()) {
	t.Helper()
	d.st.queueCreateID(ovfA)
	arrived, release := d.st.holdNextReply("Target.createTarget")
	conn, err := net.Dial("tcp", d.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	post := func(method string) {
		body := rpcReq(method, map[string]any{}, 1)
		if _, err := fmt.Fprintf(conn, "POST /v1 HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", d.addr, len(body), body); err != nil {
			t.Fatal(err)
		}
	}
	post("tab_new")
	select {
	case <-arrived:
	case <-time.After(shutdownTestWait):
		t.Fatal("Target.createTarget never reached the browser")
	}
	post("tab_list")
	// Let the server's background read take the pipelined byte.
	time.Sleep(100 * time.Millisecond)
	return release
}

// Forced HTTP stop with the next request already sent on the connection
// (HTTP/1.1 pipelining), production entry point and session. The handler's
// context is not cancelled by the closed connection, so the stop has to tell
// the creation itself: it enters its settlement, the reply released after the
// stop is not committed, exactly A is closed and accounted for.
func TestForcedShutdownTellsAPipelinedCreationToSettle(t *testing.T) {
	st := newCDPStub(t, ovfB)
	steps := &createSteps{}
	logs := &settleLogBuf{}
	d := startShutdownDaemon(t, st, time.Minute, func(s *Server) {
		forcedStop(s)
		s.createStepHook = steps.hook
		s.logger = logs.logger()
	})
	release := pipelinedTabNew(t, d)
	defer release()

	d.stop()
	// Reached only through the request context or the explicit notice.
	steps.await(t, createStepSettling, shutdownTestWait)
	select {
	case err := <-d.done:
		t.Fatalf("ListenAndServe returned (%v) while a creation reply was in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := d.finish(shutdownTestWait); err == nil || !strings.Contains(err.Error(), "http shutdown") {
		t.Errorf("ListenAndServe returned %v, want the forced http shutdown error", err)
	}

	if closed := st.closedTargets(); len(closed) != 1 || closed[0] != ovfA {
		t.Errorf("Target.closeTarget calls = %v, want exactly one for A", closed)
	}
	if st.hasPage(ovfA) || !st.hasPage(ovfB) {
		t.Error("A was left open, or B was closed")
	}
	if n := d.s.tabs.OpenWindows(); n != 0 {
		t.Errorf("%d request windows left open", n)
	}
	lines := settleLog(t, d.dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 || created[0].OK || created[0].Target != string(ovfA) || created[0].Error != "tab_new interrupted before completion" {
		t.Errorf("tab_new audit lines = %+v, want one failed line naming A", created)
	}
	if n := settleManagedRecords(lines, ovfA); n != 0 {
		t.Errorf("%d managed lifecycle records for the uncommitted tab", n)
	}
	if d.replayedManaged()[ovfA] {
		t.Error("a restart would restore the rolled back tab as managed")
	}
	// The schedule is the one of the finding: the request context never said
	// the request was over, the shutdown's notice did.
	if out := logs.String(); !strings.Contains(out, "its HTTP service has stopped") {
		t.Logf("note: the handler's context was cancelled as well in this run; the explicit notice was not the only signal\n%s", out)
	}
}

// The production budget, unshortened, with the production session: NewServer +
// ListenAndServe, a real browser.Session, handleV1's 60s timeout.Operation and
// the 5s createSettleTimeout. The stub browser executes Target.createTarget and
// holds the reply until the RPC's 60s have passed; the reply then arrives
// within the settlement. The session used to cut the round trip at its own
// timeout.Operation, at the same moment the RPC ended: the reply was dropped,
// the page stayed open, no close was sent and the audit line had no target.
//
// It takes the 60s it is about; -short skips it.
func TestTabNewReplyAfterTheOperationBudgetIsRolledBackByTheProductionSession(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the production 60s operation budget")
	}
	t.Parallel()
	st := newCDPStub(t, ovfB)
	steps := &createSteps{}
	d := startShutdownDaemon(t, st, time.Minute, func(s *Server) { s.createStepHook = steps.hook })
	if _, ok := d.s.tabConn().(interface {
		CreatePageTargetContext(context.Context, string, bool) (target.ID, error)
	}); !ok || d.s.createSettle != 0 {
		t.Fatal("precondition: production session and production settlement")
	}

	st.queueCreateID(ovfA)
	arrived, release := st.holdNextReply("Target.createTarget")
	defer release()
	type answer struct {
		r   enumRPCReply
		err error
	}
	answers := make(chan answer, 1)
	started := time.Now()
	go func() {
		var a answer
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 2 * timeout.Operation}
		res, err := client.Post("http://"+d.addr+"/v1", "application/json", bytes.NewBufferString(rpcReq("tab_new", map[string]any{}, 1)))
		if err != nil {
			a.err = err
		} else {
			a.err = json.NewDecoder(bufio.NewReader(res.Body)).Decode(&a.r)
			_ = res.Body.Close()
		}
		answers <- a
	}()
	select {
	case <-arrived:
	case <-time.After(shutdownTestWait):
		t.Fatal("Target.createTarget never reached the browser")
	}

	// The handler enters its settlement when the RPC's 60s have passed.
	steps.await(t, createStepSettling, timeout.Operation+shutdownTestWait)
	if waited := time.Since(started); waited < timeout.Operation {
		t.Fatalf("the settlement began after %v, before the %v operation budget passed", waited, timeout.Operation)
	}
	// Well past the instant at which a round trip limited to timeout.Operation
	// has ended, well inside the settlement.
	time.Sleep(time.Second)
	release()

	var a answer
	select {
	case a = <-answers:
	case <-time.After(shutdownTestWait + createRollbackTimeout):
		t.Fatal("tab_new was never answered")
	}
	if a.err != nil {
		t.Fatalf("tab_new: %v", a.err)
	}
	msg, hint := settleErrText(a.r)
	if msg != "tab_new interrupted before completion" {
		t.Fatalf("tab_new answered %v / %v; the reply that arrived inside the settlement was not acted on", a.r.Result, a.r.Error)
	}
	if !strings.Contains(hint, string(ovfA)) || !strings.Contains(hint, "cleanup: closed") ||
		!strings.Contains(hint, fmt.Sprintf("the %ds limit of one RPC passed", timeout.OperationSeconds)) {
		t.Errorf("hint = %q, want the RPC limit, the complete tab id and the cleanup outcome", hint)
	}
	if closed := st.closedTargets(); len(closed) != 1 || closed[0] != ovfA {
		t.Errorf("Target.closeTarget calls = %v, want exactly one for A", closed)
	}
	if st.hasPage(ovfA) || !st.hasPage(ovfB) {
		t.Error("A was left open, or B was closed")
	}
	if d.s.tabs.Has(ovfA) || d.s.tabs.OpenWindows() != 0 {
		t.Error("the tab was registered after the RPC limit, or a request window was left open")
	}

	d.stop()
	if err := d.finish(shutdownTestWait); err != nil {
		t.Errorf("ListenAndServe returned %v", err)
	}
	lines := settleLog(t, d.dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 || created[0].OK || created[0].Target != string(ovfA) {
		t.Errorf("tab_new audit lines = %+v, want one failed line naming A", created)
	}
	if n := settleManagedRecords(lines, ovfA); n != 0 {
		t.Errorf("%d managed lifecycle records for a tab that was never committed", n)
	}
}
