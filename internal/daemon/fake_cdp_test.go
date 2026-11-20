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

// fakeCDP is a loopback DevTools endpoint: the HTTP discovery routes plus a
// browser websocket that answers the commands the daemon and chromedp send.
// It lets tests run the real ListenAndServe, with the real browser.Session,
// without a Chrome.
type fakeCDP struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	writeMu    sync.Mutex
	conns      []net.Conn
	targets    []target.ID // page targets, in the order Chrome reports them
	nextCreate []target.ID
	closed     []target.ID

	// failGetTargets makes that many Target.getTargets calls fail.
	failGetTargets int
	// onGetTargets runs after the reply's target list is fixed and before the
	// reply is sent: whatever it does makes that reply stale.
	onGetTargets func()
	// onCloseTarget runs before Target.closeTarget is answered.
	onCloseTarget func(target.ID)
	// closeReplied receives each target once its closeTarget reply is sent.
	closeReplied chan target.ID
}

func newFakeCDP(t *testing.T, targets ...target.ID) *fakeCDP {
	t.Helper()
	f := &fakeCDP{t: t, targets: targets, closeReplied: make(chan target.ID, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"Browser":              "FakeChrome/1.0",
			"webSocketDebuggerUrl": "ws://" + r.Host + "/devtools/browser/fake",
		})
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		list := make([]map[string]string, 0, len(f.targets))
		for _, id := range f.targets {
			list = append(list, map[string]string{"id": string(id), "type": "page"})
		}
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("/devtools/browser/fake", f.serveWS)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.close)
	return f
}

// addr is the host:port to pass as the daemon's debugger URL.
func (f *fakeCDP) addr() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeCDP) close() {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	f.srv.Close()
}

func (f *fakeCDP) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conns = append(f.conns, conn)
	f.mu.Unlock()
	for {
		data, _, err := wsutil.ReadClientData(conn)
		if err != nil {
			return
		}
		var msg struct {
			ID        int64           `json:"id"`
			SessionID string          `json:"sessionId"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		// Commands are answered concurrently, as Chrome does: a hook may hold
		// one reply back while others go out.
		go func() {
			result, errMsg, after := f.handle(msg.Method, msg.Params)
			reply := map[string]any{"id": msg.ID}
			if msg.SessionID != "" {
				reply["sessionId"] = msg.SessionID
			}
			if errMsg != "" {
				reply["error"] = map[string]any{"code": -32000, "message": errMsg}
			} else {
				reply["result"] = result
			}
			f.send(conn, reply)
			if after != nil {
				after()
			}
		}()
	}
}

func (f *fakeCDP) send(conn net.Conn, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = wsutil.WriteServerMessage(conn, ws.OpText, data)
}

// emit sends a browser-level event on every connection.
func (f *fakeCDP) emit(method string, params any) {
	f.mu.Lock()
	conns := append([]net.Conn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		f.send(c, map[string]any{"method": method, "params": params})
	}
}

func fakeTargetInfo(id target.ID) map[string]any {
	return map[string]any{
		"targetId": string(id), "type": "page", "title": "", "url": "about:blank",
		"attached": false, "canAccessOpener": false,
	}
}

// openTarget adds a page as if the user opened it in Chrome.
func (f *fakeCDP) openTarget(id target.ID) {
	f.mu.Lock()
	f.targets = append(f.targets, id)
	f.mu.Unlock()
	f.emit("Target.targetCreated", map[string]any{"targetInfo": fakeTargetInfo(id)})
}

func (f *fakeCDP) closedTargets() []target.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]target.ID(nil), f.closed...)
}

func (f *fakeCDP) hasTarget(id target.ID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cur := range f.targets {
		if cur == id {
			return true
		}
	}
	return false
}

// handle answers one command. after, if set, runs once the reply is sent.
func (f *fakeCDP) handle(method string, params json.RawMessage) (result any, errMsg string, after func()) {
	switch method {
	case "Target.attachToTarget":
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(params, &p)
		return map[string]any{"sessionId": "session-" + p.TargetID}, "", nil
	case "Runtime.evaluate":
		return map[string]any{"result": map[string]any{"type": "object", "className": "Window"}}, "", nil
	case "Page.getFrameTree":
		return map[string]any{"frameTree": map[string]any{"frame": map[string]any{
			"id": "frame", "loaderId": "loader", "url": "about:blank",
			"securityOrigin": "null", "mimeType": "text/html",
		}}}, "", nil
	case "DOM.getDocument":
		return map[string]any{"root": map[string]any{
			"nodeId": 1, "backendNodeId": 1, "nodeType": 9, "nodeName": "#document",
			"localName": "", "nodeValue": "",
		}}, "", nil
	case "Browser.getVersion":
		return map[string]any{
			"protocolVersion": "1.3", "product": "FakeChrome/1.0", "revision": "0",
			"userAgent": "fake", "jsVersion": "0",
		}, "", nil
	case "Target.getTargets":
		f.mu.Lock()
		fail := f.failGetTargets > 0
		if fail {
			f.failGetTargets--
		}
		infos := make([]map[string]any, 0, len(f.targets))
		for _, id := range f.targets {
			infos = append(infos, fakeTargetInfo(id))
		}
		hook := f.onGetTargets
		f.mu.Unlock()
		if fail {
			return nil, "getTargets failed (injected)", nil
		}
		if hook != nil {
			hook()
		}
		return map[string]any{"targetInfos": infos}, "", nil
	case "Target.createTarget":
		f.mu.Lock()
		var id target.ID
		if len(f.nextCreate) > 0 {
			id, f.nextCreate = f.nextCreate[0], f.nextCreate[1:]
		} else {
			id = target.ID(fmt.Sprintf("CREATED%08d", len(f.targets)+len(f.closed)))
		}
		f.targets = append(f.targets, id)
		f.mu.Unlock()
		f.emit("Target.targetCreated", map[string]any{"targetInfo": fakeTargetInfo(id)})
		return map[string]any{"targetId": string(id)}, "", nil
	case "Target.closeTarget":
		var p struct {
			TargetID target.ID `json:"targetId"`
		}
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		f.closed = append(f.closed, p.TargetID)
		hook := f.onCloseTarget
		f.mu.Unlock()
		if hook != nil {
			hook(p.TargetID)
		}
		f.mu.Lock()
		found := false
		kept := f.targets[:0]
		for _, id := range f.targets {
			if id == p.TargetID {
				found = true
				continue
			}
			kept = append(kept, id)
		}
		f.targets = kept
		f.mu.Unlock()
		if !found {
			return nil, "No target with given id found", nil
		}
		return map[string]any{"success": true}, "", func() {
			f.closeReplied <- p.TargetID
			f.emit("Target.targetDestroyed", map[string]any{"targetId": string(p.TargetID)})
		}
	default:
		return map[string]any{}, "", nil
	}
}
