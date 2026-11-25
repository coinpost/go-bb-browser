package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/browser"
	"github.com/coinpost/go-bb-browser/internal/state"
	"github.com/coinpost/go-bb-browser/internal/store"
	"github.com/coinpost/go-bb-browser/internal/timeout"
	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSONRPCBytes(ctx, w, protocol.NullID, func() ([]byte, error) {
			return protocol.MarshalErrorResponse(protocol.NullID, protocol.CodeParseError, "Parse error", nil)
		})
		return
	}

	var req protocol.Request
	if err := json.Unmarshal(rawBody, &req); err != nil {
		s.writeJSONRPCBytes(ctx, w, protocol.NullID, func() ([]byte, error) {
			return protocol.MarshalErrorResponse(protocol.NullID, protocol.CodeParseError, "Parse error", nil)
		})
		return
	}

	if req.JSONRPC != "2.0" {
		id := req.ID
		if !protocol.RequestHasID(id) {
			id = protocol.NullID
		}
		s.writeJSONRPCBytes(ctx, w, id, func() ([]byte, error) {
			return protocol.MarshalErrorResponse(id, protocol.CodeInvalidRequest, "invalid jsonrpc version", nil)
		})
		return
	}

	if !protocol.RequestHasID(req.ID) {
		s.writeJSONRPCBytes(ctx, w, protocol.NullID, func() ([]byte, error) {
			return protocol.MarshalErrorResponse(protocol.NullID, protocol.CodeInvalidRequest, "missing id", nil)
		})
		return
	}

	id := req.ID
	params := protocol.NormalizeParams(req.Params)
	method := strings.TrimSpace(req.Method)

	if !protocol.IsDispatchedMethod(method) {
		if method == "" {
			s.rpcErr(ctx, w, id, protocol.CodeInvalidRequest, "missing method", nil)
			return
		}
		s.rpcErr(ctx, w, id, protocol.CodeMethodNotFound, "method not found", &protocol.ErrData{
			Error:  "unknown method",
			Method: method,
		})
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout.Operation)
	defer cancel()

	ctx = contextWithAudit(ctx, &auditMeta{
		action:   method,
		body:     json.RawMessage(rawBody),
		senderIP: store.ClientIP(r),
		at:       time.Now().UTC(),
	})
	// Cached supervisor-state check only: this never sends a CDP command.
	if err := s.ensureBrowserSession(ctx); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser not connected", &protocol.ErrData{
			Error: "browser session unavailable",
			Hint:  err.Error(),
		})
		return
	}

	switch method {
	case protocol.MethodTabList:
		s.handleTabList(ctx, w, id, params)
	case protocol.MethodTabFocus:
		s.handleTabFocus(ctx, w, id, params)
	case protocol.MethodTabSelect:
		s.handleTabSelect(ctx, w, id, params)
	case protocol.MethodTabNew:
		s.handleTabNew(ctx, w, id, params)
	case protocol.MethodGoto:
		s.handleGoto(ctx, w, id, params)
	case protocol.MethodReload:
		s.handleReload(ctx, w, id, params)
	case protocol.MethodTabClose:
		s.handleTabClose(ctx, w, id, params)
	case protocol.MethodScreenshot:
		s.handleScreenshot(ctx, w, id, params)
	case protocol.MethodEval:
		s.handleEval(ctx, w, id, params)
	case protocol.MethodClick:
		s.handleClick(ctx, w, id, params)
	case protocol.MethodFill:
		s.handleFill(ctx, w, id, params)
	case protocol.MethodNetwork:
		s.handleNetwork(ctx, w, id, params)
	case protocol.MethodConsole:
		s.handleConsole(ctx, w, id, params)
	case protocol.MethodErrors:
		s.handleErrors(ctx, w, id, params)
	case protocol.MethodFetch:
		s.handleFetch(ctx, w, id, params)
	case protocol.MethodSnapshot:
		s.handleSnapshot(ctx, w, id, params)
	case protocol.MethodNetworkRoute:
		s.handleNetworkRoute(ctx, w, id, params)
	case protocol.MethodNetworkUnroute:
		s.handleNetworkUnroute(ctx, w, id, params)
	case protocol.MethodNetworkClear:
		s.handleNetworkClear(ctx, w, id, params)
	case protocol.MethodConsoleClear:
		s.handleConsoleClear(ctx, w, id, params)
	case protocol.MethodErrorsClear:
		s.handleErrorsClear(ctx, w, id, params)
	}
}

func (s *Server) writeJSONRPCBytes(ctx context.Context, w http.ResponseWriter, id json.RawMessage, build func() ([]byte, error)) {
	b, err := build()
	if err != nil {
		s.logger.Error("json-rpc marshal failed", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(b); err != nil && s.logger != nil {
		s.logger.Error("json-rpc write failed", "err", err)
	}
	s.scheduleAudit(ctx, b)
}

func (s *Server) scheduleAudit(ctx context.Context, resp []byte) {
	meta := auditMetaFrom(ctx)
	if meta == nil || s.store == nil {
		return
	}
	seq, ok, errMsg := store.ParseResponseSummary(resp)
	rec := store.LogRecord{
		Action: meta.action,
		Body:   meta.body,
		// Only the identity this request was actually resolved to is recorded;
		// what the client asked for stays in Body.
		Target:   string(meta.target),
		SenderIP: meta.senderIP,
		Seq:      seq,
		OK:       ok,
		Error:    errMsg,
		Time:     meta.at,
	}
	s.enqueueAudit(rec)
}

// enqueueAudit keeps the non-blocking send. Once closeAudit has closed the
// channel, the record is appended under auditMu so store.Close cannot race it
// and a late handler cannot panic on a closed channel.
func (s *Server) enqueueAudit(rec store.LogRecord) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if s.auditClosed {
		s.appendAudit(rec)
		return
	}
	s.auditWG.Add(1)
	select {
	case s.auditCh <- rec:
	default:
		// Preserve the recovery log while bounding memory and goroutine count.
		s.appendAudit(rec)
		s.auditWG.Done()
	}
}

func (s *Server) appendAudit(rec store.LogRecord) {
	if err := s.store.AppendRPC(rec); err != nil && s.logger != nil {
		s.logger.Warn(auditFailureMessage(err), "err", err, "action", rec.Action)
	}
}

func (s *Server) rpcErr(ctx context.Context, w http.ResponseWriter, id json.RawMessage, code int, msg string, data *protocol.ErrData) {
	s.writeJSONRPCBytes(ctx, w, id, func() ([]byte, error) {
		return protocol.MarshalErrorResponse(id, code, msg, data)
	})
}

func (s *Server) rpcOK(ctx context.Context, w http.ResponseWriter, id json.RawMessage, result any) {
	s.touchSuccessfulRPC(ctx)
	s.writeJSONRPCBytes(ctx, w, id, func() ([]byte, error) {
		return protocol.MarshalResponse(id, result)
	})
}

// touchSuccessfulRPC keeps the in-memory idle clock aligned with rpc.jsonl:
// only successful tab-related RPCs renew a managed tab. tab_new is registered
// at creation, tab_close removes state, and tab_list is intentionally passive.
// The tab renewed is the identity pinned while the request ran; nothing is
// looked up again here.
func (s *Server) touchSuccessfulRPC(ctx context.Context) {
	meta := auditMetaFrom(ctx)
	if meta == nil || meta.target == "" || !protocol.IsTabRelatedMethod(meta.action) {
		return
	}
	switch meta.action {
	case protocol.MethodTabNew, protocol.MethodTabClose, protocol.MethodTabList:
		return
	}
	s.touchTabActivity(meta.target)
}

// pinTarget records the complete target id this request operates on. It is set
// once: the lock, the CDP command, the idle renewal and the audit record of a
// request all refer to the same target.
func pinTarget(ctx context.Context, tid target.ID) {
	if meta := auditMetaFrom(ctx); meta != nil && meta.target == "" {
		meta.target = tid
	}
}

const unknownTabHint = "tab must be the complete CDP target id of an open tab, exactly as returned by tab_new / tab_list"

func (s *Server) nextSeq(ctx context.Context, w http.ResponseWriter, id json.RawMessage) (uint64, bool) {
	seq, err := s.store.NextSeq()
	if err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInternalError, "sequence error", nil)
		return 0, false
	}
	return seq, true
}

func (s *Server) handleTabList(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.TabListParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	_ = p

	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	snaps, err := s.enumerateAuthoritativeTabs(ctx, conn)
	if err != nil {
		s.logger.Error("tab_list targets failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to list targets", &protocol.ErrData{
			Error: "failed to list targets",
			Hint:  s.cdpHint(err),
		})
		return
	}
	items := make([]protocol.TabListItem, 0, len(snaps))
	for _, sn := range snaps {
		items = append(items, protocol.TabListItem{
			Tab:   string(sn.TargetID),
			Title: sn.Title,
			URL:   sn.URL,
		})
	}
	focus := s.tabs.Selected()
	tabField := operationalTab(s.tabs, snaps)
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.TabListResult{
		Tab:   string(tabField),
		Seq:   seq,
		Tabs:  items,
		Focus: string(focus),
	})
}

func (s *Server) handleTabFocus(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.TabFocusParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	_ = p

	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	snaps, err := s.enumerateAuthoritativeTabs(ctx, conn)
	if err != nil {
		s.logger.Error("tab_focus targets failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to list targets", &protocol.ErrData{
			Error: "failed to list targets",
			Hint:  s.cdpHint(err),
		})
		return
	}
	if len(snaps) == 0 {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "no focused tab", &protocol.ErrData{
			Error:  "no page tabs",
			Hint:   "open a tab in Chrome or attach to a session with at least one page target",
			Method: protocol.MethodTabFocus,
		})
		return
	}
	s.syncRegistryFocusFromBrowser(ctx, conn, snaps)
	focus := s.tabs.Selected()
	tabField := operationalTab(s.tabs, snaps)
	pinTarget(ctx, tabField)
	var title, url string
	for _, sn := range snaps {
		if sn.TargetID == tabField {
			title = sn.Title
			url = sn.URL
			break
		}
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.TabFocusResult{
		Tab:   string(tabField),
		Focus: string(focus),
		Title: title,
		URL:   url,
		Seq:   seq,
	})
}

// syncRegistryFocusFromBrowser updates TabRegistry selection when the browser
// has exactly one page with document.visibilityState === "visible" (typical
// single-window foreground tab after the user switches tabs in Chrome).
func (s *Server) syncRegistryFocusFromBrowser(ctx context.Context, conn tabConn, snaps []state.TabSnapshot) {
	if conn == nil || len(snaps) == 0 {
		return
	}
	type foregroundDetector interface {
		DetectForegroundTarget(snaps []state.TabSnapshot) (id target.ID, ok bool)
	}
	type foregroundDetectorContext interface {
		DetectForegroundTargetContext(context.Context, []state.TabSnapshot) (id target.ID, ok bool)
	}
	if d, ok := conn.(foregroundDetectorContext); ok {
		tid, ok := d.DetectForegroundTargetContext(ctx, snaps)
		if ok {
			_ = s.tabs.Select(tid)
		}
		return
	}
	d, ok := conn.(foregroundDetector)
	if !ok {
		return
	}
	tid, ok := d.DetectForegroundTarget(snaps)
	if !ok {
		return
	}
	_ = s.tabs.Select(tid)
}

// operationalTab is the tab reported by calls that name none: the selected tab
// when it is in snaps, else the first snapshot (snaps are ordered by target id).
func operationalTab(reg *state.TabRegistry, snaps []state.TabSnapshot) target.ID {
	if len(snaps) == 0 {
		return ""
	}
	focus := reg.Selected()
	if focus != "" {
		for _, sn := range snaps {
			if sn.TargetID == focus {
				return focus
			}
		}
	}
	return snaps[0].TargetID
}

func (s *Server) handleTabSelect(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.TabSelectParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Hint:   `tab_select requires "tab"`,
			Method: protocol.MethodTabSelect,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	// Selecting is an operation on the tab like any other: it takes the tab's
	// operation lock, so it cannot succeed (and renew the idle clock) while an
	// idle close of the same tab is in progress. The lock is held until the
	// response, renewal included, is done.
	unlock, locked := s.lockTab(ctx, tab)
	if !locked {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveEnumeratedTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodTabSelect, err)
		return
	}
	if !s.tabs.Select(tid) {
		if s.tabs.Suspended(tid) {
			s.rpcResolveErr(ctx, w, id, protocol.MethodTabSelect, fmt.Errorf("%w: tab state pending verification", errTabLookup))
		} else {
			s.rpcResolveErr(ctx, w, id, protocol.MethodTabSelect, errTabCloseUnknownID)
		}
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.TabSelectResult{Tab: tab, Seq: seq})
}

func (s *Server) handleTabNew(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.TabNewParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	// Shutdown lets the creations in flight settle and sends no new one.
	if !s.creates.begin() {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "daemon shutting down", &protocol.ErrData{
			Error:  "daemon shutting down",
			Hint:   "no tab was requested from the browser",
			Method: protocol.MethodTabNew,
		})
		return
	}
	defer s.creates.end()
	initial := strings.TrimSpace(p.URL)
	// Only a creation that was sent has anything to settle: a request that is
	// already over asks the browser for nothing.
	if ended, over := s.creationEnded(ctx); over {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to create tab", &protocol.ErrData{
			Error:  "failed to create tab",
			Hint:   ended + " before Target.createTarget was sent; no tab was requested from the browser",
			Method: protocol.MethodTabNew,
		})
		return
	}
	// Window before the request: the new target exists in the browser, and can
	// be listed, closed and destroyed, before this handler sees the reply.
	win := s.tabs.OpenWindow()
	reply, answered := s.requestCreatedTarget(ctx, conn, initial, p.Silent)
	if !answered {
		// The request ended and the reply did not come within the settlement:
		// whether a tab exists is not known and no id identifies it.
		s.tabs.Abandon(win)
		ended, _ := s.creationEnded(ctx)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.triggerBrowserProbe()
		}
		s.logger.Warn("tab_new ended before Target.createTarget answered; a tab may have been created, its id is not known, it is not managed and was not closed", "reason", ended)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to create tab", &protocol.ErrData{
			Error:  "failed to create tab",
			Hint:   fmt.Sprintf("%s before Target.createTarget answered, and no reply followed within the cleanup wait; if the browser did create a tab, its id is not known: it is unmanaged, was not closed and shows up in tab_list", ended),
			Method: protocol.MethodTabNew,
		})
		return
	}
	tid, err := reply.tid, reply.err
	if err == nil && tid == "" {
		err = errors.New("Target.createTarget returned no target id")
	}
	if err != nil {
		s.tabs.Abandon(win)
		hint := s.cdpHint(err)
		if createOutcomeUnknown(err) {
			// Not a refusal: the round trip ended without the browser's
			// answer (connection lost, deadline). The command may have been
			// executed.
			s.logger.Warn("tab_new lost its Target.createTarget round trip before the browser answered; a tab may have been created, its id is not known, it is not managed and was not closed", "err", err)
			hint += "; the browser did not answer Target.createTarget, so whether it created a tab is not known: if it did, the tab's id is not known, it is unmanaged, was not closed and shows up in tab_list"
		} else {
			s.logger.Error("tab_new create target failed", "err", err)
		}
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to create tab", &protocol.ErrData{
			Error: "failed to create tab",
			Hint:  hint,
		})
		return
	}
	pinTarget(ctx, tid)
	committed, ended := s.commitCreatedTab(ctx, conn, win, tid)
	if ended != "" {
		// The request ended before the creation was committed: nobody can be
		// given this id any more. Roll back exactly the tab the reply names
		// instead of leaving it open and unmanaged.
		cleanup, closed := s.rollbackCreatedTab(ctx, conn, tid)
		hint := fmt.Sprintf("%s before the new tab %s was committed; it is not registered as managed; cleanup: %s", ended, tid, cleanup)
		if closed {
			s.logger.Warn("tab_new ended before its tab was committed; the created tab was rolled back", "tab", string(tid), "reason", ended, "cleanup", cleanup)
		} else {
			s.logger.Warn("tab_new ended before its tab was committed and the rollback failed; the tab may still be open and is not managed", "tab", string(tid), "reason", ended, "cleanup", cleanup)
			hint += "; the tab may still be open: it shows up in tab_list and can be closed with tab_close"
		}
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "tab_new interrupted before completion", &protocol.ErrData{
			Error:  "tab_new interrupted before completion",
			Hint:   hint,
			Method: protocol.MethodTabNew,
		})
		return
	}
	if !committed {
		// The tab was reported gone while it was being created, or right after
		// by the enumeration that settled contradicting evidence. Make sure of
		// it: one bounded close of exactly this id, so no unmanaged tab is
		// left behind silently.
		cleanup, closed := s.rollbackCreatedTab(ctx, conn, tid)
		if !closed {
			s.logger.Warn("tab_new rollback failed; the tab may still be open and is not managed", "tab", string(tid), "cleanup", cleanup)
		}
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "tab closed before tab_new completed", &protocol.ErrData{
			Error:  "tab closed before tab_new completed",
			Hint:   fmt.Sprintf("the new tab %s was closed (tab_close, idle cleanup, or in the browser) while it was being created; cleanup: %s; call tab_new again", tid, cleanup),
			Method: protocol.MethodTabNew,
		})
		return
	}
	if !p.Silent {
		s.tabs.Select(tid)
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.TabNewResult{Tab: string(tid), Seq: seq})
}

func (s *Server) handleGoto(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.GotoParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Hint:   `goto requires "tab" and "url"`,
			Method: protocol.MethodGoto,
		})
		return
	}
	urlStr := strings.TrimSpace(p.URL)
	if urlStr == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing url", &protocol.ErrData{
			Error:  "missing url",
			Hint:   `goto requires "url"`,
			Method: protocol.MethodGoto,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodGoto, err)
		return
	}
	if err := navigate(ctx, conn, tid, urlStr); err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("goto navigate failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "navigation failed", &protocol.ErrData{
			Error: "navigation failed",
			Hint:  s.cdpHint(err),
		})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.GotoResult{Tab: tab, Seq: seq})
}

func (s *Server) handleReload(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.ReloadParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Hint:   `reload requires "tab"`,
			Method: protocol.MethodReload,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodReload, err)
		return
	}
	if err := reload(ctx, conn, tid); err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("reload failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "reload failed", &protocol.ErrData{
			Error: "reload failed",
			Hint:  s.cdpHint(err),
		})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.ReloadResult{Tab: tab, Seq: seq})
}

func (s *Server) handleTabClose(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.TabCloseParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Hint:   `tab_close requires "tab"`,
			Method: protocol.MethodTabClose,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	if err := s.closeTab(ctx, tab); err != nil {
		switch {
		case errors.Is(err, errTabCloseUnknownID), errors.Is(err, errTabLookup):
			s.rpcResolveErr(ctx, w, id, protocol.MethodTabClose, err)
		case errors.Is(err, errTabCloseNoConn):
			s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		case errors.Is(err, errTabLockTimeout):
			s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		default:
			s.logger.Error("tab_close failed", "err", err)
			s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to close tab", &protocol.ErrData{
				Error: "failed to close tab",
				Hint:  s.cdpHint(err),
			})
		}
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.TabCloseResult{Tab: tab, Seq: seq})
}

func (s *Server) handleScreenshot(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.ScreenshotParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Method: protocol.MethodScreenshot,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodScreenshot, err)
		return
	}
	raw, mime, err := screenshot(ctx, conn, tid, p.Format)
	if err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("screenshot failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "screenshot failed", &protocol.ErrData{
			Error: "screenshot failed",
			Hint:  s.cdpHint(err),
		})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.ScreenshotResult{
		Tab:  tab,
		Seq:  seq,
		Data: base64.StdEncoding.EncodeToString(raw),
		MIME: mime,
	})
}

func (s *Server) handleEval(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.EvalParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	script := strings.TrimSpace(p.Script)
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Method: protocol.MethodEval,
		})
		return
	}
	if script == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing script", &protocol.ErrData{
			Error:  "missing script",
			Method: protocol.MethodEval,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodEval, err)
		return
	}
	out, err := eval(ctx, conn, tid, script)
	if err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("eval failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "eval failed", &protocol.ErrData{
			Error: "eval failed",
			Hint:  s.cdpHint(err),
		})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.EvalResult{Tab: tab, Seq: seq, Result: out})
}

func (s *Server) handleClick(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.ClickParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	sel := strings.TrimSpace(p.Selector)
	ref := strings.TrimSpace(p.Ref)
	if ref != "" {
		sel = browser.SnapshotRefSelector(ref)
	}
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Method: protocol.MethodClick,
		})
		return
	}
	if sel == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing selector", &protocol.ErrData{
			Error:  "missing selector or ref",
			Method: protocol.MethodClick,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodClick, err)
		return
	}
	if err := click(ctx, conn, tid, sel); err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("click failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "click failed", &protocol.ErrData{
			Error: "click failed",
			Hint:  s.cdpHint(err),
		})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.ClickResult{Tab: tab, Seq: seq})
}

func (s *Server) handleFetch(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.FetchParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	rawURL := strings.TrimSpace(p.URL)
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Method: protocol.MethodFetch,
		})
		return
	}
	if rawURL == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing url", &protocol.ErrData{
			Error:  "missing url",
			Method: protocol.MethodFetch,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodFetch, err)
		return
	}
	routing, ok := conn.(fetchConn)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "fetch not supported", &protocol.ErrData{
			Error: "browser backend does not implement fetch",
		})
		return
	}
	hdrs := []byte(strings.TrimSpace(p.Headers))
	out, err := fetchPage(ctx, routing, tid, rawURL, p.Method, hdrs, p.Body)
	if err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("fetch failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "fetch failed", &protocol.ErrData{
			Error: "fetch failed",
			Hint:  s.cdpHint(err),
		})
		return
	}
	var wrap struct {
		OK     bool `json:"ok"`
		Status int  `json:"status"`
	}
	_ = json.Unmarshal(out, &wrap)
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.FetchResult{
		Tab:    tab,
		Seq:    seq,
		OK:     wrap.OK,
		Status: wrap.Status,
		Result: out,
	})
}

func (s *Server) handleSnapshot(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.SnapshotParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Method: protocol.MethodSnapshot,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodSnapshot, err)
		return
	}
	snapper, ok := conn.(snapshotConn)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "snapshot not supported", nil)
		return
	}
	title, pageURL, text, refs, err := snapshot(ctx, snapper, tid, browser.SnapshotOpts{
		InteractiveOnly: p.InteractiveOnly,
		PruneEmpty:      p.PruneEmpty,
		MaxDepth:        p.MaxDepth,
		SelectorScope:   strings.TrimSpace(p.SelectorScope),
	})
	if err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("snapshot failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "snapshot failed", &protocol.ErrData{
			Error: "snapshot failed",
			Hint:  s.cdpHint(err),
		})
		return
	}
	if refs == nil {
		refs = map[string]string{}
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.SnapshotResult{
		Tab:   tab,
		Seq:   seq,
		Title: title,
		URL:   pageURL,
		Text:  text,
		Refs:  refs,
	})
}

func (s *Server) handleNetworkRoute(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.NetworkRouteParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	pat := strings.TrimSpace(p.URLPattern)
	if tab == "" || pat == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab or url_pattern", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, "", err)
		return
	}
	rc, ok := conn.(routeConn)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "network_route not supported", nil)
		return
	}
	rule := browser.NetworkRouteRule{
		URLPattern:  pat,
		Abort:       p.Abort,
		MockBody:    p.Body,
		ContentType: p.ContentType,
		Status:      p.Status,
	}
	if err := appendNetworkRoute(ctx, rc, tid, rule); err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("network_route failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "network_route failed", &protocol.ErrData{Hint: s.cdpHint(err)})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.NetworkRouteResult{
		Tab:    tab,
		Seq:    seq,
		Routes: rc.NetworkRouteCount(tid),
	})
}

func (s *Server) handleNetworkUnroute(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.NetworkUnrouteParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, "", err)
		return
	}
	rc, ok := conn.(routeConn)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "network_unroute not supported", nil)
		return
	}
	if err := removeNetworkRoutes(ctx, rc, tid, p.URLPattern); err != nil {
		s.observeTargetError(conn, tid, err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "network_unroute failed", &protocol.ErrData{Hint: s.cdpHint(err)})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.NetworkUnrouteResult{
		Tab:    tab,
		Seq:    seq,
		Routes: rc.NetworkRouteCount(tid),
	})
}

func (s *Server) handleConsoleClear(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.ConsoleClearParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, "", err)
		return
	}
	if s.obsStore != nil {
		s.obsStore.ClearConsoleOnly(tid)
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.ConsoleClearResult{Tab: tab, Seq: seq})
}

func (s *Server) handleErrorsClear(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.ErrorsClearParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, "", err)
		return
	}
	if s.obsStore != nil {
		s.obsStore.ClearErrorsOnly(tid)
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.ErrorsClearResult{Tab: tab, Seq: seq})
}

func (s *Server) handleNetworkClear(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.NetworkClearParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, "", err)
		return
	}
	if s.obsStore != nil {
		s.obsStore.ClearNetworkOnly(tid)
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.NetworkClearResult{Tab: tab, Seq: seq})
}

func (s *Server) handleFill(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.FillParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := p.Tab
	sel := strings.TrimSpace(p.Selector)
	ref := strings.TrimSpace(p.Ref)
	if ref != "" {
		sel = browser.SnapshotRefSelector(ref)
	}
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", &protocol.ErrData{
			Error:  "missing tab",
			Method: protocol.MethodFill,
		})
		return
	}
	if sel == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing selector", &protocol.ErrData{
			Error:  "missing selector or ref",
			Method: protocol.MethodFill,
		})
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	unlock, ok := s.lockTab(ctx, tab)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "request timed out", tabLockTimeoutData())
		return
	}
	defer unlock()
	tid, err := s.resolveTab(ctx, conn, tab)
	if err != nil {
		s.rpcResolveErr(ctx, w, id, protocol.MethodFill, err)
		return
	}
	if err := fill(ctx, conn, tid, sel, p.Text); err != nil {
		s.observeTargetError(conn, tid, err)
		s.logger.Error("fill failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "fill failed", &protocol.ErrData{
			Error: "fill failed",
			Hint:  s.cdpHint(err),
		})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.FillResult{Tab: tab, Seq: seq})
}

// resolveTab resolves the tab id of a request to a registered target and pins
// it for the rest of the request. tab must be a complete CDP target id; when it
// is not registered yet the registry is aligned with the browser and the same
// id is looked up again. Nothing else is ever substituted for it.
//
// The error says why there is no target: errTabCloseUnknownID when the id still
// does not resolve after the browser was asked successfully and the target is
// not suspended pending verification, errTabLookup wrapping the cause when the
// browser could not be asked or verification stayed inconclusive. A failed or
// inconclusive enumeration proves nothing about the id and must not be reported
// as an invalid one.
func (s *Server) resolveTab(ctx context.Context, conn tabConn, tab string) (target.ID, error) {
	if tid, ok := s.tabs.Lookup(tab); ok {
		pinTarget(ctx, tid)
		return tid, nil
	}
	return s.resolveEnumeratedTab(ctx, conn, tab)
}

func (s *Server) resolveEnumeratedTab(ctx context.Context, conn tabConn, tab string) (target.ID, error) {
	for attempt := 0; ; attempt++ {
		if _, err := s.enumerateTabs(ctx, conn); err != nil {
			return "", fmt.Errorf("%w: %w", errTabLookup, err)
		}
		if tid, ok := s.tabs.Lookup(tab); ok {
			pinTarget(ctx, tid)
			return tid, nil
		}
		if !s.tabs.Suspended(target.ID(tab)) {
			return "", errTabCloseUnknownID
		}
		if attempt >= maxSuspendedVerifyAttempts {
			return "", fmt.Errorf("%w: tab state pending verification", errTabLookup)
		}
	}
}

// rpcResolveErr answers a request whose tab id did not resolve (err is from
// resolveTab): the explicit "unknown tab id" only for an id the browser does
// not have, otherwise the failure of the lookup with its cause, on the same
// path as a failed tab_list (a timeout names the operation budget).
func (s *Server) rpcResolveErr(ctx context.Context, w http.ResponseWriter, id json.RawMessage, method string, err error) {
	if errors.Is(err, errTabCloseUnknownID) {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "unknown tab id", &protocol.ErrData{
			Error:  "unknown tab id",
			Hint:   unknownTabHint,
			Method: method,
		})
		return
	}
	s.logger.Error("resolve tab: list targets failed", "method", method, "err", err)
	s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to list targets", &protocol.ErrData{
		Error:  "failed to list targets",
		Hint:   s.cdpHint(err),
		Method: method,
	})
}

func (s *Server) cdpHint(err error) string {
	if err == nil {
		return ""
	}
	if isCDPSessionLost(err) {
		s.fatalCDPLost(err)
	} else if errors.Is(err, context.DeadlineExceeded) {
		s.triggerBrowserProbe()
		return fmt.Sprintf("%v (one RPC is limited to %ds; split longer work into several calls)", err, timeout.OperationSeconds)
	}
	return err.Error()
}

// auditFailureMessage names what failed for an audit append error: the write
// itself, or only the log maintenance around a record that went to the open
// log handle (which does not confirm that a restart reads it back).
func auditFailureMessage(err error) string {
	if store.RecordWritten(err) {
		return "rpc log record written to the open log, but log maintenance failed; recoverability after a restart is not confirmed"
	}
	return "append rpc log failed"
}
