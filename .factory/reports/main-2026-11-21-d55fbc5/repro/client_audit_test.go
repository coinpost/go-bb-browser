package daemonclient

// main@d55fbc5 审计复现（只读 overlay）。断言正确行为；FAIL 即缺陷证据。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

func TestAudit_SpreadWithInterleavedHealth(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	p, err := NewPool(NewClient(a.URL), NewClient(b.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 20 {
		if err := p.Health(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := p.TabNew(ctx, protocol.TabNewParams{}); err != nil {
			t.Fatal(err)
		}
	}
	got := []int{sa.tabNewCount(), sb.tabNewCount()}
	t.Logf("tab_new per backend = %v", got)
	if got[0] != 10 || got[1] != 10 {
		t.Fatalf("uneven spread: %v want [10 10]", got)
	}
}

func TestAudit_SpreadWithInterleavedHealth3(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	b, sb := newFakeDaemon(t, "b")
	c, sc := newFakeDaemon(t, "c")
	p, _ := NewPool(NewClient(a.URL), NewClient(b.URL), NewClient(c.URL))
	ctx := context.Background()
	for range 30 {
		_ = p.Live(ctx)
		_ = p.Ready(ctx)
		if _, err := p.TabNew(ctx, protocol.TabNewParams{}); err != nil {
			t.Fatal(err)
		}
	}
	got := []int{sa.tabNewCount(), sb.tabNewCount(), sc.tabNewCount()}
	t.Logf("tab_new per backend = %v", got)
	if got[0] != 10 || got[1] != 10 || got[2] != 10 {
		t.Fatalf("uneven spread: %v want [10 10 10]", got)
	}
}

func (p *Pool) tabsSnapshot() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for k, v := range p.tabs {
		out[k] = v
	}
	return out
}

func TestAudit_TabListHungBackend(t *testing.T) {
	release := make(chan struct{})
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer a.Close()
	defer close(release)
	b, sb := newFakeDaemon(t, "b")
	sb.mu.Lock()
	sb.listTab = "bb11"
	sb.mu.Unlock()
	p, _ := NewPool(NewClient(a.URL), NewClient(b.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := p.TabList(ctx, protocol.TabListParams{})
	t.Logf("TabList => %+v err=%v (B tab_list calls=%d)", out, err, sb.methodCount(protocol.MethodTabList))
	if err != nil || len(out.Tabs) != 1 {
		t.Fatalf("hung backend 0 made whole tab_list fail: err=%v tabs=%v", err, out.Tabs)
	}
}

func TestAudit_TabListDoesNotPruneStale(t *testing.T) {
	a, sa := newFakeDaemon(t, "a")
	p, _ := NewPool(NewClient(a.URL))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		sa.mu.Lock()
		sa.listTab = "t" + string(rune('0'+i))
		sa.mu.Unlock()
		if _, err := p.TabList(ctx, protocol.TabListParams{}); err != nil {
			t.Fatal(err)
		}
	}
	m := p.tabsSnapshot()
	t.Logf("map after 5 lists each showing exactly one tab: %v", m)
	if _, ok := p.ClientForTab("0:t0"); ok {
		t.Fatalf("tab 0:t0 no longer listed by its daemon but still bound (map size %d)", len(m))
	}
}
