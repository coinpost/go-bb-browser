//go:build realchrome

package daemon

// Harness for the real-Chrome acceptance tests (real_chrome_test.go). Nothing
// here runs in the default test suite: the build tag keeps it out.
//
//	BB_REAL_CHROME=<path to the Google Chrome binary> \
//	BB_REAL_CHROME_EVIDENCE=<dir for raw CDP logs> \
//	go test -tags realchrome -race -count=5 ./internal/daemon -run '^TestRealChrome'
//
// Every test starts its own Chrome: not headless, a throwaway profile under
// the test's temp dir, DevTools on 127.0.0.1 with a port Chrome picks, DNS
// resolution disabled. It never attaches to, signals or reads any other
// Chrome. The daemon under test is the production one — NewServer +
// ListenAndServe: browser.Connect, the target sync loop, the watchdog and the
// HTTP /v1 endpoint — pointed at a loopback proxy in front of that Chrome.
//
// The proxy forwards the real bytes in both directions and writes every frame
// to cdp.jsonl. It can delay the reply to one request, or the delivery of
// lifecycle notifications; it never fabricates a frame and never sends a
// command of its own. In particular it does not enable target discovery: the
// only Target.setDiscoverTargets on the wire is the daemon's.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/internal/store"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const rcWait = 15 * time.Second

// ---------------------------------------------------------------- Chrome

type rcChrome struct {
	t    *testing.T
	cmd  *exec.Cmd
	addr string // 127.0.0.1:port of the real DevTools endpoint
	dir  string // evidence directory of this run
}

// rcStartChrome launches an isolated, non-headless Chrome. startupWindow=false
// starts it without any page.
func rcStartChrome(t *testing.T, dir string, startupWindow bool) *rcChrome {
	t.Helper()
	bin := os.Getenv("BB_REAL_CHROME")
	if bin == "" {
		t.Fatal("BB_REAL_CHROME must name the Google Chrome binary")
	}
	profile := filepath.Join(t.TempDir(), "chrome-profile")
	args := []string{
		"--user-data-dir=" + profile,
		"--remote-debugging-port=0",
		"--remote-debugging-address=127.0.0.1",
		"--no-first-run", "--no-default-browser-check",
		"--disable-search-engine-choice-screen",
		"--disable-background-networking", "--disable-component-update",
		"--disable-sync", "--disable-default-apps", "--disable-domain-reliability",
		"--disable-client-side-phishing-detection", "--no-pings",
		"--disable-features=Translate,MediaRouter,OptimizationHints,DialMediaRouteProvider",
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		"--use-mock-keychain", "--password-store=basic",
		"--window-size=640,480", "--window-position=40,40",
	}
	if startupWindow {
		args = append(args, "about:blank")
	} else {
		args = append(args, "--no-startup-window")
	}
	logf, err := os.Create(filepath.Join(dir, "chrome.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	shown := append([]string{"<chrome>"}, args...)
	shown[1] = "--user-data-dir=<test temp dir>/chrome-profile"
	_ = os.WriteFile(filepath.Join(dir, "chrome-command.txt"), []byte(strings.Join(shown, " ")+"\n"), 0o600)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &rcChrome{t: t, cmd: cmd, dir: dir}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		_ = logf.Close()
		_ = os.RemoveAll(profile)
	})
	// Chrome writes the port it picked into the profile.
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err == nil {
			if port, _, _ := strings.Cut(string(raw), "\n"); port != "" {
				c.addr = "127.0.0.1:" + strings.TrimSpace(port)
				if _, err := c.get("/json/version"); err == nil {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("Chrome did not open its DevTools port")
		}
		time.Sleep(50 * time.Millisecond)
	}
	version, _ := c.get("/json/version")
	_ = os.WriteFile(filepath.Join(dir, "version.json"), version, 0o600)
	c.recordListeners()
	return c
}

// recordListeners writes the TCP listeners of the Chrome process tree and
// fails unless every one of them is on 127.0.0.1.
func (c *rcChrome) recordListeners() {
	c.t.Helper()
	out, _ := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-a", "-g", strconv.Itoa(c.cmd.Process.Pid)).CombinedOutput()
	_ = os.WriteFile(filepath.Join(c.dir, "listeners.txt"), out, 0o600)
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "(LISTEN)") {
			continue
		}
		n++
		if !strings.Contains(line, "TCP 127.0.0.1:") {
			c.t.Fatalf("Chrome listens outside 127.0.0.1: %s", line)
		}
	}
	if n == 0 {
		c.t.Log("lsof reported no listener for the Chrome process group (not verified by lsof)")
	}
}

func (c *rcChrome) do(method, path string) ([]byte, error) {
	req, err := http.NewRequest(method, "http://"+c.addr+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, body)
	}
	return body, nil
}

func (c *rcChrome) get(path string) ([]byte, error) { return c.do(http.MethodGet, path) }

// pages lists the open page targets, asked from Chrome directly.
func (c *rcChrome) pages() map[target.ID]bool {
	c.t.Helper()
	raw, err := c.get("/json/list")
	if err != nil {
		c.t.Fatal(err)
	}
	var entries []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		c.t.Fatal(err)
	}
	out := make(map[target.ID]bool)
	for _, e := range entries {
		if e.Type == "page" {
			out[target.ID(e.ID)] = true
		}
	}
	return out
}

func (c *rcChrome) onlyPage() target.ID {
	c.t.Helper()
	pages := c.pages()
	if len(pages) != 1 {
		c.t.Fatalf("Chrome has %d pages, want exactly the startup page", len(pages))
	}
	for id := range pages {
		return id
	}
	return ""
}

// userOpen opens a tab the way a user would: no daemon involved.
func (c *rcChrome) userOpen() target.ID {
	c.t.Helper()
	raw, err := c.do(http.MethodPut, "/json/new?about:blank")
	if err != nil {
		c.t.Fatal(err)
	}
	var e struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.ID == "" {
		c.t.Fatalf("/json/new: %v %s", err, raw)
	}
	return target.ID(e.ID)
}

// userClose closes a tab the way a user would and waits until it is gone.
func (c *rcChrome) userClose(id target.ID) {
	c.t.Helper()
	if _, err := c.get("/json/close/" + string(id)); err != nil {
		c.t.Fatal(err)
	}
	rcAwait(c.t, "tab gone from Chrome", func() bool { return !c.pages()[id] })
}

// flood makes Chrome emit lifecycle notifications until done reports true:
// targetInfoChanged of a scratch tab whose URL fragment changes (Chrome
// throttles those after about 200), then scratch tabs opened and closed. It uses its
// own DevTools connection straight to Chrome.
func (c *rcChrome) flood(done func() bool) {
	c.t.Helper()
	scratch := c.userOpen()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	raw, _ := c.get("/json/version")
	_ = json.Unmarshal(raw, &v)
	conn, _, _, err := ws.Dial(context.Background(), v.WS)
	if err != nil {
		c.t.Fatal(err)
	}
	defer conn.Close()
	id := 0
	call := func(session, method string, params any) json.RawMessage {
		id++
		msg := map[string]any{"id": id, "method": method, "params": params}
		if session != "" {
			msg["sessionId"] = session
		}
		out, _ := json.Marshal(msg)
		if err := wsutil.WriteClientText(conn, out); err != nil {
			c.t.Fatal(err)
		}
		for {
			in, err := wsutil.ReadServerText(conn)
			if err != nil {
				c.t.Fatal(err)
			}
			var reply struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(in, &reply) == nil && reply.ID == id {
				return reply.Result
			}
		}
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(call("", "Target.attachToTarget", map[string]any{"targetId": scratch, "flatten": true}), &attached)
	for i := 0; i < 400 && !done(); i++ {
		if i < 190 {
			call(attached.SessionID, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf("location.hash='n%d'", i)})
			continue
		}
		c.userClose(c.userOpen())
	}
	if !done() {
		c.t.Fatal("could not produce enough lifecycle notifications")
	}
}

// ---------------------------------------------------------------- proxy

type rcFrame struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
}

// rcHold delays the reply to one request of the daemon.
type rcHold struct {
	method  string
	arrived chan struct{} // the reply reached the proxy and is being held
	reply   rcFrame
	release chan struct{}
	once    sync.Once
}

func (h *rcHold) Release() { h.once.Do(func() { close(h.release) }) }

type rcProxy struct {
	t      *testing.T
	chrome string
	ln     net.Listener

	logMu sync.Mutex
	log   *os.File

	mu        sync.Mutex
	rules     []*rcHold
	held      map[string]*rcHold // "<session>/<id>" of the request
	withhold  func(method string, id target.ID) bool
	withheld  [][]byte
	discover  int             // root-level Target.setDiscoverTargets sent by the daemon
	closes    []target.ID     // Target.closeTarget sent by the daemon
	sent      map[string]int  // requests of the daemon by method
	rootEvent map[string]int  // root-level notifications delivered to the daemon
	delivered map[string]bool // "<method>/<targetId>" of delivered root lifecycle notifications
	clientMu  sync.Mutex      // serializes writes to the daemon
	client    net.Conn
}

func rcStartProxy(t *testing.T, chrome *rcChrome) *rcProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logf, err := os.Create(filepath.Join(chrome.dir, "cdp.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	p := &rcProxy{
		t: t, chrome: chrome.addr, ln: ln, log: logf,
		held: map[string]*rcHold{}, sent: map[string]int{}, rootEvent: map[string]int{}, delivered: map[string]bool{},
	}
	srv := &http.Server{Handler: http.HandlerFunc(p.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		p.releaseAll()
		_ = srv.Close()
		p.logMu.Lock()
		_ = p.log.Close()
		p.logMu.Unlock()
	})
	return p
}

func (p *rcProxy) addr() string { return p.ln.Addr().String() }

// releaseAll lets every delayed reply through (teardown).
func (p *rcProxy) releaseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range p.held {
		h.Release()
	}
	for _, h := range p.rules {
		h.Release()
	}
}

func (p *rcProxy) record(dir string, raw []byte) {
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		raw, _ = json.Marshal(string(raw))
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	fmt.Fprintf(p.log, `{"t":%q,"dir":%q,"msg":%s}`+"\n", time.Now().Format("15:04:05.000000"), dir, raw)
}

func (p *rcProxy) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		// The DevTools HTTP endpoints, with Chrome's address replaced by ours
		// so the daemon's websocket comes through the proxy as well.
		req, _ := http.NewRequest(r.Method, "http://"+p.chrome+r.URL.RequestURI(), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(bytes.ReplaceAll(body, []byte(p.chrome), []byte(p.addr())))
		return
	}
	up, _, _, err := ws.Dial(r.Context(), "ws://"+p.chrome+r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()
	down, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		return
	}
	defer down.Close()
	p.mu.Lock()
	p.client = down
	p.mu.Unlock()
	go p.fromChrome(up, down)
	for {
		raw, err := wsutil.ReadClientText(down)
		if err != nil {
			return
		}
		p.record("daemon->chrome", raw)
		var f rcFrame
		if json.Unmarshal(raw, &f) == nil && f.Method != "" {
			p.mu.Lock()
			p.sent[f.Method]++
			switch f.Method {
			case "Target.setDiscoverTargets":
				if f.SessionID == "" {
					p.discover++
				}
			case "Target.closeTarget":
				var q struct {
					TargetID target.ID `json:"targetId"`
				}
				_ = json.Unmarshal(f.Params, &q)
				p.closes = append(p.closes, q.TargetID)
			}
			for i, h := range p.rules {
				if h.method == f.Method {
					p.rules = append(p.rules[:i], p.rules[i+1:]...)
					p.held[fmt.Sprintf("%s/%d", f.SessionID, f.ID)] = h
					break
				}
			}
			p.mu.Unlock()
		}
		if err := wsutil.WriteClientText(up, raw); err != nil {
			return
		}
	}
}

func (p *rcProxy) toDaemon(down net.Conn, dir string, raw []byte) {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	p.record(dir, raw)
	_ = wsutil.WriteServerText(down, raw)
}

func (p *rcProxy) fromChrome(up, down net.Conn) {
	for {
		raw, err := wsutil.ReadServerText(up)
		if err != nil {
			return
		}
		var f rcFrame
		_ = json.Unmarshal(raw, &f)
		if f.ID != 0 {
			key := fmt.Sprintf("%s/%d", f.SessionID, f.ID)
			p.mu.Lock()
			h := p.held[key]
			delete(p.held, key)
			p.mu.Unlock()
			if h != nil {
				p.record("chrome->proxy (reply held)", raw)
				h.reply = f
				close(h.arrived)
				go func() {
					<-h.release
					p.toDaemon(down, "proxy->daemon (held reply released)", raw)
				}()
				continue
			}
		} else if f.SessionID == "" && strings.HasPrefix(f.Method, "Target.target") {
			var q struct {
				TargetID   target.ID `json:"targetId"`
				TargetInfo struct {
					TargetID target.ID `json:"targetId"`
				} `json:"targetInfo"`
			}
			_ = json.Unmarshal(f.Params, &q)
			id := q.TargetID
			if id == "" {
				id = q.TargetInfo.TargetID
			}
			p.mu.Lock()
			if p.withhold != nil && p.withhold(f.Method, id) {
				p.withheld = append(p.withheld, raw)
				p.mu.Unlock()
				p.record("chrome->proxy (notification withheld)", raw)
				continue
			}
			p.rootEvent[f.Method]++
			p.delivered[f.Method+"/"+string(id)] = true
			p.mu.Unlock()
		}
		p.toDaemon(down, "chrome->daemon", raw)
	}
}

// holdNext delays the reply to the daemon's next request of method.
func (p *rcProxy) holdNext(method string) *rcHold {
	h := &rcHold{method: method, arrived: make(chan struct{}), release: make(chan struct{})}
	p.mu.Lock()
	p.rules = append(p.rules, h)
	p.mu.Unlock()
	return h
}

func (p *rcProxy) awaitHeld(h *rcHold, what string) {
	p.t.Helper()
	select {
	case <-h.arrived:
	case <-time.After(rcWait):
		p.t.Fatalf("%s: the reply to %s never reached the proxy", what, h.method)
	}
}

// withholdNotifications keeps the root-level lifecycle notifications fn
// selects away from the daemon until deliverWithheld (or for good).
func (p *rcProxy) withholdNotifications(fn func(method string, id target.ID) bool) {
	p.mu.Lock()
	p.withhold = fn
	p.mu.Unlock()
}

func (p *rcProxy) deliverWithheld() {
	p.mu.Lock()
	p.withhold = nil
	raws := p.withheld
	p.withheld = nil
	down := p.client
	p.mu.Unlock()
	for _, raw := range raws {
		p.toDaemon(down, "proxy->daemon (withheld notification delivered)", raw)
	}
}

func (p *rcProxy) wasDelivered(method string, id target.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.delivered[method+"/"+string(id)]
}

type rcWire struct {
	discover   int
	closes     []target.ID
	getTargets int
	rootEvents int
}

func (p *rcProxy) wire() rcWire {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := rcWire{discover: p.discover, closes: append([]target.ID(nil), p.closes...), getTargets: p.sent["Target.getTargets"]}
	for _, n := range p.rootEvent {
		w.rootEvents += n
	}
	return w
}

// ---------------------------------------------------------------- daemon

type rcEnv struct {
	t      *testing.T
	dir    string
	chrome *rcChrome
	proxy  *rcProxy
	srv    *Server
	base   string
	state  string
	stop   context.CancelFunc
	done   chan error
	b      target.ID // the page Chrome started with ("" when started without one)
}

func rcEvidenceDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv("BB_REAL_CHROME_EVIDENCE")
	if root == "" {
		root = t.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, strings.ReplaceAll(t.Name(), "/", "_")+"-")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// rcStart launches Chrome, the proxy and the production daemon with its
// default configuration.
func rcStart(t *testing.T, startupWindow bool) *rcEnv {
	t.Helper()
	return rcStartConfigured(t, startupWindow, nil)
}

// rcStartPatient is rcStart for schedules that keep a reply to the daemon's
// own enumeration delayed for longer than the default 2s watchdog timeout
// allows (--cdp-watchdog-timeout / --cdp-watchdog-interval, raised to 30s/60s).
func rcStartPatient(t *testing.T) *rcEnv {
	t.Helper()
	return rcStartConfigured(t, true, func(cfg *Config) {
		cfg.CDPWatchdogInterval = time.Minute
		cfg.CDPWatchdogTimeout = 30 * time.Second
	})
}

func rcStartConfigured(t *testing.T, startupWindow bool, tune func(*Config)) *rcEnv {
	t.Helper()
	dir := rcEvidenceDir(t)
	e := &rcEnv{t: t, dir: dir, state: filepath.Join(dir, "state")}
	e.chrome = rcStartChrome(t, dir, startupWindow)
	if startupWindow {
		e.b = e.chrome.onlyPage()
	} else if n := len(e.chrome.pages()); n != 0 {
		t.Fatalf("Chrome started with %d pages, want none", n)
	}
	e.proxy = rcStartProxy(t, e.chrome)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	_ = ln.Close()
	cfg := Config{DebuggerURL: e.proxy.addr(), ListenAddr: listen, StateDir: e.state}
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logf, err := os.Create(filepath.Join(dir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	e.srv, err = NewServer(cfg, slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.stop = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- e.srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		e.shutdown()
		_ = logf.Close()
	})
	e.base = "http://" + listen
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(e.base + "/health")
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if ok {
				break
			}
		}
		select {
		case err := <-e.done:
			e.done <- err
			e.stop = nil
			if !startupWindow && err != nil && strings.Contains(err.Error(), "no browser is open") {
				t.Skipf("NOT VERIFIED: this Chrome refuses to open the daemon's first tab when it runs without any window, so the daemon cannot start: %v", err)
			}
			t.Fatalf("daemon exited during startup: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not become healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return e
}

// shutdown stops the daemon (idempotent) and returns once it has closed its
// log.
func (e *rcEnv) shutdown() {
	if e.stop == nil {
		return
	}
	e.proxy.releaseAll()
	e.stop()
	e.stop = nil
	select {
	case <-e.done:
	case <-time.After(30 * time.Second):
		e.t.Error("daemon did not shut down")
	}
}

// managedOnDisk stops the daemon and replays its rpc.jsonl.
func (e *rcEnv) managedOnDisk() map[target.ID]bool {
	e.t.Helper()
	e.shutdown()
	replay, err := store.Open(store.OpenConfig{StateDir: e.state})
	if err != nil {
		e.t.Fatal(err)
	}
	defer replay.Close()
	return managedTargetSet(replay.ManagedTabs())
}

func (e *rcEnv) session() *browser.Session {
	e.t.Helper()
	sess, ok := e.srv.tabConn().(*browser.Session)
	if !ok {
		e.t.Fatal("the daemon has no browser session")
	}
	return sess
}

type rcReply struct {
	Result map[string]any `json:"result"`
	Error  map[string]any `json:"error"`
}

func (e *rcEnv) rpc(method string, params map[string]any) rcReply {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(e.base+"/v1", "application/json", bytes.NewReader(body))
	if err != nil {
		return rcReply{Error: map[string]any{"message": err.Error()}}
	}
	defer resp.Body.Close()
	var out rcReply
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return rcReply{Error: map[string]any{"message": err.Error()}}
	}
	return out
}

func (e *rcEnv) rpcAsync(method string, params map[string]any) <-chan rcReply {
	ch := make(chan rcReply, 1)
	go func() { ch <- e.rpc(method, params) }()
	return ch
}

func (e *rcEnv) mustRPC(method string, params map[string]any) map[string]any {
	e.t.Helper()
	r := e.rpc(method, params)
	if r.Error != nil {
		e.t.Fatalf("%s: %v", method, r.Error)
	}
	return r.Result
}

func (e *rcEnv) listed(id target.ID) bool {
	e.t.Helper()
	rows, _ := e.mustRPC("tab_list", nil)["tabs"].([]any)
	for _, row := range rows {
		if row.(map[string]any)["tab"] == string(id) {
			return true
		}
	}
	return false
}

// clearsAccept reports how many of the three observation clears accept tab.
// A clear that is not accepted must have been refused as "unknown tab id": a
// lookup that failed (a Target.getTargets that timed out, say) says nothing
// about the id and is reported as a test failure, not counted as a refusal.
func (e *rcEnv) clearsAccept(id target.ID) int {
	e.t.Helper()
	n := 0
	for _, m := range []string{"network_clear", "console_clear", "errors_clear"} {
		r := e.rpc(m, map[string]any{"tab": string(id)})
		if r.Error == nil {
			n++
		} else if r.Error["message"] != "unknown tab id" {
			e.t.Errorf("%s on %s failed with %v, which is not a refusal of the id", m, id, r.Error)
		}
	}
	return n
}

func (e *rcEnv) idleTracked(id target.ID) bool {
	_, ok := e.srv.tabIdle.Snapshot()[id]
	return ok
}

// park leaves the daemon's lifecycle consumer waiting for the reply to an
// enumeration of its own, using nothing but delayed replies: a tab_list whose
// reply is held, a tab opened meanwhile (registered by its notification), and
// the verification the daemon requests when the held reply does not list it.
// It returns the hold that parks the consumer.
func (e *rcEnv) park() *rcHold {
	e.t.Helper()
	first := e.proxy.holdNext("Target.getTargets")
	list := e.rpcAsync("tab_list", nil)
	e.proxy.awaitHeld(first, "park: first list")
	x := e.chrome.userOpen()
	rcAwait(e.t, "the notification registered the new tab", func() bool { return e.srv.tabs.Has(x) })
	parked := e.proxy.holdNext("Target.getTargets")
	first.Release()
	if r := <-list; r.Error != nil {
		e.t.Fatal(r.Error)
	}
	e.proxy.awaitHeld(parked, "park: the daemon's own verification")
	return parked
}

func rcAwait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(rcWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func rcCreatedID(t *testing.T, h *rcHold) target.ID {
	t.Helper()
	var r struct {
		TargetID target.ID `json:"targetId"`
	}
	if err := json.Unmarshal(h.reply.Result, &r); err != nil || r.TargetID == "" {
		t.Fatalf("held Target.createTarget reply has no target id: %s", h.reply.Result)
	}
	return r.TargetID
}

// rcOnlyClosed fails when the daemon sent a Target.closeTarget for anything
// but allowed.
func rcOnlyClosed(t *testing.T, w rcWire, allowed ...target.ID) {
	t.Helper()
	for _, id := range w.closes {
		if !containsTarget(allowed, id) {
			t.Errorf("unexpected Target.closeTarget(%s) on the wire; all closes: %v", id, w.closes)
		}
	}
}
