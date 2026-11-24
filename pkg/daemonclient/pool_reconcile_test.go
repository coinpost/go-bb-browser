package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// scriptedDaemon is a JSON-RPC fake whose replies come from a per-test
// handler. The handler runs outside the fake's lock, so it may block on a
// barrier channel to hold one response in flight while the test drives other
// calls.
type scriptedDaemon struct {
	mu     sync.Mutex
	calls  [][2]string // method, tab
	handle func(method, tab string) (any, error)
}

func newScriptedDaemon(t *testing.T, handle func(method, tab string) (any, error)) (*httptest.Server, *scriptedDaemon) {
	t.Helper()
	st := &scriptedDaemon{handle: handle}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				Tab string `json:"tab"`
			} `json:"params"`
		}
		_ = json.Unmarshal(b, &req)
		st.mu.Lock()
		st.calls = append(st.calls, [2]string{req.Method, req.Params.Tab})
		st.mu.Unlock()
		result, err := st.handle(req.Method, req.Params.Tab)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func (st *scriptedDaemon) count(method string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, c := range st.calls {
		if c[0] == method {
			n++
		}
	}
	return n
}

// scriptedList builds a tab_list result whose tab / focus metadata name the
// first listed tab.
func scriptedList(tabs ...string) map[string]any {
	items := make([]map[string]string, 0, len(tabs))
	for _, tab := range tabs {
		items = append(items, map[string]string{"tab": tab, "title": "t", "url": "u"})
	}
	out := map[string]any{"seq": 1, "tabs": items}
	if len(tabs) > 0 {
		out["tab"] = tabs[0]
		out["focus"] = tabs[0]
	}
	return out
}

func scriptedTab(tab string) map[string]any {
	return map[string]any{"seq": 1, "tab": tab, "focus": tab, "title": "t", "url": "u", "result": json.RawMessage("1")}
}

func poolBoundCount(p *Pool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tabs)
}

func mustTabList(t *testing.T, p *Pool) protocol.TabListResult {
	t.Helper()
	out, err := p.TabList(context.Background(), protocol.TabListParams{})
	if err != nil {
		t.Fatalf("TabList: %v", err)
	}
	return out
}

func assertPoolBound(t *testing.T, p *Pool, tab string) {
	t.Helper()
	if _, ok := p.ClientForTab(tab); !ok {
		t.Fatalf("tab %q must be bound", tab)
	}
}

// assertPoolUnknown checks that tab is unbound and that a tab-scoped call
// fails locally with *UnknownTabError.
func assertPoolUnknown(t *testing.T, p *Pool, tab string) {
	t.Helper()
	if _, ok := p.ClientForTab(tab); ok {
		t.Fatalf("tab %q must not be bound", tab)
	}
	_, err := p.Eval(context.Background(), protocol.EvalParams{Tab: tab, Script: "1"})
	var ut *UnknownTabError
	if !errors.As(err, &ut) {
		t.Fatalf("Eval(%q): want *UnknownTabError, got %v", tab, err)
	}
}

func newScriptedPool(t *testing.T, srvs ...*httptest.Server) *Pool {
	t.Helper()
	clients := make([]*Client, len(srvs))
	for i, srv := range srvs {
		clients[i] = NewClient(srv.URL)
	}
	p, err := NewPool(clients...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// FT26 acceptance: five lists each naming one different tab must leave one
// binding, and the retired ids must fail locally without an RPC.
func TestPool_tabListRetiresTabsNoLongerListed(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv, st := newScriptedDaemon(t, func(method, tab string) (any, error) {
		if method != protocol.MethodTabList {
			return scriptedTab(tab), nil
		}
		mu.Lock()
		defer mu.Unlock()
		out := scriptedList("t" + strconv.Itoa(n))
		n++
		return out, nil
	})
	p := newScriptedPool(t, srv)
	for range 5 {
		mustTabList(t, p)
	}
	if got := poolBoundCount(p); got != 1 {
		t.Fatalf("bound tabs after 5 single-tab lists: got %d want 1", got)
	}
	assertPoolBound(t, p, "0:t4")
	for i := range 4 {
		assertPoolUnknown(t, p, "0:t"+strconv.Itoa(i))
	}
	if got := st.count(protocol.MethodEval); got != 0 {
		t.Fatalf("retired ids must not reach the daemon, got %d eval RPCs", got)
	}
}

// A successful empty list is authoritative too, and tab / focus metadata must
// not keep a tab alive that the Tabs set no longer names.
func TestPool_tabListEmptyClearsBackend(t *testing.T) {
	var mu sync.Mutex
	list := scriptedList("a", "b")
	srv, _ := newScriptedDaemon(t, func(method, tab string) (any, error) {
		switch method {
		case protocol.MethodTabNew:
			return scriptedTab("n"), nil
		case protocol.MethodTabList:
			mu.Lock()
			defer mu.Unlock()
			return list, nil
		}
		return scriptedTab(tab), nil
	})
	p := newScriptedPool(t, srv)
	if _, err := p.TabNew(context.Background(), protocol.TabNewParams{}); err != nil {
		t.Fatal(err)
	}
	mustTabList(t, p)
	assertPoolBound(t, p, "0:a")
	assertPoolBound(t, p, "0:b")
	assertPoolUnknown(t, p, "0:n")

	mu.Lock()
	list = map[string]any{"seq": 2, "tabs": []any{}, "tab": "a", "focus": "b"}
	mu.Unlock()
	out := mustTabList(t, p)
	if len(out.Tabs) != 0 {
		t.Fatalf("tabs %v", out.Tabs)
	}
	if got := poolBoundCount(p); got != 0 {
		t.Fatalf("bound tabs after empty list: got %d want 0", got)
	}
	assertPoolUnknown(t, p, "0:a")
	assertPoolUnknown(t, p, "0:b")
}

// Only a backend whose tab_list succeeded is reconciled; a failed backend
// keeps its bindings, and one backend's list never touches another's.
func TestPool_tabListReconcilesPerBackendAndSkipsFailures(t *testing.T) {
	var mu sync.Mutex
	lists := [2]map[string]any{scriptedList("x"), scriptedList("old")}
	fail := [2]bool{}
	handler := func(i int) func(method, tab string) (any, error) {
		return func(method, tab string) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			if fail[i] {
				return nil, errors.New("down")
			}
			if method == protocol.MethodTabList {
				return lists[i], nil
			}
			return scriptedTab(tab), nil
		}
	}
	a, _ := newScriptedDaemon(t, handler(0))
	b, _ := newScriptedDaemon(t, handler(1))
	p := newScriptedPool(t, a, b)
	mustTabList(t, p)
	assertPoolBound(t, p, "0:x")
	assertPoolBound(t, p, "1:old")

	mu.Lock()
	fail[0] = true
	lists[0] = scriptedList()
	lists[1] = scriptedList("new")
	mu.Unlock()
	mustTabList(t, p)
	assertPoolBound(t, p, "0:x") // backend 0 failed: not reconciled
	assertPoolBound(t, p, "1:new")
	assertPoolUnknown(t, p, "1:old")
	// The same native id on another backend is a different binding.
	assertPoolUnknown(t, p, "1:x")

	mu.Lock()
	fail[1] = true
	mu.Unlock()
	if _, err := p.TabList(context.Background(), protocol.TabListParams{}); err == nil {
		t.Fatal("want AllFailedError")
	}
	assertPoolBound(t, p, "0:x")
	assertPoolBound(t, p, "1:new")

	mu.Lock()
	fail = [2]bool{}
	mu.Unlock()
	mustTabList(t, p)
	assertPoolUnknown(t, p, "0:x")
	assertPoolBound(t, p, "1:new")
	if got := poolBoundCount(p); got != 1 {
		t.Fatalf("bound tabs: got %d want 1", got)
	}
}

// tab_focus is a partial observation: it binds what it names and never
// retires anything else.
func TestPool_tabFocusIsNotAuthoritative(t *testing.T) {
	srv, _ := newScriptedDaemon(t, func(method, tab string) (any, error) {
		switch method {
		case protocol.MethodTabList:
			return scriptedList("a", "b"), nil
		case protocol.MethodTabFocus:
			return scriptedTab("c"), nil
		}
		return scriptedTab(tab), nil
	})
	p := newScriptedPool(t, srv)
	mustTabList(t, p)
	if _, err := p.TabFocus(context.Background(), protocol.TabFocusParams{}); err != nil {
		t.Fatal(err)
	}
	for _, tab := range []string{"0:a", "0:b", "0:c"} {
		assertPoolBound(t, p, tab)
	}
	// The next authoritative list retires the focus-only binding.
	mustTabList(t, p)
	assertPoolUnknown(t, p, "0:c")
	assertPoolBound(t, p, "0:a")
}

// gate holds one scripted response in flight: the handler signals entered
// (its snapshot is already decided) and then waits for release.
type gate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// newGate returns a gate that holds its handler until opened. Callers must `defer g.open()`: a deferred open
// runs before the httptest server's Cleanup, which would otherwise wait forever
// on the held handler when an assertion fails.
func newGate() *gate {
	return &gate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) wait() {
	close(g.entered)
	<-g.release
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// A tab created while an older list is in flight must survive that list,
// even though the list's snapshot predates it.
func TestPool_tabNewSurvivesInFlightList(t *testing.T) {
	g := newGate()
	defer g.open()
	var mu sync.Mutex
	lists := 0
	srv, _ := newScriptedDaemon(t, func(method, tab string) (any, error) {
		switch method {
		case protocol.MethodTabNew:
			return scriptedTab("n"), nil
		case protocol.MethodTabFocus:
			return scriptedTab("f"), nil
		case protocol.MethodTabList:
			mu.Lock()
			lists++
			first := lists == 1
			mu.Unlock()
			if first {
				g.wait() // snapshot: no tabs yet
				return scriptedList(), nil
			}
			return scriptedList("n", "f"), nil
		}
		return scriptedTab(tab), nil
	})
	p := newScriptedPool(t, srv)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := p.TabList(ctx, protocol.TabListParams{})
		done <- err
	}()
	awaitGate(t, g, done)
	out, err := p.TabNew(ctx, protocol.TabNewParams{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.TabFocus(ctx, protocol.TabFocusParams{}); err != nil {
		t.Fatal(err)
	}
	g.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertPoolBound(t, p, out.Tab)
	assertPoolBound(t, p, "0:f")
	mustTabList(t, p)
	assertPoolBound(t, p, out.Tab)
	if got := poolBoundCount(p); got != 2 {
		t.Fatalf("bound tabs: got %d want 2", got)
	}
}

// Overlapping lists: a snapshot that started earlier but arrives later must
// not revive a binding the newer list retired.
func TestPool_staleListDoesNotReviveRetiredTab(t *testing.T) {
	g := newGate()
	defer g.open()
	var mu sync.Mutex
	lists := 0
	srv, _ := newScriptedDaemon(t, func(method, tab string) (any, error) {
		if method != protocol.MethodTabList {
			return scriptedTab(tab), nil
		}
		mu.Lock()
		lists++
		n := lists
		mu.Unlock()
		switch n {
		case 1:
			return scriptedList("x", "keep"), nil
		case 2:
			g.wait() // older snapshot still names x
			return scriptedList("x", "keep"), nil
		}
		return scriptedList("keep"), nil
	})
	p := newScriptedPool(t, srv)
	mustTabList(t, p)
	assertPoolBound(t, p, "0:x")
	done := make(chan error, 1)
	go func() {
		_, err := p.TabList(context.Background(), protocol.TabListParams{})
		done <- err
	}()
	awaitGate(t, g, done)
	mustTabList(t, p) // newer list: x is gone
	assertPoolUnknown(t, p, "0:x")
	g.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertPoolUnknown(t, p, "0:x")
	assertPoolBound(t, p, "0:keep")
	if got := poolBoundCount(p); got != 1 {
		t.Fatalf("bound tabs: got %d want 1", got)
	}
}

// Overlapping lists, other order: an older snapshot arriving late must not
// retire what a newer list bound or confirmed in the meantime.
func TestPool_staleListDoesNotRetireNewerObservations(t *testing.T) {
	g := newGate()
	defer g.open()
	var mu sync.Mutex
	lists := 0
	srv, _ := newScriptedDaemon(t, func(method, tab string) (any, error) {
		if method != protocol.MethodTabList {
			return scriptedTab(tab), nil
		}
		mu.Lock()
		lists++
		n := lists
		mu.Unlock()
		switch n {
		case 1:
			return scriptedList("old"), nil
		case 2:
			g.wait() // older snapshot: nothing
			return scriptedList(), nil
		}
		return scriptedList("old", "y"), nil
	})
	p := newScriptedPool(t, srv)
	mustTabList(t, p)
	done := make(chan error, 1)
	go func() {
		_, err := p.TabList(context.Background(), protocol.TabListParams{})
		done <- err
	}()
	awaitGate(t, g, done)
	mustTabList(t, p) // newer list binds y and confirms old
	g.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertPoolBound(t, p, "0:y")
	assertPoolBound(t, p, "0:old")
}

// A successful TabClose or an explicit ForgetTab that overlaps an in-flight
// list / focus must not be undone by that older observation.
func TestPool_closeAndForgetNotRevivedByStaleObservation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		observe string
		drop    func(p *Pool, tab string) error
	}{
		{"close/list", protocol.MethodTabList, func(p *Pool, tab string) error {
			_, err := p.TabClose(context.Background(), protocol.TabCloseParams{Tab: tab})
			return err
		}},
		{"forget/list", protocol.MethodTabList, func(p *Pool, tab string) error {
			p.ForgetTab(tab)
			return nil
		}},
		{"close/focus", protocol.MethodTabFocus, func(p *Pool, tab string) error {
			_, err := p.TabClose(context.Background(), protocol.TabCloseParams{Tab: tab})
			return err
		}},
		{"forget/focus", protocol.MethodTabFocus, func(p *Pool, tab string) error {
			p.ForgetTab(tab)
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGate()
			defer g.open()
			var mu sync.Mutex
			held := false
			srv, _ := newScriptedDaemon(t, func(method, tab string) (any, error) {
				switch method {
				case protocol.MethodTabNew:
					return scriptedTab("x"), nil
				case protocol.MethodTabList, protocol.MethodTabFocus:
					mu.Lock()
					first := !held && method == tc.observe
					if first {
						held = true
					}
					mu.Unlock()
					if first {
						g.wait() // snapshot still names x
					}
					if method == protocol.MethodTabList {
						return scriptedList("x"), nil
					}
					return scriptedTab("x"), nil
				}
				return scriptedTab(tab), nil
			})
			p := newScriptedPool(t, srv)
			ctx := context.Background()
			out, err := p.TabNew(ctx, protocol.TabNewParams{})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				done <- p.Call(ctx, tc.observe, nil, nil)
			}()
			awaitGate(t, g, done)
			if err := tc.drop(p, out.Tab); err != nil {
				t.Fatal(err)
			}
			g.open()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			assertPoolUnknown(t, p, out.Tab)
			if got := poolBoundCount(p); got != 0 {
				t.Fatalf("bound tabs: got %d want 0", got)
			}
			// An observation that starts after the drop is fresh evidence:
			// a forgotten tab the daemon still lists becomes usable again.
			mustTabList(t, p)
			assertPoolBound(t, p, out.Tab)
		})
	}
}

// Concurrent TabNew / TabList / TabFocus against a daemon with a real tab set:
// no created tab may be retired by a racing list (run with -race).
func TestPool_concurrentTabNewAndTabListKeepsCreatedTabs(t *testing.T) {
	type daemonState struct {
		mu   sync.Mutex
		n    int
		tabs []string
	}
	states := [2]*daemonState{{}, {}}
	handler := func(st *daemonState) func(method, tab string) (any, error) {
		return func(method, tab string) (any, error) {
			st.mu.Lock()
			defer st.mu.Unlock()
			switch method {
			case protocol.MethodTabNew:
				st.n++
				id := "t" + strconv.Itoa(st.n)
				st.tabs = append(st.tabs, id)
				return scriptedTab(id), nil
			case protocol.MethodTabList:
				return scriptedList(st.tabs...), nil
			case protocol.MethodTabFocus:
				if len(st.tabs) == 0 {
					return scriptedTab(""), nil
				}
				return scriptedTab(st.tabs[len(st.tabs)-1]), nil
			}
			return scriptedTab(tab), nil
		}
	}
	a, _ := newScriptedDaemon(t, handler(states[0]))
	b, _ := newScriptedDaemon(t, handler(states[1]))
	p := newScriptedPool(t, a, b)
	ctx := context.Background()

	const creators, perCreator, listers = 4, 15, 4
	var wg sync.WaitGroup
	var createdMu sync.Mutex
	var created []string
	stop := make(chan struct{})
	errs := make(chan error, creators*perCreator+listers)
	var listWG sync.WaitGroup
	for range listers {
		listWG.Add(1)
		go func() {
			defer listWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := p.TabList(ctx, protocol.TabListParams{}); err != nil {
					errs <- err
					return
				}
				if _, err := p.TabFocus(ctx, protocol.TabFocusParams{}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perCreator {
				out, err := p.TabNew(ctx, protocol.TabNewParams{})
				if err != nil {
					errs <- err
					return
				}
				// The tab is live on the daemon, so it must stay usable.
				if _, err := p.Eval(ctx, protocol.EvalParams{Tab: out.Tab, Script: "1"}); err != nil {
					errs <- err
					return
				}
				createdMu.Lock()
				created = append(created, out.Tab)
				createdMu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(stop)
	listWG.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(created) != creators*perCreator {
		t.Fatalf("created %d", len(created))
	}
	for _, tab := range created {
		assertPoolBound(t, p, tab)
	}
	if got := poolBoundCount(p); got != len(created) {
		t.Fatalf("bound tabs: got %d want %d", got, len(created))
	}
}

// Removal marks are bounded: they are recorded only while an observation is
// in flight on that backend and dropped when the last one returns, including
// a failed one.
func TestPool_removalMarksDoNotOutliveObservations(t *testing.T) {
	g := newGate()
	defer g.open()
	var mu sync.Mutex
	lists := 0
	srv, _ := newScriptedDaemon(t, func(method, tab string) (any, error) {
		if method != protocol.MethodTabList {
			return scriptedTab(tab), nil
		}
		mu.Lock()
		lists++
		n := lists
		mu.Unlock()
		if n == 2 {
			g.wait()
			return nil, errors.New("down")
		}
		return scriptedList("a", "b", "c"), nil
	})
	p := newScriptedPool(t, srv)
	marks := func() (int, int) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.removed[0]), len(p.observing[0])
	}
	mustTabList(t, p)
	p.ForgetTab("0:a") // nothing in flight: no mark
	if n, obs := marks(); n != 0 || obs != 0 {
		t.Fatalf("idle pool: marks=%d observing=%d", n, obs)
	}
	done := make(chan error, 1)
	go func() {
		_, err := p.TabList(context.Background(), protocol.TabListParams{})
		done <- err
	}()
	awaitGate(t, g, done)
	p.ForgetTab("0:b")
	p.ForgetTab("0:never-bound")
	p.ForgetTab("9:other-pool") // unknown backend key: ignored
	if n, obs := marks(); n != 2 || obs != 1 {
		t.Fatalf("in flight: marks=%d observing=%d", n, obs)
	}
	g.open()
	if err := <-done; err == nil {
		t.Fatal("want list failure")
	}
	if n, obs := marks(); n != 0 || obs != 0 {
		t.Fatalf("after failed list: marks=%d observing=%d", n, obs)
	}
	// The failed list retired nothing.
	assertPoolBound(t, p, "0:c")
	assertPoolUnknown(t, p, "0:b")
}
