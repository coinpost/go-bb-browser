package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/internal/store"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Regressions for a tab_new whose request ends while Target.createTarget has
// been executed by the browser and its reply is still in flight (client gone,
// forced HTTP stop, operation budget). The daemon used to cancel the round
// trip with the request: the reply was dropped, the page stayed open and
// unmanaged, and the audit line said ok=false without a target.
//
// The handler-level tests drive the JSON-RPC handler with a request context
// the test cancels and a fake browser whose create reply is parked; the
// shutdown tests run the production entry point (NewServer + ListenAndServe, a
// real browser.Session) against the CDP stub — loopback only, no Chrome. A is
// the tab being created, B a tab that must never be touched; they share their
// id suffix on purpose.

// settleConn parks the Target.createTarget reply after the page exists in the
// fake browser and lets the test decide what the reply says.
type settleConn struct {
	*barrierConn
	smu   sync.Mutex
	reply func(created target.ID) (target.ID, error) // nil: the created id
}

func newSettleConn(pages ...target.ID) *settleConn {
	return &settleConn{barrierConn: &barrierConn{
		enumConn:      newEnumConn(pages...),
		createEntered: make(chan struct{}),
		createRelease: make(chan struct{}),
	}}
}

func (c *settleConn) CreatePageTarget(url string, silent bool) (target.ID, error) {
	id, err := c.barrierConn.CreatePageTarget(url, silent)
	c.smu.Lock()
	reply := c.reply
	c.smu.Unlock()
	if err == nil && reply != nil {
		return reply(id)
	}
	return id, err
}

func newSettleServer(t *testing.T, c *settleConn, dir string) *Server {
	t.Helper()
	srv := newReconcileRaceServer(t, c.enumConn, dir, 0)
	srv.tabHook = c
	c.nextIDs = []target.ID{barrierA}
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	enumMustRPC(t, srv, protocol.MethodTabSelect, map[string]any{"tab": string(barrierB)})
	return srv
}

// settleRequest posts one JSON-RPC call whose request context is ctx.
func settleRequest(ctx context.Context, srv *Server, method string) <-chan enumRPCReply {
	done := make(chan enumRPCReply, 1)
	go func() {
		var out enumRPCReply
		req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(rpcReq(method, map[string]any{}, 1))).WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			out.Error = map[string]any{"message": "undecodable response: " + rec.Body.String()}
		}
		done <- out
	}()
	return done
}

func settleAwaitReply(t *testing.T, done <-chan enumRPCReply, limit time.Duration) enumRPCReply {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(limit):
		t.Fatalf("tab_new did not return within %v", limit)
		return enumRPCReply{}
	}
}

func settleErrText(r enumRPCReply) (msg, hint string) {
	msg, _ = r.Error["message"].(string)
	data, _ := r.Error["data"].(map[string]any)
	hint, _ = data["hint"].(string)
	return msg, hint
}

type settleLogLine struct {
	Action string `json:"action"`
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
}

// settleLog reads the rpc.jsonl of a state dir whose log has been closed.
func settleLog(t *testing.T, dir string) []settleLogLine {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []settleLogLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var l settleLogLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("rpc.jsonl line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func settleLines(lines []settleLogLine, action string) []settleLogLine {
	var out []settleLogLine
	for _, l := range lines {
		if l.Action == action {
			out = append(out, l)
		}
	}
	return out
}

// settleClosedLog drains the audit queue, closes the log and returns its lines.
func settleClosedLog(t *testing.T, srv *Server, dir string) []settleLogLine {
	t.Helper()
	drainRPCLog(t, srv)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	return settleLog(t, dir)
}

// assertSettleNoManagedTrace: A never had a managed lifecycle — no open and no
// retirement record, nothing idle-tracked, nothing a restart would restore.
func assertSettleNoManagedTrace(t *testing.T, lines []settleLogLine, dir string, idle map[target.ID]time.Time) {
	t.Helper()
	for _, l := range lines {
		if (l.Action == store.ActionManagedTabOpen || l.Action == store.ActionManagedTabRetire) && l.Target == string(barrierA) {
			t.Errorf("managed lifecycle record written for the uncommitted tab: %+v", l)
		}
	}
	if _, ok := idle[barrierA]; ok {
		t.Error("the uncommitted tab is idle-tracked")
	}
	st, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if managedTargetSet(st.ManagedTabs())[barrierA] {
		t.Error("replaying rpc.jsonl restores the uncommitted tab as managed")
	}
}

// The request ends, then the reply names the created tab: exactly that tab is
// closed, nothing is registered, managed or selected, and the audit line
// carries the complete id with ok=false.
func TestInterruptedTabNewRollsBackTheTabItsLateReplyNames(t *testing.T) {
	dir := t.TempDir()
	c := newSettleConn(barrierB)
	srv := newSettleServer(t, c, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
	enumAwait(t, c.createEntered, "A exists in the browser, create reply parked")
	cancel()
	// The handler is in its settlement now or will be; either way the reply
	// arrives after the request ended.
	close(c.createRelease)
	r := settleAwaitReply(t, newTab, enumTestWait)

	msg, hint := settleErrText(r)
	if msg != "tab_new interrupted before completion" {
		t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
	}
	if !strings.Contains(hint, string(barrierA)) || !strings.Contains(hint, "cleanup: closed") {
		t.Errorf("hint = %q, want the complete tab id and the cleanup outcome", hint)
	}
	if log := c.closeLog(); len(log) != 1 || log[0] != barrierA {
		t.Errorf("Target.closeTarget calls = %v, want exactly one for A", log)
	}
	if c.has(barrierA) {
		t.Error("the created tab was left open")
	}
	if !c.has(barrierB) || !srv.tabs.Has(barrierB) || srv.tabs.Selected() != barrierB {
		t.Error("B was closed, unregistered or lost the focus")
	}
	barrierRejects(t, srv, string(barrierA))
	if n := srv.tabs.OpenWindows(); n != 0 {
		t.Errorf("%d request windows left open", n)
	}
	if n := srv.creates.inFlight(); n != 0 {
		t.Errorf("%d creations still counted in flight", n)
	}
	idle := srv.tabIdle.Snapshot()
	lines := settleClosedLog(t, srv, dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 || created[0].OK || created[0].Target != string(barrierA) || created[0].Error != "tab_new interrupted before completion" {
		t.Errorf("tab_new audit lines = %+v, want one failed line naming A", created)
	}
	assertSettleNoManagedTrace(t, lines, dir, idle)
}

// What the late reply says decides, and only a complete id is acted on.
func TestInterruptedTabNewActsOnlyOnAnIdentifiedTab(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(target.ID) (target.ID, error)
	}{
		{name: "reply without a target id", reply: func(target.ID) (target.ID, error) { return "", nil }},
		{name: "reply is an error", reply: func(target.ID) (target.ID, error) { return "", errors.New("Target.createTarget: refused") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := newSettleConn(barrierB)
			c.reply = tc.reply
			srv := newSettleServer(t, c, dir)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
			enumAwait(t, c.createEntered, "create reply parked")
			cancel()
			close(c.createRelease)
			r := settleAwaitReply(t, newTab, enumTestWait)

			if msg, _ := settleErrText(r); msg != "failed to create tab" {
				t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
			}
			if log := c.closeLog(); len(log) != 0 {
				t.Errorf("Target.closeTarget sent for %v without an id from the reply", log)
			}
			if !c.has(barrierB) || !srv.tabs.Has(barrierB) {
				t.Error("B was closed or unregistered")
			}
			if n := srv.tabs.OpenWindows(); n != 0 {
				t.Errorf("%d request windows left open", n)
			}
			idle := srv.tabIdle.Snapshot()
			lines := settleClosedLog(t, srv, dir)
			created := settleLines(lines, protocol.MethodTabNew)
			if len(created) != 1 || created[0].OK || created[0].Target != "" {
				t.Errorf("tab_new audit lines = %+v, want one failed line without a target", created)
			}
			assertSettleNoManagedTrace(t, lines, dir, idle)
		})
	}
}

// The reply never comes: the handler stops waiting after the settlement bound,
// closes nothing (the page the fake browser created is not identified by any
// reply; a listing cannot tell whose it is) and says so.
func TestInterruptedTabNewWithoutReplyIsBoundedAndGuessesNothing(t *testing.T) {
	dir := t.TempDir()
	c := newSettleConn(barrierB)
	srv := newSettleServer(t, c, dir)
	srv.createSettle = 50 * time.Millisecond
	defer close(c.createRelease) // only to let the fake's goroutine end

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
	enumAwait(t, c.createEntered, "create reply parked")
	cancel()
	r := settleAwaitReply(t, newTab, enumTestWait)

	msg, hint := settleErrText(r)
	if msg != "failed to create tab" || !strings.Contains(hint, "its id is not known") {
		t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
	}
	if log := c.closeLog(); len(log) != 0 {
		t.Errorf("Target.closeTarget sent for %v on a guess", log)
	}
	if !c.has(barrierA) {
		t.Error("precondition: the fake browser created the page")
	}
	if srv.tabs.Has(barrierA) || srv.tabs.Selected() != barrierB {
		t.Error("the unidentified page was registered or selected")
	}
	if n := srv.tabs.OpenWindows(); n != 0 {
		t.Errorf("%d request windows left open", n)
	}
	// Not hidden: it is an ordinary unmanaged tab for the next listing.
	if !enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil))[string(barrierA)] {
		t.Error("the page left open is not listed")
	}
	idle := srv.tabIdle.Snapshot()
	lines := settleClosedLog(t, srv, dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 || created[0].OK || created[0].Target != "" {
		t.Errorf("tab_new audit lines = %+v, want one failed line without a target", created)
	}
	assertSettleNoManagedTrace(t, lines, dir, idle)
}

// The rollback of an interrupted creation can fail like any close. The tab is
// then reported as not closed, stays an ordinary unmanaged tab and nothing
// else is closed in its place.
func TestInterruptedTabNewReportsAFailedRollback(t *testing.T) {
	dir := t.TempDir()
	c := newSettleConn(barrierB)
	c.closeErr = func(target.ID) error { return errors.New("Target.closeTarget: browser busy") }
	srv := newSettleServer(t, c, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
	enumAwait(t, c.createEntered, "create reply parked")
	cancel()
	close(c.createRelease)
	r := settleAwaitReply(t, newTab, enumTestWait)

	msg, hint := settleErrText(r)
	if msg != "tab_new interrupted before completion" || !strings.Contains(hint, "cleanup: not closed (") {
		t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
	}
	if log := c.closeLog(); len(log) != 1 || log[0] != barrierA {
		t.Errorf("Target.closeTarget calls = %v, want one attempt on A", log)
	}
	if !c.has(barrierA) || !c.has(barrierB) {
		t.Error("a page disappeared although the close failed")
	}
	idle := srv.tabIdle.Snapshot()
	lines := settleClosedLog(t, srv, dir)
	assertSettleNoManagedTrace(t, lines, dir, idle)
}

// Explicit close before the create reply (the second finding of the item).
// FT28 already refuses the late reply; what is pinned here is the accounting:
// one successful tab_close for A, one failed tab_new naming A, no managed
// lifecycle record at all — whether the request is still alive when the reply
// arrives or has ended meanwhile.
func TestCloseBeforeCreateReplyIsAccountedOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interrupted bool
		wantMsg     string
	}{
		{name: "request alive", wantMsg: "tab closed before tab_new completed"},
		{name: "request ended", interrupted: true, wantMsg: "tab_new interrupted before completion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := newSettleConn(barrierB)
			srv := newSettleServer(t, c, dir)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
			enumAwait(t, c.createEntered, "A exists in the browser, create reply parked")
			a := barrierListed(t, srv, barrierA)
			enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": a})
			if c.has(barrierA) || srv.tabs.Has(barrierA) {
				t.Fatal("precondition: A is closed and unregistered before the reply")
			}
			// The rollback finds the target gone, as Chrome answers.
			c.bmu.Lock()
			c.closeErr = func(id target.ID) error {
				return &browser.CDPError{Kind: browser.ErrorTargetGone, Op: "Target.closeTarget", Target: id, Err: errors.New("No target with given id found")}
			}
			c.bmu.Unlock()
			if tc.interrupted {
				cancel()
			}
			close(c.createRelease)
			r := settleAwaitReply(t, newTab, enumTestWait)

			msg, hint := settleErrText(r)
			if msg != tc.wantMsg {
				t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
			}
			if !strings.Contains(hint, "cleanup: ") || !strings.Contains(hint, string(barrierA)) {
				t.Errorf("hint = %q", hint)
			}
			for _, id := range c.closeLog() {
				if id != barrierA {
					t.Errorf("Target.closeTarget sent for %s", id)
				}
			}
			if srv.tabs.Selected() != barrierB || !c.has(barrierB) {
				t.Error("B lost the focus or was closed")
			}
			barrierRejects(t, srv, a)
			idle := srv.tabIdle.Snapshot()
			lines := settleClosedLog(t, srv, dir)
			var okCloses int
			for _, l := range settleLines(lines, protocol.MethodTabClose) {
				if l.Target != string(barrierA) {
					t.Errorf("tab_close audit line for %q", l.Target)
				}
				if l.OK {
					okCloses++
				}
			}
			if okCloses != 1 {
				t.Errorf("%d successful tab_close lines for A, want exactly the explicit one", okCloses)
			}
			created := settleLines(lines, protocol.MethodTabNew)
			if len(created) != 1 || created[0].OK || created[0].Target != string(barrierA) {
				t.Errorf("tab_new audit lines = %+v, want one failed line naming A", created)
			}
			assertSettleNoManagedTrace(t, lines, dir, idle)
		})
	}
}

// Once shutdown waits for the creations in flight, no further creation is sent
// to the browser.
func TestTabNewAfterShutdownStartedSendsNothing(t *testing.T) {
	dir := t.TempDir()
	c := newSettleConn(barrierB)
	srv := newSettleServer(t, c, dir)
	srv.settleCreations(true)

	r := settleAwaitReply(t, settleRequest(context.Background(), srv, protocol.MethodTabNew), enumTestWait)
	if msg, _ := settleErrText(r); msg != "daemon shutting down" {
		t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
	}
	select {
	case <-c.createEntered:
		t.Error("Target.createTarget was sent after shutdown closed the gate")
	default:
	}
	if c.has(barrierA) || srv.tabs.OpenWindows() != 0 {
		t.Error("a page or a request window was left behind")
	}
}

// settleCreations waits for a creation in flight, and when the connection is
// not to be asked any more (or the wait ran out) it makes the request stop
// waiting for its reply instead of holding the shutdown.
func TestSettleCreationsWaitsBoundedForCreationsInFlight(t *testing.T) {
	t.Run("the reply arrives while shutdown waits", func(t *testing.T) {
		dir := t.TempDir()
		c := newSettleConn(barrierB)
		srv := newSettleServer(t, c, dir)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
		enumAwait(t, c.createEntered, "create reply parked")
		cancel()
		settled := make(chan struct{})
		go func() { srv.settleCreations(true); close(settled) }()
		select {
		case <-settled:
			t.Fatal("shutdown did not wait for the creation in flight")
		case <-time.After(50 * time.Millisecond):
		}
		close(c.createRelease)
		enumAwait(t, settled, "shutdown to see the creation settled")
		// The handler had returned before shutdown went on: rollback done,
		// audit line queued while the log is still open.
		r := settleAwaitReply(t, newTab, enumTestWait)
		if msg, _ := settleErrText(r); msg != "tab_new interrupted before completion" {
			t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
		}
		if c.has(barrierA) {
			t.Error("the created tab was left open")
		}
		lines := settleClosedLog(t, srv, dir)
		if created := settleLines(lines, protocol.MethodTabNew); len(created) != 1 || created[0].Target != string(barrierA) || created[0].OK {
			t.Errorf("tab_new audit lines = %+v", created)
		}
	})
	t.Run("connection not trusted", func(t *testing.T) {
		dir := t.TempDir()
		c := newSettleConn(barrierB)
		srv := newSettleServer(t, c, dir)
		defer close(c.createRelease)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		newTab := settleRequest(ctx, srv, protocol.MethodTabNew)
		enumAwait(t, c.createEntered, "create reply parked")
		cancel()
		start := time.Now()
		srv.settleCreations(false)
		if took := time.Since(start); took > createSettleTimeout {
			t.Errorf("shutdown without a trusted connection waited %v for a creation reply", took)
		}
		r := settleAwaitReply(t, newTab, enumTestWait)
		if msg, _ := settleErrText(r); msg != "failed to create tab" {
			t.Fatalf("tab_new answered %v / %v", r.Result, r.Error)
		}
		if log := c.closeLog(); len(log) != 0 {
			t.Errorf("Target.closeTarget sent for %v", log)
		}
	})
}

// startCreateInFlight starts a tab_new for A on a running daemon and returns
// once the stub has created the page and parked the reply: the browser has
// acted, the daemon has not heard.
func startCreateInFlight(t *testing.T, d *shutdownDaemon) (clientErr <-chan error, answer <-chan enumRPCReply, release func()) {
	t.Helper()
	d.st.queueCreateID(ovfA)
	arrived, release := d.st.holdNextReply("Target.createTarget")
	errs := make(chan error, 1)
	answers := make(chan enumRPCReply, 1)
	go func() {
		r, err := d.rpc("tab_new", nil)
		answers <- r
		errs <- err
	}()
	select {
	case <-arrived:
	case <-time.After(shutdownTestWait):
		t.Fatal("Target.createTarget never reached the browser")
	}
	if !d.st.hasPage(ovfA) {
		t.Fatal("precondition: the browser created A")
	}
	return errs, answers, release
}

func forcedStop(s *Server) {
	s.shutdownTimeout = 30 * time.Millisecond
	s.shutdownDrainTimeout = 30 * time.Millisecond
}

func settleManagedRecords(lines []settleLogLine, id target.ID) (n int) {
	for _, l := range lines {
		if (l.Action == store.ActionManagedTabOpen || l.Action == store.ActionManagedTabRetire) && l.Target == string(id) {
			n++
		}
	}
	return n
}

// The schedule of the finding, on the production entry point: the browser has
// executed Target.createTarget, the reply is in flight, the stop signal comes
// and the HTTP stop is forced (the connection of the request is closed). The
// reply arrives within the settlement: the session and the log are still
// there, exactly A is closed, and rpc.jsonl accounts for it under its complete
// id.
func TestForcedShutdownSettlesCreateReplyInFlight(t *testing.T) {
	st := newCDPStub(t, ovfB)
	d := startShutdownDaemon(t, st, time.Minute, forcedStop)
	clientErr, _, release := startCreateInFlight(t, d)
	defer release()

	d.stop()
	select {
	case err := <-clientErr:
		if err == nil {
			t.Fatal("tab_new was answered although its reply is parked")
		}
	case <-time.After(shutdownTestWait):
		t.Fatal("the forced HTTP stop did not end the request")
	}
	// The request is gone; the daemon must still be waiting for its creation.
	select {
	case err := <-d.done:
		t.Fatalf("ListenAndServe returned (%v) while a creation reply was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	if d.s.tabConn() == nil {
		t.Fatal("the session was released while a creation reply was in flight")
	}
	release()
	// The service error of a forced stop is unchanged.
	if err := d.finish(shutdownTestWait); err == nil || !strings.Contains(err.Error(), "http shutdown") {
		t.Errorf("ListenAndServe returned %v, want the forced http shutdown error", err)
	}

	if closed := st.closedTargets(); len(closed) != 1 || closed[0] != ovfA {
		t.Errorf("Target.closeTarget calls = %v, want exactly one for A", closed)
	}
	if st.hasPage(ovfA) {
		t.Error("the created page was left open, unmanaged")
	}
	if !st.hasPage(ovfB) {
		t.Error("B was closed")
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
}

// The reply never comes. The shutdown is not held beyond the settlement bound,
// nothing is closed on a guess, and the audit line claims neither a target nor
// a success.
func TestForcedShutdownWithoutCreateReplyIsBounded(t *testing.T) {
	st := newCDPStub(t, ovfB)
	d := startShutdownDaemon(t, st, time.Minute, func(s *Server) {
		forcedStop(s)
		s.createSettle = 100 * time.Millisecond
	})
	clientErr, _, release := startCreateInFlight(t, d)
	defer release()

	d.stop()
	start := time.Now()
	_ = d.finish(shutdownTestWait)
	if took := time.Since(start); took > createSettleTimeout {
		t.Errorf("shutdown took %v with a 100ms settlement", took)
	}
	select {
	case err := <-clientErr:
		if err == nil {
			t.Error("tab_new was answered although its reply never came")
		}
	case <-time.After(shutdownTestWait):
		t.Fatal("the request never ended")
	}
	if closed := st.closedTargets(); len(closed) != 0 {
		t.Errorf("Target.closeTarget sent for %v without a reply naming a tab", closed)
	}
	if !st.hasPage(ovfB) {
		t.Error("B was closed")
	}
	if d.s.tabs.Has(ovfA) || d.s.tabs.OpenWindows() != 0 {
		t.Error("the unidentified page was registered, or a request window was left open")
	}
	lines := settleLog(t, d.dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 || created[0].OK || created[0].Target != "" || created[0].Error != "failed to create tab" {
		t.Errorf("tab_new audit lines = %+v, want one failed line without a target", created)
	}
	if n := settleManagedRecords(lines, ovfA); n != 0 {
		t.Errorf("%d managed lifecycle records for a tab the daemon never identified", n)
	}
}

// The connection is lost with the reply in flight: nothing can be learned or
// closed any more. The exit stays bounded and keeps its error, and the request
// is accounted as failed, without a target.
func TestCDPLossWithCreateReplyInFlightIsBounded(t *testing.T) {
	st := newCDPStub(t, ovfB)
	d := startShutdownDaemon(t, st, time.Minute, forcedStop)
	_, _, release := startCreateInFlight(t, d)
	defer release()

	st.writeMu.Lock()
	_ = st.conn.Close()
	st.writeMu.Unlock()

	err := d.finish(shutdownTestWait)
	if err == nil || !strings.Contains(err.Error(), "cdp session lost") {
		t.Fatalf("CDP loss returned %v", err)
	}
	if closed := st.closedTargets(); len(closed) != 0 {
		t.Errorf("Target.closeTarget sent for %v", closed)
	}
	if d.s.tabs.OpenWindows() != 0 {
		t.Error("a request window was left open")
	}
	lines := settleLog(t, d.dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 {
		t.Errorf("%d tab_new audit lines, want the one failed request", len(created))
	}
	for _, l := range created {
		if l.OK || l.Target != "" {
			t.Errorf("tab_new audit line %+v claims a result the lost connection never gave", l)
		}
	}
	if n := settleManagedRecords(lines, ovfA); n != 0 {
		t.Errorf("%d managed lifecycle records for a tab the daemon never identified", n)
	}
}

// An orderly stop is unchanged: the request drains, its reply is committed and
// answered, and the tab is a managed tab for the next process.
func TestGracefulShutdownCommitsCreateReplyInFlight(t *testing.T) {
	st := newCDPStub(t, ovfB)
	d := startShutdownDaemon(t, st, time.Minute, nil)
	clientErr, answer, release := startCreateInFlight(t, d)
	defer release()

	d.beginShutdown()
	release()
	select {
	case err := <-clientErr:
		if err != nil {
			t.Fatalf("tab_new failed during an orderly stop: %v", err)
		}
	case <-time.After(shutdownTestWait):
		t.Fatal("tab_new was never answered")
	}
	if r := <-answer; r.Error != nil || r.Result["tab"] != string(ovfA) {
		t.Errorf("tab_new answered %v / %v", r.Result, r.Error)
	}
	if err := d.finish(shutdownTestWait); err != nil {
		t.Errorf("ListenAndServe returned %v", err)
	}
	if closed := st.closedTargets(); len(closed) != 0 {
		t.Errorf("Target.closeTarget sent for %v", closed)
	}
	lines := settleLog(t, d.dir)
	created := settleLines(lines, protocol.MethodTabNew)
	if len(created) != 1 || !created[0].OK || created[0].Target != string(ovfA) {
		t.Errorf("tab_new audit lines = %+v", created)
	}
	if !d.replayedManaged()[ovfA] {
		t.Error("the committed tab is not managed after a restart")
	}
}
