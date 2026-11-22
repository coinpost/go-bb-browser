package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Regressions for the mirror image of target_enumeration_test.go: a full
// enumeration that was requested while a tab still existed must not bring that
// tab's id back once the tab was closed in the meantime (INV-6). The snapshot
// is parked after the fake browser took it and applied only after the removal.

var obsClearMethods = []string{
	protocol.MethodNetworkClear,
	protocol.MethodConsoleClear,
	protocol.MethodErrorsClear,
}

func enumPushAllObs(t *testing.T, srv *Server, tid target.ID) {
	t.Helper()
	for _, push := range []func(target.ID, uint64, json.RawMessage){
		srv.obsStore.PushNetwork, srv.obsStore.PushConsole, srv.obsStore.PushError,
	} {
		seq, err := srv.store.NextSeq()
		if err != nil {
			t.Fatal(err)
		}
		push(tid, seq, json.RawMessage(`{"n":1}`))
	}
}

func enumObsCounts(srv *Server, tid target.ID) [3]int {
	n, _, _ := srv.obsStore.QueryNetwork(tid, 0)
	c, _, _ := srv.obsStore.QueryConsole(tid, 0)
	e, _, _ := srv.obsStore.QueryErrors(tid, 0)
	return [3]int{len(n), len(c), len(e)}
}

func enumListedTabs(t *testing.T, result map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	tabs, _ := result["tabs"].([]any)
	for _, row := range tabs {
		m, _ := row.(map[string]any)
		id, _ := m["tab"].(string)
		out[id] = true
	}
	return out
}

func TestStaleEnumerationDoesNotReviveTabClosedMeanwhile(t *testing.T) {
	// A and B share their suffix: nothing may land on B in place of A.
	const idA, idB = fullIDA, fullIDB

	enumerators := []struct {
		name   string
		method string
		params map[string]any
		// wantErr: the enumerating request itself fails (unknown tab id).
		wantErr bool
	}{
		{name: "tab_list", method: protocol.MethodTabList},
		{name: "tab_focus", method: protocol.MethodTabFocus},
		{name: "tab_select other tab", method: protocol.MethodTabSelect, params: map[string]any{"tab": string(idB)}},
		{name: "resolveTab fallback", method: protocol.MethodEval, params: map[string]any{"tab": "feed", "script": "1"}, wantErr: true},
	}
	removals := []struct {
		name   string
		remove func(t *testing.T, srv *Server, conn *enumConn)
	}{
		{name: "tab_close", remove: func(t *testing.T, srv *Server, _ *enumConn) {
			enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})
		}},
		{name: "target destroyed event", remove: func(_ *testing.T, srv *Server, conn *enumConn) {
			conn.removeExternally(idA)
			srv.removeTargetState(idA) // what runTargetSync does for Target.targetDestroyed
		}},
	}

	for _, en := range enumerators {
		for _, rm := range removals {
			t.Run(en.name+"/"+rm.name, func(t *testing.T) {
				conn := newEnumConn(idA, idB)
				srv := newEnumServer(t, conn)
				enumMustRPC(t, srv, protocol.MethodTabList, nil)
				enumMustRPC(t, srv, protocol.MethodTabSelect, map[string]any{"tab": string(idA)})
				enumPushAllObs(t, srv, idA)
				enumPushAllObs(t, srv, idB)

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
				enumAwait(t, entered, en.method+" to take its target snapshot (still listing A)")

				rm.remove(t, srv, conn)
				if srv.tabs.Has(idA) || conn.has(idA) {
					t.Fatal("precondition: A was not removed")
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

				// The late snapshot neither re-registered A nor reported it.
				if _, ok := srv.tabs.Lookup(string(idA)); ok {
					t.Error("stale enumeration re-registered the closed tab id")
				}
				if en.method == protocol.MethodTabList {
					if listed := enumListedTabs(t, got.reply.Result); listed[string(idA)] || !listed[string(idB)] || len(listed) != 1 {
						t.Errorf("stale tab_list returned %v, want only %s", listed, idB)
					}
				}
				for _, key := range []string{"tab", "focus"} {
					if v, _ := got.reply.Result[key].(string); v == string(idA) {
						t.Errorf("%s reported the closed tab as %s", en.method, key)
					}
				}
				if sel := srv.tabs.Selected(); sel == idA {
					t.Error("closed tab is the focused tab again")
				}

				// Every *_clear with the stale complete id is rejected and B's
				// buffers stay as they were.
				for _, method := range obsClearMethods {
					reply, err := enumRPC(srv, method, map[string]any{"tab": string(idA)})
					if err != nil {
						t.Fatal(err)
					}
					if reply.Error == nil {
						t.Errorf("%s accepted the id of a closed tab: %v", method, reply.Result)
					} else if msg, _ := reply.Error["message"].(string); msg != "unknown tab id" {
						t.Errorf("%s error = %v, want unknown tab id", method, reply.Error)
					}
				}
				if got := enumObsCounts(srv, idB); got != [3]int{1, 1, 1} {
					t.Errorf("B observation buffers (network, console, errors) = %v, want [1 1 1]", got)
				}
				if got := enumObsCounts(srv, idA); got != [3]int{} {
					t.Errorf("closed tab kept observation events: %v", got)
				}
				if !conn.has(idB) || !srv.tabs.Has(idB) {
					t.Error("B was closed or unregistered")
				}

				// B is still fully addressable, and a current enumeration is clean.
				for _, method := range obsClearMethods {
					enumMustRPC(t, srv, method, map[string]any{"tab": string(idB)})
				}
				if listed := enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil)); len(listed) != 1 || !listed[string(idB)] {
					t.Errorf("fresh tab_list = %v, want only %s", listed, idB)
				}
			})
		}
	}
}

// The filter is per target and per request: a tab that exists again when a
// later enumeration is requested is listed and usable, and a tab created while
// the stale enumeration was in flight keeps its protection.
func TestStaleEnumerationRemovalFilterKeepsLiveTargets(t *testing.T) {
	const idA, idB = fullIDA, fullIDB
	conn := newEnumConn(idA, idB)
	conn.nextIDs = []target.ID{enumNewID}
	srv := newEnumServer(t, conn)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)

	entered, release := conn.holdNextSnapshot()
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := enumRPC(srv, protocol.MethodTabList, nil)
		done <- err
	}()
	enumAwait(t, entered, "tab_list to take its target snapshot")

	enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})
	created := enumTabNew(t, srv) // not in the parked snapshot
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(enumTestWait):
		t.Fatal("tab_list did not finish")
	}

	if srv.tabs.Has(idA) {
		t.Error("closed tab re-registered")
	}
	if !srv.tabs.Has(target.ID(created)) || !srv.tabs.Has(idB) {
		t.Error("stale enumeration removed a live tab")
	}
	if _, ok := srv.tabIdle.Snapshot()[target.ID(created)]; !ok {
		t.Error("tab created meanwhile lost its managed idle tracking")
	}

	// The browser shows a page with A's id again (ids are opaque: the daemon
	// promises nothing about reuse). An enumeration requested now is current
	// for it, so the id resolves again.
	conn.mu.Lock()
	conn.pages = append(conn.pages, &target.Info{TargetID: idA, Type: "page"})
	conn.mu.Unlock()
	listed := enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil))
	if len(listed) != 3 || !listed[string(idA)] {
		t.Errorf("current tab_list = %v, want A, B and the new tab", listed)
	}
	enumMustRPC(t, srv, protocol.MethodNetworkClear, map[string]any{"tab": string(idA)})
}

// Target.closeTarget is confirmed before the browser destroys the target, so an
// enumeration requested after tab_close(A) returned can still list A. When the
// destroyed notification for A arrives before that reply is applied, the reply
// predates the notification and must not bring A back, although the registry
// had already removed A once (on the confirmed close) before the request.
func TestEnumerationAfterCloseDoesNotReviveTabDestroyedMeanwhile(t *testing.T) {
	// A and B share their suffix: nothing may land on B in place of A.
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
	for _, en := range enumerators {
		t.Run(en.name, func(t *testing.T) {
			conn := newEnumConn(idA, idB)
			conn.closeLingers = true
			srv := newEnumServer(t, conn)
			enumMustRPC(t, srv, protocol.MethodTabList, nil)
			enumMustRPC(t, srv, protocol.MethodTabSelect, map[string]any{"tab": string(idA)})
			enumPushAllObs(t, srv, idA)
			enumPushAllObs(t, srv, idB)

			// 1. tab_close(A) is confirmed; the browser still lists A.
			enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})
			if srv.tabs.Has(idA) || !conn.has(idA) {
				t.Fatal("precondition: A must be unregistered but still listed by the browser")
			}

			// 2. An enumeration requested now takes a snapshot that contains A.
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
			enumAwait(t, entered, en.method+" to take its target snapshot (still listing A)")

			// 3. Target.targetDestroyed(A) arrives (what runTargetSync does for it).
			conn.removeExternally(idA)
			srv.removeTargetState(idA)

			// 4. The parked snapshot is applied.
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

			if _, ok := srv.tabs.Lookup(string(idA)); ok {
				t.Error("enumeration re-registered the id of a closed and destroyed tab")
			}
			if en.method == protocol.MethodTabList {
				if listed := enumListedTabs(t, got.reply.Result); listed[string(idA)] || !listed[string(idB)] || len(listed) != 1 {
					t.Errorf("tab_list returned %v, want only %s", listed, idB)
				}
			}
			for _, key := range []string{"tab", "focus"} {
				if v, _ := got.reply.Result[key].(string); v == string(idA) {
					t.Errorf("%s reported the destroyed tab as %s", en.method, key)
				}
			}
			if sel := srv.tabs.Selected(); sel == idA {
				t.Error("destroyed tab is the focused tab again")
			}

			for _, method := range obsClearMethods {
				reply, err := enumRPC(srv, method, map[string]any{"tab": string(idA)})
				if err != nil {
					t.Fatal(err)
				}
				if reply.Error == nil {
					t.Errorf("%s accepted the id of a closed tab: %v", method, reply.Result)
				} else if msg, _ := reply.Error["message"].(string); msg != "unknown tab id" {
					t.Errorf("%s error = %v, want unknown tab id", method, reply.Error)
				}
			}
			if got := enumObsCounts(srv, idB); got != [3]int{1, 1, 1} {
				t.Errorf("B observation buffers (network, console, errors) = %v, want [1 1 1]", got)
			}
			if got := enumObsCounts(srv, idA); got != [3]int{} {
				t.Errorf("closed tab kept observation events: %v", got)
			}
			if !conn.has(idB) || !srv.tabs.Has(idB) {
				t.Error("B was closed or unregistered")
			}

			// B is still fully addressable, and a current enumeration is clean.
			for _, method := range obsClearMethods {
				enumMustRPC(t, srv, method, map[string]any{"tab": string(idB)})
			}
			if listed := enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil)); len(listed) != 1 || !listed[string(idB)] {
				t.Errorf("fresh tab_list = %v, want only %s", listed, idB)
			}
		})
	}
}

// Without the destroyed notification nothing vouches against the snapshot: a
// target the browser still lists after a confirmed close resolves again, and the
// notification, whenever it arrives, removes it for good.
func TestEnumerationAfterCloseListsLingeringTargetUntilDestroyed(t *testing.T) {
	const idA, idB = fullIDA, fullIDB
	conn := newEnumConn(idA, idB)
	conn.closeLingers = true
	srv := newEnumServer(t, conn)
	enumMustRPC(t, srv, protocol.MethodTabList, nil)
	enumMustRPC(t, srv, protocol.MethodTabClose, map[string]any{"tab": string(idA)})

	if listed := enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil)); len(listed) != 2 || !listed[string(idA)] {
		t.Fatalf("tab_list = %v, want the lingering A and B", listed)
	}
	conn.removeExternally(idA)
	srv.removeTargetState(idA)
	if srv.tabs.Has(idA) || !srv.tabs.Has(idB) {
		t.Fatal("destroyed notification did not remove exactly A")
	}
	if listed := enumListedTabs(t, enumMustRPC(t, srv, protocol.MethodTabList, nil)); len(listed) != 1 || !listed[string(idB)] {
		t.Errorf("tab_list after destruction = %v, want only %s", listed, idB)
	}
}
