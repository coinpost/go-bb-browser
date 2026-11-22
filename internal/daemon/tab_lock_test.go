package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

func tabLockEntryCount(srv *Server) int {
	srv.tabMuOps.Lock()
	defer srv.tabMuOps.Unlock()
	return len(srv.tabCDPLocks)
}

// tabLockState reports the entry currently mapped for short and its reference count.
func tabLockState(srv *Server, short string) (*tabLockEntry, int) {
	srv.tabMuOps.Lock()
	defer srv.tabMuOps.Unlock()
	ent := srv.tabCDPLocks[short]
	if ent == nil {
		return nil, 0
	}
	return ent, ent.refs
}

func waitTabLockRefs(t *testing.T, srv *Server, short string, want int) *tabLockEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ent, refs := tabLockState(srv, short)
		if refs == want {
			return ent
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock refs for %q = %d, want %d", short, refs, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// tabRPCParams returns params for the representative tab-scoped methods used below.
func tabRPCParams(method, tab string) map[string]any {
	params := map[string]any{"tab": tab}
	switch method {
	case protocol.MethodGoto:
		params["url"] = "https://example.com"
	case protocol.MethodEval:
		params["script"] = "1"
	}
	return params
}

var tabLockRPCMethods = []string{
	protocol.MethodGoto,
	protocol.MethodEval,
	protocol.MethodNetwork,
	protocol.MethodTabClose,
}

func expectUnknownTabError(t *testing.T, srv *Server, method, tab string) {
	t.Helper()
	rec := postRPC(t, srv, rpcReq(method, tabRPCParams(method, tab), 1))
	var resp protocol.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s %q: decode response: %v; body %s", method, tab, err, rec.Body.String())
	}
	if resp.Error == nil {
		t.Fatalf("%s %q: expected error, got %s", method, tab, rec.Body.String())
	}
	if resp.Error.Code != protocol.CodeInvalidParams || !strings.Contains(resp.Error.Message, "unknown tab id") {
		t.Fatalf("%s %q: error = %d %q, want %d unknown tab id", method, tab, resp.Error.Code, resp.Error.Message, protocol.CodeInvalidParams)
	}
}

func TestUnknownTabRPCsLeaveNoLockEntries(t *testing.T) {
	srv := newIdleTestServer(&fakeConn{infos: []*target.Info{
		{TargetID: "ABCDEF123456", Type: "page", Title: "t", URL: "https://ex"},
	}}, time.Minute)

	const distinct = 300
	for i := 0; i < distinct; i++ {
		method := tabLockRPCMethods[i%len(tabLockRPCMethods)]
		expectUnknownTabError(t, srv, method, fmt.Sprintf("unknown-%03d", i))
	}
	// A client controls the key bytes too, not just how many keys there are.
	expectUnknownTabError(t, srv, protocol.MethodEval, strings.Repeat("k", 64<<10))

	if got := tabLockEntryCount(srv); got != 0 {
		t.Fatalf("lock entries after %d unknown tab requests = %d, want 0", distinct+1, got)
	}
}

func TestClosedTabRetriesLeaveNoLockEntries(t *testing.T) {
	srv := newIdleTestServer(&fakeConn{}, time.Minute)

	var shorts []string
	for i := 0; i < 3; i++ {
		rec := postRPC(t, srv, rpcReq(protocol.MethodTabNew, map[string]any{"url": "https://example.com"}, 1))
		var env struct {
			Result struct {
				Tab string `json:"tab"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Result.Tab == "" {
			t.Fatalf("tab_new: err %v body %s", err, rec.Body.String())
		}
		shorts = append(shorts, env.Result.Tab)
	}
	for _, short := range shorts {
		rec := postRPC(t, srv, rpcReq(protocol.MethodTabClose, map[string]any{"tab": short}, 2))
		var resp protocol.Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error != nil {
			t.Fatalf("tab_close %q: err %v body %s", short, err, rec.Body.String())
		}
	}
	if got := tabLockEntryCount(srv); got != 0 {
		t.Fatalf("lock entries after closing every tab = %d, want 0", got)
	}

	for round := 0; round < 2; round++ {
		for _, short := range shorts {
			for _, method := range tabLockRPCMethods {
				expectUnknownTabError(t, srv, method, short)
			}
		}
	}
	if got := tabLockEntryCount(srv); got != 0 {
		t.Fatalf("lock entries after retrying closed tab ids = %d, want 0", got)
	}
}

func TestLockTabReleasesEntryAfterLastReference(t *testing.T) {
	tid := target.ID("ABCDEF123456")
	srv := newIdleTestServer(&fakeConn{infos: []*target.Info{
		{TargetID: tid, Type: "page", Title: "t", URL: "https://ex"},
	}}, time.Minute)
	valid := srv.tabs.RegisterPageTarget(tid)

	// The empty key is a no-op lock and must not touch the map, even on first use.
	unlock, ok := srv.lockTab(context.Background(), "")
	if !ok {
		t.Fatal("empty key lock failed")
	}
	unlock()
	if got := tabLockEntryCount(srv); got != 0 {
		t.Fatalf("empty key created %d lock entries", got)
	}

	for _, tc := range []struct{ name, key string }{
		{"valid tab", valid},
		{"unknown tab", "no-such-tab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unlock, ok := srv.lockTab(context.Background(), tc.key)
			if !ok {
				t.Fatal("initial lock failed")
			}
			held := waitTabLockRefs(t, srv, tc.key, 1)

			// A waiter that gives up drops only its own reference: the holder's
			// entry stays mapped so later callers still serialize behind it.
			ctx, cancel := context.WithCancel(context.Background())
			waiterDone := make(chan bool, 1)
			go func() {
				_, ok := srv.lockTab(ctx, tc.key)
				waiterDone <- ok
			}()
			waitTabLockRefs(t, srv, tc.key, 2)
			cancel()
			select {
			case ok := <-waiterDone:
				if ok {
					t.Fatal("cancelled waiter acquired a held lock")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled waiter did not return")
			}
			if ent, refs := tabLockState(srv, tc.key); ent != held || refs != 1 {
				t.Fatalf("after waiter cancellation: entry %p refs %d, want %p refs 1", ent, refs, held)
			}

			unlock()
			if ent, _ := tabLockState(srv, tc.key); ent != nil {
				t.Fatal("lock entry retained after last reference was released")
			}

			// The key stays usable: a fresh entry serializes again and is reclaimed.
			unlock, ok = srv.lockTab(context.Background(), tc.key)
			if !ok {
				t.Fatal("reacquire failed")
			}
			expired, cancelExpired := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancelExpired()
			if _, ok := srv.lockTab(expired, tc.key); ok {
				t.Fatal("second holder acquired a held lock")
			}
			unlock()
			if got := tabLockEntryCount(srv); got != 0 {
				t.Fatalf("lock entries after reacquire = %d, want 0", got)
			}
		})
	}
}

func TestLockTabSerializesConcurrentSameKey(t *testing.T) {
	srv := newIdleTestServer(&fakeConn{}, time.Minute)

	const workers, rounds = 16, 50
	var inside, violations atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Two keys: entries are created and reclaimed constantly while
			// other goroutines are racing to take a reference on them.
			key := fmt.Sprintf("key-%d", w%2)
			other := fmt.Sprintf("solo-%d", w)
			for i := 0; i < rounds; i++ {
				unlock, ok := srv.lockTab(context.Background(), key)
				if !ok {
					violations.Add(1)
					return
				}
				if w%2 == 0 {
					if inside.Add(1) != 1 {
						violations.Add(1)
					}
					inside.Add(-1)
				}
				unlock()
				unlockOther, ok := srv.lockTab(context.Background(), other)
				if !ok {
					violations.Add(1)
					return
				}
				unlockOther()
			}
		}(w)
	}
	wg.Wait()
	if got := violations.Load(); got != 0 {
		t.Fatalf("%d same-key critical sections overlapped or failed to lock", got)
	}
	if got := tabLockEntryCount(srv); got != 0 {
		t.Fatalf("lock entries after all holders drained = %d, want 0", got)
	}
}

// Target removal while an operation holds the lock must not open a second lock
// generation for the same short id: the next caller waits on the same entry.
func TestRemovedTargetLockIsSharedUntilReferencesDrain(t *testing.T) {
	tid := target.ID("ABCDEF123456")
	srv := newIdleTestServer(&fakeConn{}, time.Minute)
	short := srv.tabs.RegisterPageTarget(tid)

	unlock, ok := srv.lockTab(context.Background(), short)
	if !ok {
		t.Fatal("initial lock failed")
	}
	held := waitTabLockRefs(t, srv, short, 1)
	srv.removeTargetState(tid)
	if ent, refs := tabLockState(srv, short); ent != held || refs != 1 {
		t.Fatalf("after removal: entry %p refs %d, want held entry %p refs 1", ent, refs, held)
	}

	acquired := make(chan func(), 1)
	go func() {
		unlock, ok := srv.lockTab(context.Background(), short)
		if !ok {
			acquired <- nil
			return
		}
		acquired <- unlock
	}()
	if ent := waitTabLockRefs(t, srv, short, 2); ent != held {
		t.Fatal("reacquire after removal created a second lock generation")
	}
	select {
	case <-acquired:
		t.Fatal("reacquire overlapped the operation still holding the removed tab's lock")
	case <-time.After(20 * time.Millisecond):
	}

	unlock()
	select {
	case unlock2 := <-acquired:
		if unlock2 == nil {
			t.Fatal("waiter failed to acquire after release")
		}
		if ent, refs := tabLockState(srv, short); ent != held || refs != 1 {
			t.Fatalf("waiter holds entry %p refs %d, want %p refs 1", ent, refs, held)
		}
		unlock2()
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not acquire after release")
	}
	if got := tabLockEntryCount(srv); got != 0 {
		t.Fatalf("lock entries after references drained = %d, want 0", got)
	}
}
