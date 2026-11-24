package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/store"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Regressions for replies that arrive after the identity they name was
// invalidated (INV-6): a late Target.createTarget reply for a tab that was
// already closed, and an enumeration reply older than an enumeration that
// already found the tab absent. Neither may make the complete id of the dead
// tab A resolve again, in the registry, in idle tracking or in rpc.jsonl, and
// nothing may land on B, which shares A's suffix.

// createHoldConn parks the Target.createTarget reply after the page exists in
// the browser, which is when other requests can already see and close it.
type createHoldConn struct {
	*enumConn
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newCreateHoldConn(conn *enumConn) *createHoldConn {
	return &createHoldConn{enumConn: conn, entered: make(chan struct{}), release: make(chan struct{})}
}

func (c *createHoldConn) CreatePageTarget(url string, silent bool) (target.ID, error) {
	id, err := c.enumConn.CreatePageTarget(url, silent)
	first := false
	c.once.Do(func() { first = true })
	if first {
		close(c.entered)
		<-c.release
	}
	return id, err
}

func (c *enumConn) addExternally(id target.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pages = append(c.pages, &target.Info{TargetID: id, Type: "page"})
}

// assertStaleIDRejected: the three clear methods (which need no CDP round trip
// and so depend on the registry alone) refuse the id, and B is untouched.
func assertStaleIDRejected(t *testing.T, srv *Server, conn *enumConn, idA, idB target.ID, wantB [3]int) {
	t.Helper()
	if _, ok := srv.tabs.Lookup(string(idA)); ok {
		t.Errorf("the id of the closed tab A resolves again")
	}
	if srv.tabs.Selected() == idA {
		t.Errorf("the closed tab A is the focused tab")
	}
	for _, method := range obsClearMethods {
		reply, err := enumRPC(srv, method, map[string]any{"tab": string(idA)})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Error == nil {
			t.Errorf("%s accepted the id of the closed tab A", method)
		} else if msg, _ := reply.Error["message"].(string); msg != "unknown tab id" {
			t.Errorf("%s error = %v, want unknown tab id", method, reply.Error)
		}
	}
	if !conn.has(idB) || !srv.tabs.Has(idB) {
		t.Errorf("B was closed or unregistered")
	}
	if got := enumObsCounts(srv, idB); got != wantB {
		t.Errorf("B observation buffers = %v, want %v", got, wantB)
	}
}

// assertNotManaged: A has no idle tracking, and replaying rpc.jsonl (as a
// restarted daemon does) does not restore it. Closes srv's audit log.
func assertNotManagedAfterReplay(t *testing.T, srv *Server, dir string, gone target.ID, kept ...target.ID) {
	t.Helper()
	if _, ok := srv.tabIdle.Snapshot()[gone]; ok {
		t.Errorf("closed tab %s is idle-tracked", gone)
	}
	if managedTargetSet(srv.store.ManagedTabs())[gone] {
		t.Errorf("closed tab %s is in the managed set", gone)
	}
	drainRPCLog(t, srv)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replayed := managedTargetSet(reopened.ManagedTabs())
	if replayed[gone] {
		t.Errorf("replaying rpc.jsonl restores the closed tab %s as managed", gone)
	}
	for _, id := range kept {
		if !replayed[id] {
			t.Errorf("replaying rpc.jsonl lost the managed tab %s", id)
		}
	}
}

// tab_new(A) is parked on its reply. Meanwhile A is removed in every way a tab
// can be; then the reply is released.
func TestLateTabNewReplyDoesNotReviveTabClosedMeanwhile(t *testing.T) {
	const idA, idB = fullIDA, fullIDB

	removals := []struct {
		name   string
		remove func(t *testing.T, srv *Server, conn *enumConn)
	}{
		{
			// Another caller lists A, closes it, and targetDestroyed is processed.
			name: "listed, tab_close, destroyed event",
			remove: func(t *testing.T, srv *Server, conn *enumConn) {
				if !enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil))[string(idA)] {
					t.Fatal("precondition: tab_list must list the tab being created")
				}
				enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})
				srv.removeTargetState(idA)
			},
		},
		{
			name: "listed, tab_close only",
			remove: func(t *testing.T, srv *Server, conn *enumConn) {
				enumMustRPC(t, srv, protocol.MethodTabList, nil)
				enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})
			},
		},
		{
			// Closed in the browser before the daemon ever registered it.
			name: "destroyed event before any registration",
			remove: func(t *testing.T, srv *Server, conn *enumConn) {
				conn.removeExternally(idA)
				srv.removeTargetState(idA)
			},
		},
		{
			// targetCreated registered it, then a current enumeration dropped it.
			name: "registered by event, removed by enumeration",
			remove: func(t *testing.T, srv *Server, conn *enumConn) {
				srv.tabs.RegisterPageTarget(idA)
				conn.removeExternally(idA)
				if enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil))[string(idA)] {
					t.Fatal("precondition: the enumeration must not list A")
				}
			},
		},
		{
			// So many removals overtook the creation that A's own removal is no
			// longer remembered: a current enumeration decides.
			name: "removal forgotten",
			remove: func(t *testing.T, srv *Server, conn *enumConn) {
				enumMustRPC(t, srv, protocol.MethodTabList, nil)
				enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})
				srv.removeTargetState(idA)
				for i := 0; i < 1025; i++ {
					srv.removeTargetState(target.ID(fmt.Sprintf("gone-%d", i)))
				}
			},
		},
	}
	for _, rm := range removals {
		t.Run(rm.name, func(t *testing.T) {
			dir := t.TempDir()
			conn := newEnumConn(idB)
			conn.nextIDs = []target.ID{idA}
			hold := newCreateHoldConn(conn)
			srv := newReconcileRaceServer(t, conn, dir, 0)
			srv.tabHook = hold
			enumMustRPC(t, srv, protocol.MethodTabList, nil)
			enumMustRPC(t, srv, protocol.MethodTabSelect, map[string]any{"tab": string(idB)})
			enumPushAllObs(t, srv, idB)
			wantB := enumObsCounts(srv, idB)

			type outcome struct {
				reply enumRPCReply
				err   error
			}
			done := make(chan outcome, 1)
			go func() {
				reply, err := enumRPC(srv, protocol.MethodTabNew, nil)
				done <- outcome{reply, err}
			}()
			enumAwait(t, hold.entered, "tab_new to create the page (reply parked)")

			rm.remove(t, srv, conn)
			if conn.has(idA) || srv.tabs.Has(idA) {
				t.Fatal("precondition: A must be closed and unregistered before the reply")
			}

			close(hold.release)
			var got outcome
			select {
			case got = <-done:
			case <-time.After(enumTestWait):
				t.Fatal("tab_new did not finish")
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.reply.Error == nil {
				t.Errorf("tab_new reported the already closed tab as created: %v", got.reply.Result)
			} else if msg, _ := got.reply.Error["message"].(string); msg != "tab closed before tab_new completed" {
				t.Errorf("tab_new error = %v", got.reply.Error)
			}
			if srv.tabs.Selected() != idB {
				t.Errorf("focus = %q, want B", srv.tabs.Selected())
			}
			assertStaleIDRejected(t, srv, conn, idA, idB, wantB)
			assertNotManagedAfterReplay(t, srv, dir, idA)
		})
	}
}

// The ordering must not cost a tab that is still open: listed (and so already
// registered) while its tab_new reply is parked, or overtaken by more removals
// of other tabs than the registry remembers, it is created, managed and durable.
func TestLateTabNewReplyKeepsTabStillOpen(t *testing.T) {
	const idA, idB = fullIDA, fullIDB

	cases := []struct {
		name      string
		meanwhile func(t *testing.T, srv *Server, conn *enumConn)
	}{
		{name: "nothing happened", meanwhile: func(*testing.T, *Server, *enumConn) {}},
		{
			name: "listed by another caller",
			meanwhile: func(t *testing.T, srv *Server, conn *enumConn) {
				enumMustRPC(t, srv, protocol.MethodTabList, nil)
				if !srv.tabs.Has(idA) {
					t.Fatal("precondition: tab_list registers the tab being created")
				}
			},
		},
		{
			name: "removal memory overflowed by other tabs",
			meanwhile: func(t *testing.T, srv *Server, conn *enumConn) {
				for i := 0; i < 1025; i++ {
					srv.removeTargetState(target.ID(fmt.Sprintf("gone-%d", i)))
				}
			},
		},
		{
			// A stale list that predates the creation says nothing about A.
			name: "enumeration without the new tab applied",
			meanwhile: func(t *testing.T, srv *Server, conn *enumConn) {
				srv.applyEnumeration(nil, srv.tabs.OpenWindow(), []*target.Info{{TargetID: idB, Type: "page"}})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			conn := newEnumConn(idB)
			conn.nextIDs = []target.ID{idA}
			hold := newCreateHoldConn(conn)
			srv := newReconcileRaceServer(t, conn, dir, 0)
			srv.tabHook = hold
			enumMustRPC(t, srv, protocol.MethodTabList, nil)

			done := make(chan enumRPCReply, 1)
			go func() {
				reply, _ := enumRPC(srv, protocol.MethodTabNew, nil)
				done <- reply
			}()
			enumAwait(t, hold.entered, "tab_new to create the page (reply parked)")
			tc.meanwhile(t, srv, conn)
			close(hold.release)
			var reply enumRPCReply
			select {
			case reply = <-done:
			case <-time.After(enumTestWait):
				t.Fatal("tab_new did not finish")
			}
			if tab, _ := reply.Result["tab"].(string); reply.Error != nil || tab != string(idA) {
				t.Fatalf("tab_new = %+v, want tab %s", reply, idA)
			}
			if !srv.tabs.Has(idA) || srv.tabs.Selected() != idA {
				t.Errorf("new tab registered=%v focus=%q", srv.tabs.Has(idA), srv.tabs.Selected())
			}
			if _, ok := srv.tabIdle.Snapshot()[idA]; !ok {
				t.Errorf("new tab has no idle tracking")
			}
			// A later close still ends the lifecycle everywhere.
			drainRPCLog(t, srv)
			if !managedTargetSet(srv.store.ManagedTabs())[idA] {
				t.Errorf("new tab is not in the managed set")
			}
			enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})
			assertNotManagedAfterReplay(t, srv, dir, idA)
		})
	}
}

// close(A) is confirmed while the browser still lists A. An enumeration takes a
// snapshot with A and B and is parked. A disappears (no destroyed event is
// consumed), a newer tab_list confirms only B, and then the parked reply is
// applied. Every RPC entry point that enumerates is exercised as the old reply.
func TestOlderEnumerationDoesNotReviveTabNewerEnumerationFoundAbsent(t *testing.T) {
	const idA, idB = fullIDA, fullIDB

	enumerators := []struct {
		name    string
		method  string
		params  map[string]any
		wantErr bool
	}{
		{name: "tab_list", method: protocol.MethodTabList},
		{name: "tab_focus", method: protocol.MethodTabFocus},
		{name: "tab_select other tab", method: protocol.MethodTabSelect, params: map[string]any{"tab": string(idB)}},
		{name: "resolveTab fallback", method: protocol.MethodEval, params: map[string]any{"tab": "feed", "script": "1"}, wantErr: true},
	}
	setups := []struct {
		name string
		// prepare leaves A unregistered in the daemon but listed by the browser.
		prepare func(t *testing.T, srv *Server, conn *enumConn)
	}{
		{
			name: "A closed by tab_close",
			prepare: func(t *testing.T, srv *Server, conn *enumConn) {
				conn.addExternally(idA)
				conn.closeLingers = true
				enumMustRPC(t, srv, protocol.MethodTabList, nil)
				enumMustRPC(t, srv, protocol.MethodTabSelect, map[string]any{"tab": string(idA)})
				enumPushAllObs(t, srv, idA)
				enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})
			},
		},
		{
			// Opened in the browser after the daemon last looked: there is no
			// registration and no removal of A to remember at all.
			name: "A never registered",
			prepare: func(t *testing.T, srv *Server, conn *enumConn) {
				enumMustRPC(t, srv, protocol.MethodTabList, nil)
				conn.addExternally(idA)
			},
		},
	}
	for _, setup := range setups {
		for _, en := range enumerators {
			t.Run(setup.name+"/"+en.name, func(t *testing.T) {
				conn := newEnumConn(idB)
				srv := newEnumServer(t, conn)
				setup.prepare(t, srv, conn)
				enumPushAllObs(t, srv, idB)
				wantB := enumObsCounts(srv, idB)
				if srv.tabs.Has(idA) || !conn.has(idA) {
					t.Fatal("precondition: A must be unregistered but still listed by the browser")
				}

				entered, release := conn.holdNextSnapshot()
				defer release()
				type outcome struct {
					reply enumRPCReply
					err   error
				}
				done := make(chan outcome, 1)
				go func() {
					reply, err := enumRPC(srv, en.method, en.params)
					done <- outcome{reply, err}
				}()
				enumAwait(t, entered, en.method+" to take its target snapshot (listing A and B)")

				conn.removeExternally(idA)
				fresh := enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil))
				if len(fresh) != 1 || !fresh[string(idB)] || srv.tabs.Has(idA) {
					t.Fatalf("newer tab_list = %v, want only B", fresh)
				}

				release()
				var got outcome
				select {
				case got = <-done:
				case <-time.After(enumTestWait):
					t.Fatalf("%s did not finish", en.method)
				}
				if got.err != nil {
					t.Fatal(got.err)
				}
				if en.wantErr != (got.reply.Error != nil) {
					t.Fatalf("%s error = %v, wantErr %v", en.method, got.reply.Error, en.wantErr)
				}
				if en.method == protocol.MethodTabList {
					if listed := enumListedTabs(t, got.reply.Result); listed[string(idA)] || !listed[string(idB)] || len(listed) != 1 {
						t.Errorf("older tab_list returned %v, want only B", listed)
					}
				}
				if tab, _ := got.reply.Result["tab"].(string); tab == string(idA) {
					t.Errorf("%s reported the closed tab A as its tab", en.method)
				}
				assertStaleIDRejected(t, srv, conn, idA, idB, wantB)
				if _, ok := srv.tabIdle.Snapshot()[idA]; ok {
					t.Errorf("A is idle-tracked")
				}

				// Not a tombstone: when the browser really has such a target
				// again, a current enumeration lists it.
				conn.addExternally(idA)
				if again := enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil)); !again[string(idA)] || !again[string(idB)] {
					t.Errorf("current tab_list = %v, want A and B", again)
				}
			})
		}
	}
}

// Unsynchronized tab_new requests race a closer that lists, closes and reports
// destroyed every tab it sees. Whatever the interleaving, a tab that is closed
// and destroyed ends with no registration, no idle tracking and no persisted
// managed record, and every tab_new either reports a tab or says it was closed.
func TestConcurrentTabNewAndCloseLeaveNoManagedResidue(t *testing.T) {
	const idB = fullIDB
	const creators, perCreator = 4, 25
	dir := t.TempDir()
	conn := newEnumConn(idB)
	srv := newReconcileRaceServer(t, conn, dir, 0)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)

	closeAll := func() error {
		reply, err := enumRPC(srv, protocol.MethodTabList, nil)
		if err != nil {
			return err
		}
		tabs, _ := reply.Result["tabs"].([]any)
		for _, row := range tabs {
			m, _ := row.(map[string]any)
			tab, _ := m["tab"].(string)
			if tab == "" || tab == string(idB) {
				continue
			}
			// The reply does not matter: another pass may already have closed it.
			if _, err := enumRPC(srv, protocol.MethodTabClose, map[string]any{"tab": tab}); err != nil {
				return err
			}
			srv.removeTargetState(target.ID(tab)) // targetDestroyed
		}
		return nil
	}

	var creatorsWG sync.WaitGroup
	errs := make(chan error, creators*perCreator+1)
	for c := 0; c < creators; c++ {
		creatorsWG.Add(1)
		go func() {
			defer creatorsWG.Done()
			for i := 0; i < perCreator; i++ {
				reply, err := enumRPC(srv, protocol.MethodTabNew, map[string]any{"silent": true})
				if err != nil {
					errs <- err
					return
				}
				if reply.Error != nil {
					if msg, _ := reply.Error["message"].(string); msg != "tab closed before tab_new completed" {
						errs <- fmt.Errorf("tab_new: %v", reply.Error)
						return
					}
				}
			}
		}()
	}
	stop := make(chan struct{})
	closerDone := make(chan struct{})
	go func() {
		defer close(closerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := closeAll(); err != nil {
				errs <- err
				return
			}
		}
	}()
	creatorsWG.Wait()
	close(stop)
	enumAwait(t, closerDone, "closer")
	if err := closeAll(); err != nil { // every tab_new has returned: nothing can appear after this
		t.Fatal(err)
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if listed := enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil)); len(listed) != 1 || !listed[string(idB)] {
		t.Errorf("tabs left registered: %v", listed)
	}
	if idle := srv.tabIdle.Snapshot(); len(idle) != 0 {
		t.Errorf("closed tabs left in idle tracking: %v", idle)
	}
	if managed := srv.store.ManagedTabs(); len(managed) != 0 {
		t.Errorf("closed tabs left in the managed set: %v", managed)
	}
	drainRPCLog(t, srv)
	if err := srv.closeAudit(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(store.OpenConfig{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if replayed := reopened.ManagedTabs(); len(replayed) != 0 {
		t.Errorf("replaying rpc.jsonl restores %d closed tabs", len(replayed))
	}
}
