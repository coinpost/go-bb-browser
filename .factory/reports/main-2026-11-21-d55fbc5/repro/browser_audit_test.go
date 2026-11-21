package browser

// main@d55fbc5 审计复现（只读 overlay）。断言正确行为；FAIL 即缺陷证据。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/state"
)

type auditGateRecorder struct {
	obs     *state.TabObsStore
	entered chan struct{}
	release chan struct{}
}

func (r *auditGateRecorder) NextSeq() (uint64, bool) { return 1, true }
func (r *auditGateRecorder) RecordNetwork(id target.ID, seq uint64, data json.RawMessage) {
	close(r.entered) // writer goroutine 已取出事件，正要写入 ring
	<-r.release
	r.obs.PushNetwork(id, seq, data)
}
func (r *auditGateRecorder) RecordConsole(target.ID, uint64, json.RawMessage) {}
func (r *auditGateRecorder) RecordError(target.ID, uint64, json.RawMessage)   {}

// INV-6：tab 关闭后其观测缓冲必须清空。daemon 的 clearRemovedTarget 先 ClearTarget 再 ForgetTarget，
// 而 observer 的 writer goroutine 仍可能把已出队的事件写回 → ring 被重建且永不再清理。
func TestAudit_ObservationRingMustStayClearedAfterTabClose(t *testing.T) {
	const tid = target.ID("TAB-CLOSED")
	obsStore := state.NewTabObsStore()
	rec := &auditGateRecorder{obs: obsStore, entered: make(chan struct{}), release: make(chan struct{})}

	s := newSession(context.Background(), func() {})
	ctx, cancel := context.WithCancel(context.Background())
	obs := &tabObserver{
		tid: tid, ctx: ctx, cleanup: &attachCancellation{cancelChild: cancel}, rec: rec,
		queue: make(chan queuedObservation, observationQueueCapacity), ready: make(chan struct{}),
	}
	s.observers = map[target.ID]*tabObserver{tid: obs}
	go obs.runWriter()

	obsStore.PushNetwork(tid, 1, json.RawMessage(`{"kind":"request"}`))
	obs.queue <- queuedObservation{kind: ObservationNetwork, seq: 2, payload: map[string]any{"kind": "loadingFailed"}}
	<-rec.entered

	// internal/daemon/tab_idle.go clearRemovedTarget 的顺序：
	obsStore.ClearTarget(tid) // :49
	s.ForgetTarget(tid)       // :54
	close(rec.release)
	time.Sleep(100 * time.Millisecond)

	events, _, _ := obsStore.QueryNetwork(tid, 0)
	if len(events) != 0 {
		t.Fatalf("BUG: %d network event(s) for a closed target are back in the store after ClearTarget (INV-6); nothing clears them again", len(events))
	}
}

// network_route 失败（attach / Fetch.enable 失败或请求取消）后，规则仍留在内存表里。
func TestAudit_FailedNetworkRouteMustNotLeaveRuleRegistered(t *testing.T) {
	s := newSession(context.Background(), func() {})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 例如客户端断开 / RPC 超时
	const tid = target.ID("TAB-ROUTE")
	err := s.AppendNetworkRouteContext(ctx, tid, NetworkRouteRule{URLPattern: "*track*", Abort: true})
	if err == nil {
		t.Fatal("precondition: expected the route sync to fail")
	}
	t.Logf("AppendNetworkRouteContext error = %v", err)
	if n := s.NetworkRouteCount(tid); n != 0 {
		t.Fatalf("BUG: RPC failed but %d rule(s) stay registered (later calls report it and a later successful sync silently activates it)", n)
	}
}
