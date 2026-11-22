package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Two complete CDP target ids that share a long suffix: the old suffix alias
// ("1234", "ab1234", ...) of one is also a suffix of the other.
const (
	fullIDA = target.ID("AAAA0000000000000000000000AB1234")
	fullIDB = target.ID("BBBB0000000000000000000000AB1234")
)

var errIDConnTargetGone = errors.New("No target with given id found")

type idConnOp struct {
	name string
	id   target.ID
}

// idConn is a fake CDP backend that records the exact target id every command
// was addressed to, and fails any command addressed to a target that is gone.
type idConn struct {
	mu          sync.Mutex
	pages       []target.ID
	nextCreate  []target.ID
	ops         []idConnOp
	inFlight    map[target.ID]int
	maxInFlight map[target.ID]int

	evalDelay   time.Duration
	evalEntered chan struct{} // receives once per Eval when non-nil
	evalGate    chan struct{} // Eval blocks until closed when non-nil
}

func newIDConn(pages ...target.ID) *idConn {
	return &idConn{
		pages:       append([]target.ID(nil), pages...),
		inFlight:    make(map[target.ID]int),
		maxInFlight: make(map[target.ID]int),
	}
}

func (c *idConn) queueCreate(ids ...target.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextCreate = append(c.nextCreate, ids...)
}

// drop removes a page as if it had been closed outside the daemon.
func (c *idConn) drop(id target.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(id)
}

func (c *idConn) removeLocked(id target.ID) {
	kept := c.pages[:0]
	for _, p := range c.pages {
		if p != id {
			kept = append(kept, p)
		}
	}
	c.pages = kept
}

func (c *idConn) has(id target.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasLocked(id)
}

func (c *idConn) hasLocked(id target.ID) bool {
	for _, p := range c.pages {
		if p == id {
			return true
		}
	}
	return false
}

func (c *idConn) opsFor(id target.ID) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, op := range c.ops {
		if op.id == id {
			out = append(out, op.name)
		}
	}
	return out
}

func (c *idConn) opCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ops)
}

func (c *idConn) maxConcurrent(id target.ID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxInFlight[id]
}

func (c *idConn) op(name string, id target.ID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, idConnOp{name: name, id: id})
	if !c.hasLocked(id) {
		return errIDConnTargetGone
	}
	return nil
}

func (c *idConn) PageTargets() ([]*target.Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*target.Info, 0, len(c.pages))
	for _, id := range c.pages {
		out = append(out, &target.Info{TargetID: id, Type: "page", Title: "title-" + string(id), URL: "https://example.test/" + string(id)})
	}
	return out, nil
}

func (c *idConn) CreatePageTarget(string, bool) (target.ID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.nextCreate) == 0 {
		return "", errors.New("idConn: no queued target id")
	}
	id := c.nextCreate[0]
	c.nextCreate = c.nextCreate[1:]
	c.pages = append(c.pages, id)
	return id, nil
}

func (c *idConn) CloseTarget(id target.ID) error {
	if err := c.op("close", id); err != nil {
		return err
	}
	c.drop(id)
	return nil
}

func (c *idConn) Navigate(id target.ID, _ string) error { return c.op("navigate", id) }
func (c *idConn) Reload(id target.ID) error             { return c.op("reload", id) }
func (c *idConn) Click(id target.ID, _ string) error    { return c.op("click", id) }
func (c *idConn) Fill(id target.ID, _, _ string) error  { return c.op("fill", id) }

func (c *idConn) Screenshot(id target.ID, _ string) ([]byte, string, error) {
	if err := c.op("screenshot", id); err != nil {
		return nil, "", err
	}
	return []byte{1}, "image/png", nil
}

func (c *idConn) Eval(id target.ID, _ string) (json.RawMessage, error) {
	c.mu.Lock()
	c.inFlight[id]++
	if c.inFlight[id] > c.maxInFlight[id] {
		c.maxInFlight[id] = c.inFlight[id]
	}
	entered, gate, delay := c.evalEntered, c.evalGate, c.evalDelay
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inFlight[id]--
		c.mu.Unlock()
	}()
	if err := c.op("eval", id); err != nil {
		return nil, err
	}
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return json.RawMessage(`1`), nil
}

func (c *idConn) FetchPage(id target.ID, _, _ string, _ []byte, _ string) (json.RawMessage, error) {
	if err := c.op("fetch", id); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"ok":true,"status":200}`), nil
}

func (c *idConn) Snapshot(id target.ID, _ browser.SnapshotOpts) (string, string, string, map[string]string, error) {
	if err := c.op("snapshot", id); err != nil {
		return "", "", "", nil, err
	}
	return "t", "https://example.test/", "snap", nil, nil
}

func (c *idConn) AppendNetworkRoute(id target.ID, _ browser.NetworkRouteRule) error {
	return c.op("route", id)
}

func (c *idConn) RemoveNetworkRoutes(id target.ID, _ string) error { return c.op("unroute", id) }
func (c *idConn) NetworkRouteCount(target.ID) int                  { return 0 }

func newIDServer(t *testing.T, conn *idConn, stateDir string) *Server {
	t.Helper()
	srv := newIdleTestServerWithState(&fakeConn{}, time.Hour, stateDir)
	srv.tabHook = conn
	return srv
}

type targetedMethod struct {
	method string
	extra  map[string]any
	// cdpOp is the backend command the method must address to the tab ("" when
	// the method only reads or clears daemon-side state).
	cdpOp string
}

// targetedMethods is every JSON-RPC method that addresses one tab. tab_close
// stays last: it destroys the tab.
var targetedMethods = []targetedMethod{
	{method: protocol.MethodTabSelect},
	{method: protocol.MethodGoto, extra: map[string]any{"url": "https://example.test/"}, cdpOp: "navigate"},
	{method: protocol.MethodReload, cdpOp: "reload"},
	{method: protocol.MethodScreenshot, cdpOp: "screenshot"},
	{method: protocol.MethodEval, extra: map[string]any{"script": "1"}, cdpOp: "eval"},
	{method: protocol.MethodClick, extra: map[string]any{"selector": "#a"}, cdpOp: "click"},
	{method: protocol.MethodFill, extra: map[string]any{"selector": "#a", "text": "x"}, cdpOp: "fill"},
	{method: protocol.MethodFetch, extra: map[string]any{"url": "https://example.test/x"}, cdpOp: "fetch"},
	{method: protocol.MethodSnapshot, cdpOp: "snapshot"},
	{method: protocol.MethodNetwork},
	{method: protocol.MethodConsole},
	{method: protocol.MethodErrors},
	{method: protocol.MethodNetworkRoute, extra: map[string]any{"url_pattern": "*", "abort": true}, cdpOp: "route"},
	{method: protocol.MethodNetworkUnroute, cdpOp: "unroute"},
	{method: protocol.MethodNetworkClear},
	{method: protocol.MethodConsoleClear},
	{method: protocol.MethodErrorsClear},
	{method: protocol.MethodTabClose, cdpOp: "close"},
}

func (m targetedMethod) params(tab string) map[string]any {
	p := map[string]any{"tab": tab}
	for k, v := range m.extra {
		p[k] = v
	}
	return p
}

func idTabNew(t *testing.T, srv *Server, conn *idConn, id target.ID) string {
	t.Helper()
	conn.queueCreate(id)
	tab, _ := enumMustRPC(t, srv, protocol.MethodTabNew, map[string]any{"url": "https://example.test/"})["tab"].(string)
	if tab != string(id) {
		t.Fatalf("tab_new returned tab %q, want the complete CDP target id %q", tab, id)
	}
	return tab
}

func expectUnknownTab(t *testing.T, srv *Server, m targetedMethod, tab string) {
	t.Helper()
	reply, err := enumRPC(srv, m.method, m.params(tab))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Error == nil {
		t.Errorf("%s with tab %q succeeded: %v; want unknown tab id", m.method, tab, reply.Result)
		return
	}
	msg, _ := reply.Error["message"].(string)
	code, _ := reply.Error["code"].(float64)
	if int(code) != protocol.CodeInvalidParams || !strings.Contains(msg, "unknown tab id") {
		t.Errorf("%s with tab %q: error %v %q, want %d unknown tab id", m.method, tab, code, msg, protocol.CodeInvalidParams)
	}
}

func TestTabNewListFocusExposeFullTargetID(t *testing.T) {
	conn := newIDConn()
	srv := newIDServer(t, conn, stateDirDisabled)

	// INV-7 and the empty list keep working with zero tabs.
	empty := enumMustRPC(t, srv, protocol.MethodTabList, nil)
	if tabs, _ := empty["tabs"].([]any); len(tabs) != 0 {
		t.Fatalf("tab_list with no tabs = %v", empty)
	}

	idTabNew(t, srv, conn, fullIDA)
	idTabNew(t, srv, conn, fullIDB)

	list := enumMustRPC(t, srv, protocol.MethodTabList, nil)
	tabs, _ := list["tabs"].([]any)
	got := map[string]bool{}
	for _, raw := range tabs {
		item, _ := raw.(map[string]any)
		tab, _ := item["tab"].(string)
		got[tab] = true
	}
	if len(got) != 2 || !got[string(fullIDA)] || !got[string(fullIDB)] {
		t.Fatalf("tab_list tabs = %v, want exactly the two complete target ids", tabs)
	}
	if list["focus"] != string(fullIDB) || list["tab"] != string(fullIDB) {
		t.Fatalf("tab_list focus/tab = %v/%v, want %q (last created)", list["focus"], list["tab"], fullIDB)
	}

	enumMustRPC(t, srv, protocol.MethodTabSelect, map[string]any{"tab": string(fullIDA)})
	focus := enumMustRPC(t, srv, protocol.MethodTabFocus, nil)
	if focus["tab"] != string(fullIDA) || focus["focus"] != string(fullIDA) {
		t.Fatalf("tab_focus = %v, want %q", focus, fullIDA)
	}
	if focus["url"] != "https://example.test/"+string(fullIDA) {
		t.Fatalf("tab_focus url = %v, want the url of %q", focus["url"], fullIDA)
	}
}

func TestEveryTargetedMethodAcceptsCreatedFullID(t *testing.T) {
	conn := newIDConn()
	srv := newIDServer(t, conn, stateDirDisabled)
	tabB := idTabNew(t, srv, conn, fullIDB)
	tabA := idTabNew(t, srv, conn, fullIDA)

	for _, m := range targetedMethods {
		res := enumMustRPC(t, srv, m.method, m.params(tabA))
		if res["tab"] != tabA {
			t.Errorf("%s result tab = %v, want %q", m.method, res["tab"], tabA)
		}
		if _, ok := res["seq"]; !ok {
			t.Errorf("%s result has no seq (INV-1)", m.method)
		}
		if m.cdpOp == "" {
			continue
		}
		ops := conn.opsFor(fullIDA)
		if len(ops) == 0 || ops[len(ops)-1] != m.cdpOp {
			t.Errorf("%s: commands sent to %q = %v, want last %q", m.method, fullIDA, ops, m.cdpOp)
		}
	}
	if conn.has(fullIDA) {
		t.Error("tab_close with the complete id did not close the tab")
	}
	if ops := conn.opsFor(fullIDB); len(ops) != 0 {
		t.Errorf("same-suffix tab %q received %v", tabB, ops)
	}
	if !conn.has(fullIDB) {
		t.Error("same-suffix tab was closed")
	}
}

// fullIDVariants are inputs that used to resolve (tab suffix aliases) or that
// a lenient matcher might accept. None of them is a complete registered id.
func fullIDVariants(id target.ID) []string {
	s := string(id)
	return []string{
		s[len(s)-4:], // the old 4-digit suffix alias
		strings.ToLower(s[len(s)-4:]),
		s[len(s)-6:], // a lengthened alias
		strings.ToLower(s[len(s)-6:]),
		s[len(s)-16:],
		s[1:], // every suffix is rejected, also the longest
		s[:4],
		s[:len(s)-1],       // prefix / truncation
		strings.ToLower(s), // case variant
		" " + s,
		s + " ",
		s + "0",
		"0" + s,
	}
}

func TestShortAliasesAndIDVariantsAreRejected(t *testing.T) {
	conn := newIDConn()
	srv := newIDServer(t, conn, stateDirDisabled)
	tabA := idTabNew(t, srv, conn, fullIDA)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	before := srv.tabIdle.Snapshot()[fullIDA]

	for _, variant := range fullIDVariants(fullIDA) {
		if variant == tabA || variant == string(fullIDB) {
			t.Fatalf("test data: variant %q is a complete id", variant)
		}
		for _, m := range targetedMethods {
			expectUnknownTab(t, srv, m, variant)
		}
	}

	if n := conn.opCount(); n != 0 {
		t.Errorf("rejected ids still reached the browser: %d commands, A got %v", n, conn.opsFor(fullIDA))
	}
	if !conn.has(fullIDA) {
		t.Error("tab was closed through a rejected id")
	}
	if got := srv.tabIdle.Snapshot()[fullIDA]; !got.Equal(before) {
		t.Errorf("a rejected id renewed the tab's idle clock: %v -> %v", before, got)
	}
	if focus := enumMustRPC(t, srv, protocol.MethodTabList, nil)["focus"]; focus != tabA {
		t.Errorf("focus = %v after rejected ids, want %q", focus, tabA)
	}
	if n := tabLockEntryCount(srv); n != 0 {
		t.Errorf("lock entries after rejected ids = %d, want 0", n)
	}
}

// A is gone and the daemon knows it; B shares A's suffix. Whoever still holds
// A's id gets an error from every method and B is never touched.
func TestStaleFullIDNeverReachesSameSuffixTarget(t *testing.T) {
	conn := newIDConn()
	srv := newIDServer(t, conn, stateDirDisabled)
	tabA := idTabNew(t, srv, conn, fullIDA)
	conn.drop(fullIDA)
	tabB := idTabNew(t, srv, conn, fullIDB)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	enumMustRPC(t, srv, protocol.MethodTabSelect, map[string]any{"tab": tabB})
	before := srv.tabIdle.Snapshot()[fullIDB]

	for _, m := range targetedMethods {
		expectUnknownTab(t, srv, m, tabA)
	}

	assertTargetUntouched(t, srv, conn, fullIDB, before)
	if _, managed := srv.tabIdle.Snapshot()[fullIDA]; managed {
		t.Error("the gone tab is still tracked for idle cleanup")
	}
	if n := tabLockEntryCount(srv); n != 0 {
		t.Errorf("lock entries = %d, want 0", n)
	}
}

func assertTargetUntouched(t *testing.T, srv *Server, conn *idConn, id target.ID, idleBefore time.Time) {
	t.Helper()
	if ops := conn.opsFor(id); len(ops) != 0 {
		t.Errorf("target %q received %v", id, ops)
	}
	if !conn.has(id) {
		t.Errorf("target %q was closed", id)
	}
	if got, ok := srv.tabIdle.Snapshot()[id]; !ok || !got.Equal(idleBefore) {
		t.Errorf("target %q idle clock changed: %v -> %v (managed %v)", id, idleBefore, got, ok)
	}
	if _, ok := srv.tabs.Lookup(string(id)); !ok {
		t.Errorf("target %q is no longer registered", id)
	}
}

// A disappears after the daemon resolved it (the registry is stale): the
// command still goes to A's id, fails there, and never lands on B.
func TestTargetGoneAfterResolutionDoesNotRedirect(t *testing.T) {
	for _, m := range targetedMethods {
		if m.cdpOp == "" {
			continue
		}
		t.Run(m.method, func(t *testing.T) {
			conn := newIDConn()
			srv := newIDServer(t, conn, stateDirDisabled)
			tabA := idTabNew(t, srv, conn, fullIDA)
			idTabNew(t, srv, conn, fullIDB)
			before := srv.tabIdle.Snapshot()[fullIDB]
			aBefore := srv.tabIdle.Snapshot()[fullIDA]
			conn.drop(fullIDA) // no enumeration: the registry still lists A

			reply, err := enumRPC(srv, m.method, m.params(tabA))
			if err != nil {
				t.Fatal(err)
			}
			if reply.Error == nil {
				t.Fatalf("%s on a gone target succeeded: %v", m.method, reply.Result)
			}
			if ops := conn.opsFor(fullIDA); len(ops) != 1 || ops[0] != m.cdpOp {
				t.Errorf("commands addressed to A = %v, want [%s]", ops, m.cdpOp)
			}
			assertTargetUntouched(t, srv, conn, fullIDB, before)
			if got, ok := srv.tabIdle.Snapshot()[fullIDA]; ok && !got.Equal(aBefore) {
				t.Errorf("a failed %s renewed the gone tab's idle clock", m.method)
			}
		})
	}
}

// A request is parked on A's operation lock while A disappears and a
// same-suffix B is registered. Once it gets the lock it must fail.
func TestTargetGoneWhileWaitingForLockDoesNotRedirect(t *testing.T) {
	for _, m := range targetedMethods {
		if m.method == protocol.MethodTabSelect {
			continue // takes no operation lock
		}
		t.Run(m.method, func(t *testing.T) {
			conn := newIDConn()
			srv := newIDServer(t, conn, stateDirDisabled)
			tabA := idTabNew(t, srv, conn, fullIDA)

			entered := make(chan struct{}, 1)
			gate := make(chan struct{})
			conn.mu.Lock()
			conn.evalEntered, conn.evalGate = entered, gate
			conn.mu.Unlock()

			holder := make(chan enumRPCReply, 1)
			go func() {
				reply, _ := enumRPC(srv, protocol.MethodEval, map[string]any{"tab": tabA, "script": "1"})
				holder <- reply
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("holder never reached the browser")
			}

			waiter := make(chan enumRPCReply, 1)
			go func() {
				reply, _ := enumRPC(srv, m.method, m.params(tabA))
				waiter <- reply
			}()
			waitTabLockRefs(t, srv, tabA, 2)

			conn.drop(fullIDA)
			idTabNew(t, srv, conn, fullIDB)
			enumMustRPC(t, srv, protocol.MethodTabList, nil) // the daemon learns A is gone
			before := srv.tabIdle.Snapshot()[fullIDB]
			opsBefore := len(conn.opsFor(fullIDA))

			close(gate)
			if reply := <-holder; reply.Error != nil {
				t.Fatalf("in-flight holder failed: %v", reply.Error)
			}
			reply := <-waiter
			msg, _ := reply.Error["message"].(string)
			if reply.Error == nil || !strings.Contains(msg, "unknown tab id") {
				t.Fatalf("%s after waiting for the lock of a gone tab: result %v error %v, want unknown tab id", m.method, reply.Result, reply.Error)
			}
			if got := len(conn.opsFor(fullIDA)); got != opsBefore {
				t.Errorf("the waiter still sent a command to the gone tab: %v", conn.opsFor(fullIDA))
			}
			assertTargetUntouched(t, srv, conn, fullIDB, before)
			if n := tabLockEntryCount(srv); n != 0 {
				t.Errorf("lock entries = %d, want 0", n)
			}
		})
	}
}

func TestSameTargetOperationsAreSerialized(t *testing.T) {
	conn := newIDConn()
	conn.evalDelay = 2 * time.Millisecond
	srv := newIDServer(t, conn, stateDirDisabled)
	tabA := idTabNew(t, srv, conn, fullIDA)
	tabB := idTabNew(t, srv, conn, fullIDB)

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		for _, tab := range []string{tabA, tabB} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reply, err := enumRPC(srv, protocol.MethodEval, map[string]any{"tab": tab, "script": "1"})
				if err != nil || reply.Error != nil {
					errs <- tab
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for tab := range errs {
		t.Errorf("eval on %q failed", tab)
	}
	for _, id := range []target.ID{fullIDA, fullIDB} {
		if got := conn.maxConcurrent(id); got != 1 {
			t.Errorf("max concurrent commands on %q = %d, want 1", id, got)
		}
	}
	if n := tabLockEntryCount(srv); n != 0 {
		t.Errorf("lock entries = %d, want 0", n)
	}
}

func readRPCLog(t *testing.T, dir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, store.RPCLogFile()))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("rpc log line %q: %v", sc.Text(), err)
		}
		out = append(out, rec)
	}
	return out
}

// The identity resolved for a request is the one that renews the idle clock, is
// written to rpc.jsonl, and is recovered after a restart.
func TestFullIDIdentityFlowsThroughIdleAuditAndRecovery(t *testing.T) {
	dir := t.TempDir()
	conn := newIDConn()
	srv := newIDServer(t, conn, dir)
	tabA := idTabNew(t, srv, conn, fullIDA)
	tabB := idTabNew(t, srv, conn, fullIDB)

	// Managed clocks are pinned in the past so a renewal is unmistakable.
	past := time.Now().Add(-10 * time.Minute)
	srv.tabIdle.MarkManagedAt(fullIDA, past)
	srv.tabIdle.MarkManagedAt(fullIDB, past)

	beforeEval := time.Now().UTC()
	enumMustRPC(t, srv, protocol.MethodEval, map[string]any{"tab": tabA, "script": "1"})
	if got := srv.tabIdle.Snapshot()[fullIDA]; got.Before(beforeEval) {
		t.Errorf("eval with the complete id did not renew its tab: %v", got)
	}
	if got := srv.tabIdle.Snapshot()[fullIDB]; !got.Equal(past) {
		t.Errorf("eval on A renewed same-suffix B: %v", got)
	}
	// Rejected and failed requests leave an audit line but no identity.
	if reply, _ := enumRPC(srv, protocol.MethodEval, map[string]any{"tab": "1234", "script": "1"}); reply.Error == nil {
		t.Fatal("suffix alias was accepted")
	}
	drainRPCLog(t, srv)

	var evalAt time.Time
	seen := map[string]int{}
	for _, rec := range readRPCLog(t, dir) {
		action, _ := rec["action"].(string)
		ok, _ := rec["ok"].(bool)
		id, _ := rec["target"].(string)
		if _, legacy := rec["tab"]; legacy {
			t.Errorf("rpc log record still carries the legacy tab field: %v", rec)
		}
		if !ok {
			if id != "" {
				t.Errorf("failed %s was logged with identity %q", action, id)
			}
			continue
		}
		seen[action+" "+id]++
		if action == protocol.MethodEval {
			ts, _ := rec["time"].(string)
			evalAt, _ = time.Parse(time.RFC3339Nano, ts)
		}
	}
	for _, want := range []string{
		protocol.MethodTabNew + " " + tabA,
		protocol.MethodTabNew + " " + tabB,
		protocol.MethodEval + " " + tabA,
	} {
		if seen[want] != 1 {
			t.Errorf("rpc log successful records = %v, want one %q", seen, want)
		}
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}

	// Restart: B was closed while the daemon was down, A is still there.
	conn.drop(fullIDB)
	restarted := newIDServer(t, conn, dir)
	candidates := restarted.beginManagedReconcile()
	snaps, err := restarted.enumerateTabs(t.Context(), conn)
	if err != nil {
		t.Fatal(err)
	}
	restarted.reconcileIdleFromLog(candidates, snaps)
	managed := restarted.tabIdle.Snapshot()
	if len(managed) != 1 {
		t.Fatalf("recovered managed tabs = %v, want only %q", managed, fullIDA)
	}
	if got, ok := managed[fullIDA]; !ok || evalAt.IsZero() || !got.Equal(evalAt) {
		t.Errorf("recovered last activity of A = %v (managed %v), want the logged eval time %v", got, ok, evalAt)
	}
	// The recovered tab is addressed by the same complete id.
	enumMustRPC(t, restarted, protocol.MethodEval, map[string]any{"tab": tabA, "script": "1"})
	if err := restarted.closeAudit(); err != nil {
		t.Fatal(err)
	}
}

// rpc.jsonl written before the complete-id format only names tabs by their
// suffix alias. Such records prove no identity: recovery must not adopt any
// live tab from them, not even one whose id ends in (or equals) the alias.
func TestLegacyShortIDLogIsNotAdoptedOnRecovery(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	legacy := strings.Join([]string{
		`{"action":"tab_new","body":{"jsonrpc":"2.0","method":"tab_new","params":{},"id":1},"tab":"1234","sender_ip":"127.0.0.1","seq":1,"ok":true,"time":"` + at + `"}`,
		`{"action":"eval","body":{"jsonrpc":"2.0","method":"eval","params":{"tab":"1234","script":"1"},"id":2},"tab":"1234","sender_ip":"127.0.0.1","seq":2,"ok":true,"time":"` + at + `"}`,
		`{"action":"tab_new","body":{"jsonrpc":"2.0","method":"tab_new","params":{},"id":3},"tab":"` + string(fullIDB) + `","sender_ip":"127.0.0.1","seq":3,"ok":true,"time":"` + at + `"}`,
		`{"action":"goto","body":{"jsonrpc":"2.0","method":"goto","params":{"tab":"` + string(fullIDB) + `","url":"https://example.test/"},"id":4},"sender_ip":"127.0.0.1","seq":4,"ok":true,"time":"` + at + `"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, store.RPCLogFile()), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	conn := newIDConn(fullIDA, fullIDB, "1234")
	srv := newIDServer(t, conn, dir)
	candidates := srv.beginManagedReconcile()
	snaps, err := srv.enumerateTabs(t.Context(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("snapshots = %v", snaps)
	}
	srv.reconcileIdleFromLog(candidates, snaps)
	if managed := srv.tabIdle.Snapshot(); len(managed) != 0 {
		t.Fatalf("legacy short-id log adopted tabs for idle cleanup: %v", managed)
	}
	srv.cfg.TabIdleTimeout = time.Nanosecond
	srv.closeExpiredTabs(t.Context())
	for _, id := range []target.ID{fullIDA, fullIDB, "1234"} {
		if !conn.has(id) {
			t.Errorf("tab %q was closed on the strength of a legacy log record", id)
		}
	}
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
}
