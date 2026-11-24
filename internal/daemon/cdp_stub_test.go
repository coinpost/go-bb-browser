package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/target"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// cdpStub is a local DevTools endpoint speaking just enough of the protocol for
// a real browser.Session (chromedp remote allocator) to attach. It never starts
// or talks to Chrome and uses loopback only.
//
// Target.getTargets takes its snapshot when the request arrives and can then be
// held back by the test, which is exactly the "stale enumeration" window.
//
// Like Chrome, the stub sends target lifecycle notifications on the browser
// session only after the client asked for them there with a root-level
// Target.setDiscoverTargets{discover:true} (a page-session one, which chromedp
// sends when it attaches, does not count), and answers that command by
// announcing every existing target first. A test therefore cannot hand the
// daemon lifecycle events its production connection would not receive.
type cdpStub struct {
	t   *testing.T
	srv *httptest.Server

	writeMu sync.Mutex
	conn    net.Conn

	mu       sync.Mutex
	discover bool // root-level discovery enabled by the client
	// failDiscover makes the stub a DevTools host without target discovery:
	// the root-level command is answered with an error.
	failDiscover  bool
	discoverReq   int // root-level Target.setDiscoverTargets requests seen
	pages         []target.ID
	nextCreate    []target.ID
	sessionSeq    int
	closed        []target.ID
	getTargets    int
	holdTargets   []chan struct{} // one entry per upcoming Target.getTargets call
	arrived       chan int        // call number of every Target.getTargets request
	checkAttach   bool            // Target.attachToTarget requires targetId in st.pages
	attachErrOnce string          // when non-empty, next Target.attachToTarget fails with this CDP error
	evalExcOnce   string          // when non-empty, next Runtime.evaluate returns an exceptionDetails error
	holds         map[string][]*cdpStubHold
	failures      map[string]int         // upcoming commands per method answered with an error
	failureMsgs   map[string][]string    // error text of those failures, in order; default when exhausted
	sessions      map[target.ID][]string // flat sessions attached per target, oldest first
	detached      map[string]bool        // flat sessions detached again by the client
}

// cdpStubHold parks one upcoming command of a given method: arrived is closed
// when the command reaches the stub, and it is handled once release is closed.
type cdpStubHold struct {
	arrived chan struct{}
	release chan struct{}
	// replyOnly: the command takes effect when it arrives and only its reply
	// waits for release.
	replyOnly bool
}

type cdpStubMessage struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId,omitempty"`

	// replyGate, when set by a reply-only hold, delays every reply to this command.
	replyGate chan struct{} `json:"-"`
}

func newCDPStub(t *testing.T, pages ...target.ID) *cdpStub {
	t.Helper()
	st := &cdpStub{t: t, pages: append([]target.ID(nil), pages...), arrived: make(chan int, 64)}
	mux := http.NewServeMux()
	st.srv = httptest.NewServer(mux)
	wsURL := "ws" + st.srv.URL[len("http"):] + "/devtools/browser/stub"
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Browser": "stub", "webSocketDebuggerUrl": wsURL})
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		out := make([]map[string]string, 0, len(st.pages))
		for _, id := range st.pages {
			out = append(out, map[string]string{"id": string(id), "type": "page"})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/devtools/browser/stub", st.serveWS)
	t.Cleanup(st.close)
	return st
}

func (st *cdpStub) url() string { return st.srv.URL }

func (st *cdpStub) close() {
	st.mu.Lock()
	holds := st.holdTargets
	st.holdTargets = nil
	st.mu.Unlock()
	for _, h := range holds {
		closeOnce(h)
	}
	st.mu.Lock()
	methodHolds := st.holds
	st.holds = nil
	st.mu.Unlock()
	for _, list := range methodHolds {
		for _, h := range list {
			closeOnce(h.release)
		}
	}
	st.writeMu.Lock()
	if st.conn != nil {
		_ = st.conn.Close()
	}
	st.writeMu.Unlock()
	st.srv.Close()
}

func closeOnce(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}

// holdNextGetTargets queues a gate for the next not-yet-gated Target.getTargets
// call. The reply (already snapshotted on arrival) is sent once it is closed.
func (st *cdpStub) holdNextGetTargets() chan struct{} {
	gate := make(chan struct{})
	st.mu.Lock()
	st.holdTargets = append(st.holdTargets, gate)
	st.mu.Unlock()
	return gate
}

// holdNext parks the next not-yet-parked command named method. The returned
// release function is idempotent.
func (st *cdpStub) holdNext(method string) (arrived <-chan struct{}, release func()) {
	h := &cdpStubHold{arrived: make(chan struct{}), release: make(chan struct{})}
	st.mu.Lock()
	if st.holds == nil {
		st.holds = make(map[string][]*cdpStubHold)
	}
	st.holds[method] = append(st.holds[method], h)
	st.mu.Unlock()
	return h.arrived, func() { closeOnce(h.release) }
}

// holdNextReply lets the next command named method take effect as soon as it
// arrives and parks only its reply: the browser has acted, the client has not
// heard yet. The returned release function is idempotent.
func (st *cdpStub) holdNextReply(method string) (arrived <-chan struct{}, release func()) {
	h := &cdpStubHold{arrived: make(chan struct{}), release: make(chan struct{}), replyOnly: true}
	st.mu.Lock()
	if st.holds == nil {
		st.holds = make(map[string][]*cdpStubHold)
	}
	st.holds[method] = append(st.holds[method], h)
	st.mu.Unlock()
	return h.arrived, func() { closeOnce(h.release) }
}

// failNext answers the next command named method with a protocol error.
func (st *cdpStub) failNext(method string) {
	st.mu.Lock()
	if st.failures == nil {
		st.failures = make(map[string]int)
	}
	st.failures[method]++
	st.mu.Unlock()
}

// failNextWith is failNext with the browser's own error text. It must not be
// mixed with failNext for the same method.
func (st *cdpStub) failNextWith(method, message string) {
	st.mu.Lock()
	if st.failureMsgs == nil {
		st.failureMsgs = make(map[string][]string)
	}
	st.failureMsgs[method] = append(st.failureMsgs[method], message)
	st.mu.Unlock()
	st.failNext(method)
}

// sessionFor returns the most recent flat session attached to id.
func (st *cdpStub) sessionFor(id target.ID) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	list := st.sessions[id]
	if len(list) == 0 {
		return "", false
	}
	return list[len(list)-1], true
}

func (st *cdpStub) sessionCount(id target.ID) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions[id])
}

// liveSessions returns the flat sessions attached to id that the client has not
// detached, oldest first.
func (st *cdpStub) liveSessions(id target.ID) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var live []string
	for _, sid := range st.sessions[id] {
		if !st.detached[sid] {
			live = append(live, sid)
		}
	}
	return live
}

// emitSession delivers a target-scoped event on a flat session.
func (st *cdpStub) emitSession(sessionID, method string, params any) {
	st.write(map[string]any{"method": method, "params": params, "sessionId": sessionID})
}

// destroyPage makes a page vanish as if the user closed it, and announces it.
func (st *cdpStub) destroyPage(id target.ID) {
	st.removePage(id, true)
}

// dropPage makes a page vanish without any lifecycle notification.
func (st *cdpStub) dropPage(id target.ID) {
	st.removePage(id, false)
}

func (st *cdpStub) queueCreateID(id target.ID) {
	st.mu.Lock()
	st.nextCreate = append(st.nextCreate, id)
	st.mu.Unlock()
}

func (st *cdpStub) closedTargets() []target.ID {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]target.ID(nil), st.closed...)
}

func (st *cdpStub) hasPage(id target.ID) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.hasPageLocked(id)
}

func (st *cdpStub) hasPageLocked(id target.ID) bool {
	for _, p := range st.pages {
		if p == id {
			return true
		}
	}
	return false
}

func (st *cdpStub) setAttachErrOnce(msg string) {
	st.mu.Lock()
	st.attachErrOnce = msg
	st.mu.Unlock()
}

func (st *cdpStub) setEvalExcOnce(desc string) {
	st.mu.Lock()
	st.evalExcOnce = desc
	st.mu.Unlock()
}

// addPage makes a page appear as if the user opened it, optionally announcing it.
func (st *cdpStub) addPage(id target.ID, announce bool) {
	st.mu.Lock()
	st.pages = append(st.pages, id)
	st.mu.Unlock()
	if announce {
		st.emit("Target.targetCreated", map[string]any{"targetInfo": stubTargetInfo(id)})
	}
}

// removePage makes a page disappear as if the user closed it in the browser:
// no command of the daemon is involved. announce sends the destroyed
// notification (when the client enabled discovery); without it the
// notification is lost.
func (st *cdpStub) removePage(id target.ID, announce bool) {
	st.mu.Lock()
	kept := st.pages[:0]
	for _, p := range st.pages {
		if p != id {
			kept = append(kept, p)
		}
	}
	st.pages = kept
	st.mu.Unlock()
	if announce {
		st.emit("Target.targetDestroyed", map[string]any{"targetId": id})
	}
}

// rootDiscoverRequests is how many root-level Target.setDiscoverTargets
// commands the client sent.
func (st *cdpStub) rootDiscoverRequests() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.discoverReq
}

func (st *cdpStub) getTargetsCalls() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.getTargets
}

func (st *cdpStub) emitInfoChanged(id target.ID) {
	st.emit("Target.targetInfoChanged", map[string]any{"targetInfo": stubTargetInfo(id)})
}

func stubTargetInfo(id target.ID) map[string]any {
	return map[string]any{
		"targetId": string(id), "type": "page", "title": "", "url": "about:blank",
		"attached": false, "canAccessOpener": false,
	}
}

// emit sends a browser-session notification. Target lifecycle notifications
// are only sent once the client enabled root-level discovery.
func (st *cdpStub) emit(method string, params any) {
	if strings.HasPrefix(method, "Target.target") {
		st.mu.Lock()
		on := st.discover
		st.mu.Unlock()
		if !on {
			return
		}
	}
	st.write(map[string]any{"method": method, "params": params})
}

// send writes a reply to msg, after its reply gate (if any) has opened.
func (st *cdpStub) send(msg cdpStubMessage, v any) {
	if msg.replyGate == nil {
		st.write(v)
		return
	}
	go func() {
		<-msg.replyGate
		st.write(v)
	}()
}

func (st *cdpStub) write(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	st.writeMu.Lock()
	defer st.writeMu.Unlock()
	if st.conn == nil {
		return
	}
	_ = wsutil.WriteServerText(st.conn, raw)
}

func (st *cdpStub) reply(msg cdpStubMessage, result any) {
	out := map[string]any{"id": msg.ID, "result": result}
	if msg.SessionID != "" {
		out["sessionId"] = msg.SessionID
	}
	st.send(msg, out)
}

func (st *cdpStub) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		return
	}
	st.writeMu.Lock()
	st.conn = conn
	st.writeMu.Unlock()
	for {
		raw, err := wsutil.ReadClientText(conn)
		if err != nil {
			return
		}
		var msg cdpStubMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		st.handle(msg)
	}
}

func (st *cdpStub) handle(msg cdpStubMessage) {
	st.mu.Lock()
	var hold *cdpStubHold
	if list := st.holds[msg.Method]; len(list) > 0 {
		hold = list[0]
		st.holds[msg.Method] = list[1:]
	}
	st.mu.Unlock()
	if hold == nil {
		st.dispatch(msg)
		return
	}
	if hold.replyOnly {
		msg.replyGate = hold.release
		st.dispatch(msg)
		close(hold.arrived)
		return
	}
	close(hold.arrived)
	// Handle out of band so the read loop keeps serving other commands.
	go func() {
		<-hold.release
		st.dispatch(msg)
	}()
}

func (st *cdpStub) dispatch(msg cdpStubMessage) {
	st.mu.Lock()
	fail := st.failures[msg.Method] > 0
	text := "stub: " + msg.Method + " refused"
	if fail {
		st.failures[msg.Method]--
		if list := st.failureMsgs[msg.Method]; len(list) > 0 {
			text = list[0]
			st.failureMsgs[msg.Method] = list[1:]
		}
	}
	st.mu.Unlock()
	if fail {
		out := map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": text}}
		if msg.SessionID != "" {
			out["sessionId"] = msg.SessionID
		}
		st.send(msg, out)
		return
	}
	switch msg.Method {
	case "Target.getTargets":
		st.mu.Lock()
		st.getTargets++
		n := st.getTargets
		infos := make([]map[string]any, 0, len(st.pages))
		for _, id := range st.pages {
			infos = append(infos, stubTargetInfo(id))
		}
		var gate chan struct{}
		if len(st.holdTargets) > 0 {
			gate = st.holdTargets[0]
			st.holdTargets = st.holdTargets[1:]
		}
		st.mu.Unlock()
		select {
		case st.arrived <- n:
		default:
		}
		send := func() { st.reply(msg, map[string]any{"targetInfos": infos}) }
		if gate == nil {
			send()
			return
		}
		// Reply out of band so the read loop keeps serving other commands.
		go func() {
			<-gate
			send()
		}()
	case "Target.setDiscoverTargets":
		if msg.SessionID != "" {
			st.reply(msg, map[string]any{})
			return
		}
		var p struct {
			Discover bool `json:"discover"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		st.mu.Lock()
		st.discoverReq++
		if st.failDiscover {
			st.mu.Unlock()
			st.write(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "'Target.setDiscoverTargets' wasn't found"}})
			return
		}
		st.discover = p.Discover
		pages := append([]target.ID(nil), st.pages...)
		st.mu.Unlock()
		if p.Discover {
			for _, id := range pages {
				st.emit("Target.targetCreated", map[string]any{"targetInfo": stubTargetInfo(id)})
			}
		}
		st.reply(msg, map[string]any{})
	case "Target.createTarget":
		st.mu.Lock()
		var id target.ID
		if len(st.nextCreate) > 0 {
			id = st.nextCreate[0]
			st.nextCreate = st.nextCreate[1:]
		} else {
			id = target.ID(fmt.Sprintf("C0FFEE%026d", len(st.pages)+1))
		}
		st.pages = append(st.pages, id)
		st.mu.Unlock()
		st.reply(msg, map[string]any{"targetId": string(id)})
	case "Target.attachToTarget":
		var p struct {
			TargetID target.ID `json:"targetId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		st.mu.Lock()
		errMsg := st.attachErrOnce
		st.attachErrOnce = ""
		if errMsg == "" && st.checkAttach && !st.hasPageLocked(p.TargetID) {
			errMsg = "No target with given id found"
		}
		var sid string
		if errMsg == "" {
			st.sessionSeq++
			sid = fmt.Sprintf("stub-session-%d", st.sessionSeq)
			if st.sessions == nil {
				st.sessions = make(map[target.ID][]string)
			}
			st.sessions[p.TargetID] = append(st.sessions[p.TargetID], sid)
		}
		st.mu.Unlock()
		if errMsg != "" {
			st.write(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32602, "message": errMsg}})
			return
		}
		st.reply(msg, map[string]any{"sessionId": sid})
	case "Target.detachFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		st.mu.Lock()
		if st.detached == nil {
			st.detached = make(map[string]bool)
		}
		st.detached[p.SessionID] = true
		st.mu.Unlock()
		st.reply(msg, map[string]any{})
	case "Target.closeTarget":
		var p struct {
			TargetID target.ID `json:"targetId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		st.mu.Lock()
		st.closed = append(st.closed, p.TargetID)
		kept := st.pages[:0]
		for _, id := range st.pages {
			if id != p.TargetID {
				kept = append(kept, id)
			}
		}
		st.pages = kept
		st.mu.Unlock()
		st.reply(msg, map[string]any{"success": true})
	case "Page.getFrameTree":
		st.reply(msg, map[string]any{"frameTree": map[string]any{"frame": map[string]any{
			"id": "stub-frame", "loaderId": "stub-loader", "url": "about:blank",
			"domainAndRegistry": "", "securityOrigin": "://", "mimeType": "text/html",
			"secureContextType": "Secure", "crossOriginIsolatedContextType": "NotIsolated",
			"gatedAPIFeatures": []string{},
		}}})
	case "DOM.getDocument":
		st.reply(msg, map[string]any{"root": map[string]any{
			"nodeId": 1, "backendNodeId": 1, "nodeType": 9, "nodeName": "#document",
			"localName": "", "nodeValue": "", "childNodeCount": 0, "frameId": "stub-frame",
		}})
	case "Runtime.evaluate":
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		var exc string
		if p.Expression != "self" {
			st.mu.Lock()
			exc = st.evalExcOnce
			st.evalExcOnce = ""
			st.mu.Unlock()
		}
		if exc != "" {
			st.reply(msg, map[string]any{
				"result": map[string]any{"type": "object", "subtype": "error", "description": exc},
				"exceptionDetails": map[string]any{
					"exceptionId":  1,
					"text":         "Uncaught",
					"lineNumber":   0,
					"columnNumber": 0,
					"exception":    map[string]any{"type": "object", "subtype": "error", "description": exc},
				},
			})
			return
		}
		st.reply(msg, map[string]any{"result": map[string]any{"type": "number", "value": 1, "description": "1"}})
	default:
		st.reply(msg, map[string]any{})
	}
}
