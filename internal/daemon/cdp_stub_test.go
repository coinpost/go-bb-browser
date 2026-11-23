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
	failDiscover bool
	discoverReq  int // root-level Target.setDiscoverTargets requests seen
	pages        []target.ID
	nextCreate   []target.ID
	sessionSeq   int
	closed       []target.ID
	getTargets   int
	holdTargets  []chan struct{} // one entry per upcoming Target.getTargets call
	arrived      chan int        // call number of every Target.getTargets request
}

type cdpStubMessage struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId,omitempty"`
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
	for _, p := range st.pages {
		if p == id {
			return true
		}
	}
	return false
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
	st.write(out)
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
		st.mu.Lock()
		st.sessionSeq++
		sid := fmt.Sprintf("stub-session-%d", st.sessionSeq)
		st.mu.Unlock()
		st.reply(msg, map[string]any{"sessionId": sid})
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
		st.reply(msg, map[string]any{"result": map[string]any{"type": "number", "value": 1, "description": "1"}})
	default:
		st.reply(msg, map[string]any{})
	}
}
