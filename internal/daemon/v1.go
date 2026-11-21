package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/internal/state"
	"github.com/yiplee/go-bb-browser/internal/store"
	"github.com/yiplee/go-bb-browser/internal/timeout"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
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

	// From here on the request may record what it does. Shutdown waits for
	// admitted requests and cancels them through rpcCtx if it has to.
	if !s.rpcGate.enter() {
		http.Error(w, "daemon is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.rpcGate.leave()
	ctx, cancelRPC := context.WithCancel(ctx)
	defer cancelRPC()
	defer context.AfterFunc(s.rpcCtx, cancelRPC)()

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
	if !s.writeJSONRPCRaw(w, b, err) {
		return
	}
	s.scheduleAudit(ctx, b)
}

// writeJSONRPCRaw sends an already-built response and reports whether there
// was one to send.
func (s *Server) writeJSONRPCRaw(w http.ResponseWriter, b []byte, err error) bool {
	if err != nil {
		s.logger.Error("json-rpc marshal failed", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(b); err != nil && s.logger != nil {
		s.logger.Error("json-rpc write failed", "err", err)
	}
	return true
}

func (s *Server) scheduleAudit(ctx context.Context, resp []byte) {
	meta := auditMetaFrom(ctx)
	if meta == nil || s.store == nil {
		return
	}
	tab, seq, ok, errMsg := store.ParseResponseSummary(resp)
	if tab == "" {
		tab = store.TabFromRequestBody(meta.body)
	}
	rec := store.LogRecord{
		Action:   meta.action,
		Body:     meta.body,
		Tab:      tab,
		Target:   string(meta.target),
		SenderIP: meta.senderIP,
		Seq:      seq,
		OK:       ok,
		Error:    errMsg,
		Time:     meta.at,
	}
	s.enqueueAudit(rec)
}

// enqueueAudit appends rec to the audit FIFO; its position is final once this
// returns. Normally the writer goroutine persists it. When the queue is at its
// bound the caller first writes the backlog itself, in order, and only then
// queues rec: that is the backpressure, it keeps the queue within auditQueueCap,
// and it never lets a record overtake one queued before it. Once the writer has
// stopped the caller also writes rec. Flushing only takes audit and store
// locks, so it is safe under lifeMu.
func (s *Server) enqueueAudit(rec store.LogRecord) {
	s.auditWG.Add(1)
	for {
		s.auditMu.Lock()
		if len(s.auditPending) < max(s.auditQueueCap, 1) {
			s.auditPending = append(s.auditPending, rec)
			closed := s.auditClosed
			s.auditMu.Unlock()
			if closed {
				s.flushAudit()
				return
			}
			select {
			case s.auditWake <- struct{}{}:
			default:
			}
			return
		}
		s.auditMu.Unlock()
		s.flushAudit()
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
//
// The tab renewed and named in the record is the one the request was bound to
// and ran on. The short id is not looked up again: by now it can belong to a
// different page, which this request never touched.
func (s *Server) touchSuccessfulRPC(ctx context.Context) {
	meta := auditMetaFrom(ctx)
	if meta == nil || meta.bound == "" || !protocol.IsTabRelatedMethod(meta.action) {
		return
	}
	switch meta.action {
	case protocol.MethodTabNew, protocol.MethodTabClose, protocol.MethodTabList:
		return
	}
	meta.target = meta.bound
	s.touchTabActivity(meta.bound)
}

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
	listed, err := s.listAndSyncTabs(ctx, conn)
	if err != nil {
		s.logger.Error("tab_list targets failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to list targets", &protocol.ErrData{
			Error: "failed to list targets",
			Hint:  s.cdpHint(err),
		})
		return
	}
	// tabs, tab and focus all come from the one registry snapshot the
	// enumeration produced: a tab opened or selected since is in none of them.
	snaps := listed.Tabs
	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].ShortID < snaps[j].ShortID
	})
	items := make([]protocol.TabListItem, 0, len(snaps))
	for _, sn := range snaps {
		items = append(items, protocol.TabListItem{
			Tab:   sn.ShortID,
			Title: sn.Title,
			URL:   sn.URL,
		})
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.TabListResult{
		Tab:   operationalTabShort(listed.Focus, snaps),
		Seq:   seq,
		Tabs:  items,
		Focus: listed.Focus,
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
	listed, err := s.listAndSyncTabs(ctx, conn)
	if err != nil {
		s.logger.Error("tab_focus targets failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to list targets", &protocol.ErrData{
			Error: "failed to list targets",
			Hint:  s.cdpHint(err),
		})
		return
	}
	snaps := listed.Tabs
	if len(snaps) == 0 {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "no focused tab", &protocol.ErrData{
			Error:  "no page tabs",
			Hint:   "open a tab in Chrome or attach to a session with at least one page target",
			Method: protocol.MethodTabFocus,
		})
		return
	}
	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].ShortID < snaps[j].ShortID
	})
	// The probe attaches to pages and takes time. Any of the enumerated tabs
	// can go away meanwhile and its short id pass to another page, so the answer
	// is decided afterwards, against the registry as it is then, by full target
	// id. The response, its title and URL and the audit record all describe the
	// one tab that decision returns.
	detected := detectForegroundTarget(ctx, conn, snaps)
	cur, focus, ok := s.tabs.CommitFocus(snaps, detected)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "no focused tab", &protocol.ErrData{
			Error:  "tabs changed",
			Hint:   "the listed tabs were closed while tab_focus was running; retry",
			Method: protocol.MethodTabFocus,
		})
		return
	}
	bindAuditTarget(ctx, cur.TargetID)
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		return
	}
	s.rpcOK(ctx, w, id, protocol.TabFocusResult{
		Tab:   cur.ShortID,
		Focus: focus,
		Title: cur.Title,
		URL:   cur.URL,
		Seq:   seq,
	})
}

// detectForegroundTarget returns the one page among snaps whose
// document.visibilityState is "visible" (typical single-window foreground tab
// after the user switches tabs in Chrome), or "" if there is no such single
// page or the probe is incomplete. It reports the target it looked at; the
// short id that target had in snaps may belong to another page by now.
func detectForegroundTarget(ctx context.Context, conn tabConn, snaps []state.TabSnapshot) target.ID {
	type foregroundDetector interface {
		DetectForegroundShort(snaps []state.TabSnapshot) (short string, ok bool)
	}
	type foregroundDetectorContext interface {
		DetectForegroundShortContext(context.Context, []state.TabSnapshot) (short string, ok bool)
	}
	var sh string
	var found bool
	if d, ok := conn.(foregroundDetectorContext); ok {
		sh, found = d.DetectForegroundShortContext(ctx, snaps)
	} else if d, ok := conn.(foregroundDetector); ok {
		sh, found = d.DetectForegroundShort(snaps)
	}
	if !found {
		return ""
	}
	for _, sn := range snaps {
		if sn.ShortID == sh {
			return sn.TargetID
		}
	}
	return ""
}

// operationalTabShort is the "tab" of a tab_list result: the focused tab, else
// the first one. focus and snaps are from the same registry snapshot, so a
// non-empty focus is one of snaps.
func operationalTabShort(focus string, snaps []state.TabSnapshot) string {
	if focus != "" || len(snaps) == 0 {
		return focus
	}
	return snaps[0].ShortID
}

func (s *Server) handleTabSelect(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.TabSelectParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := strings.TrimSpace(p.Tab)
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
	unknown := func() {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "unknown tab id", &protocol.ErrData{
			Error:  "unknown tab id",
			Hint:   "invalid or stale tab short id",
			Method: protocol.MethodTabSelect,
		})
	}
	// Bind the target the short id names now. The enumeration below only checks
	// that this target is still there; a page it discovers under the same short
	// id is not what the caller asked for. The selection is then committed only
	// if the short id still names the bound target, checked and set in one
	// registry critical section: the tab can be closed, and the short id taken
	// over, while the enumeration is in flight.
	tid, ok := s.tabs.Lookup(tab)
	if !ok {
		unknown()
		return
	}
	if _, err := s.listAndSyncTabs(ctx, conn); err != nil {
		s.logger.Error("tab_select targets failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to list targets", &protocol.ErrData{
			Error: "failed to list targets",
			Hint:  s.cdpHint(err),
		})
		return
	}
	if !s.tabs.SelectAlias(tab, tid) {
		unknown()
		return
	}
	bindAuditTarget(ctx, tid)
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
	initial := strings.TrimSpace(p.URL)
	create := s.beginCreate()
	tid, err := createPageTarget(ctx, conn, initial, p.Silent)
	// The session can already know the new target is destroyed: Chrome reported
	// it before answering createTarget. The id still comes back, for the log.
	goneAtReply := err != nil && tid != "" && errors.Is(err, browser.ErrCreatedTargetGone)
	if err != nil && !goneAtReply {
		s.endCreate(create)
		s.logger.Error("tab_new create target failed", "err", err)
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "failed to create tab", &protocol.ErrData{
			Error: "failed to create tab",
			Hint:  s.cdpHint(err),
		})
		return
	}
	seq, ok := s.nextSeq(ctx, w, id)
	if !ok {
		s.endCreate(create)
		return
	}
	// Register, mark managed and queue the tab_new record as one lifecycle
	// step. The target can be destroyed at any moment after createTarget; were
	// the record queued only after the response is written, the synthetic
	// tab_close could precede it and leave the tab managed on disk.
	s.lifeMu.Lock()
	// Its destruction can even have been handled already, before createTarget
	// returned.
	gone := s.endCreateLocked(create, tid) || goneAtReply
	if gone {
		s.lifeMu.Unlock()
		s.tabNewTargetGone(ctx, w, id, tid, seq)
		return
	}
	short := s.tabs.RegisterPageTarget(tid)
	if meta := auditMetaFrom(ctx); meta != nil {
		meta.target = tid
	}
	s.markTabManaged(tid)
	if !p.Silent {
		s.tabs.SelectTarget(tid)
	}
	resp, err := protocol.MarshalResponse(id, protocol.TabNewResult{Tab: short, Seq: seq})
	if err == nil {
		s.scheduleAudit(ctx, resp)
	}
	s.lifeMu.Unlock()
	s.writeJSONRPCRaw(w, resp, err)
}

// tabNewTargetGone finishes a tab_new whose target is known to be destroyed
// already. What is logged and what the caller is told differ on purpose.
//
// The log gets the truth about the browser: a target was created (tab_new, by
// full target id) and is gone (the release), so nothing is left managed on
// disk. The caller gets an error and no tab id. The target never gets a short
// id: one handed out for a dead target would be free for the next page that
// opens, and a caller closing "its" tab would then close that page.
func (s *Server) tabNewTargetGone(ctx context.Context, w http.ResponseWriter, id json.RawMessage, tid target.ID, seq uint64) {
	s.lifeMu.Lock()
	if meta := auditMetaFrom(ctx); meta != nil && s.store != nil {
		s.enqueueAudit(store.LogRecord{
			Action:   meta.action,
			Body:     meta.body,
			Target:   string(tid),
			SenderIP: meta.senderIP,
			Seq:      seq,
			OK:       true,
			Time:     meta.at,
		})
	}
	s.logManagedTabGone(tid, "")
	s.lifeMu.Unlock()
	// Whatever an earlier event registered for the target is dropped with it.
	s.removeTargetState(tid)
	if sess, ok := s.tabConn().(*browser.Session); ok {
		sess.ForgetTarget(tid)
	}
	if s.logger != nil {
		s.logger.Warn("tab_new target was destroyed before it could be returned", "target", string(tid))
	}
	resp, err := protocol.MarshalErrorResponse(id, protocol.CodeServerError, "tab closed before tab_new completed", &protocol.ErrData{
		Error:  "target gone",
		Hint:   "the new tab was closed before tab_new returned; no tab id was assigned",
		Method: protocol.MethodTabNew,
	})
	// Not audited: the request's record is the creation written above.
	s.writeJSONRPCRaw(w, resp, err)
}

func (s *Server) handleGoto(ctx context.Context, w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var p protocol.GotoParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "invalid params", nil)
		return
	}
	tab := strings.TrimSpace(p.Tab)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, &protocol.ErrData{
		Error:  "unknown tab id",
		Hint:   "invalid or stale tab short id",
		Method: protocol.MethodGoto,
	})
	if !ok {
		return
	}
	defer unlock()
	if err := navigate(ctx, conn, tid, urlStr); err != nil {
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
	tab := strings.TrimSpace(p.Tab)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, &protocol.ErrData{
		Error:  "unknown tab id",
		Hint:   "invalid or stale tab short id",
		Method: protocol.MethodReload,
	})
	if !ok {
		return
	}
	defer unlock()
	if err := reload(ctx, conn, tid); err != nil {
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
	tab := strings.TrimSpace(p.Tab)
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
	if err := s.closeTabByShort(ctx, tab); err != nil {
		switch {
		case errors.Is(err, errUnknownTabID):
			s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "unknown tab id", &protocol.ErrData{
				Error:  "unknown tab id",
				Hint:   "invalid or stale tab short id",
				Method: protocol.MethodTabClose,
			})
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
	tab := strings.TrimSpace(p.Tab)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, &protocol.ErrData{
		Error:  "unknown tab id",
		Hint:   "invalid or stale tab short id",
		Method: protocol.MethodScreenshot,
	})
	if !ok {
		return
	}
	defer unlock()
	raw, mime, err := screenshot(ctx, conn, tid, p.Format)
	if err != nil {
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
	tab := strings.TrimSpace(p.Tab)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, &protocol.ErrData{
		Error:  "unknown tab id",
		Hint:   "invalid or stale tab short id",
		Method: protocol.MethodEval,
	})
	if !ok {
		return
	}
	defer unlock()
	out, err := eval(ctx, conn, tid, script)
	if err != nil {
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
	tab := strings.TrimSpace(p.Tab)
	sel := strings.TrimSpace(p.Selector)
	ref := strings.TrimSpace(p.Ref)
	if ref != "" {
		sel = snapRefSelector(ref)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, &protocol.ErrData{
		Error:  "unknown tab id",
		Hint:   "invalid or stale tab short id",
		Method: protocol.MethodClick,
	})
	if !ok {
		return
	}
	defer unlock()
	if err := click(ctx, conn, tid, sel); err != nil {
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
	tab := strings.TrimSpace(p.Tab)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, &protocol.ErrData{
		Error:  "unknown tab id",
		Hint:   "invalid or stale tab short id",
		Method: protocol.MethodFetch,
	})
	if !ok {
		return
	}
	defer unlock()
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
	tab := strings.TrimSpace(p.Tab)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, &protocol.ErrData{
		Error:  "unknown tab id",
		Method: protocol.MethodSnapshot,
	})
	if !ok {
		return
	}
	defer unlock()
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
	tab := strings.TrimSpace(p.Tab)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, nil)
	if !ok {
		return
	}
	defer unlock()
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
	tab := strings.TrimSpace(p.Tab)
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, nil)
	if !ok {
		return
	}
	defer unlock()
	rc, ok := conn.(routeConn)
	if !ok {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "network_unroute not supported", nil)
		return
	}
	if err := removeNetworkRoutes(ctx, rc, tid, p.URLPattern); err != nil {
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
	tab := strings.TrimSpace(p.Tab)
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, nil)
	if !ok {
		return
	}
	defer unlock()
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
	tab := strings.TrimSpace(p.Tab)
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, nil)
	if !ok {
		return
	}
	defer unlock()
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
	tab := strings.TrimSpace(p.Tab)
	if tab == "" {
		s.rpcErr(ctx, w, id, protocol.CodeInvalidParams, "missing tab", nil)
		return
	}
	conn := s.tabConn()
	if conn == nil {
		s.rpcErr(ctx, w, id, protocol.CodeServerError, "browser session not ready", nil)
		return
	}
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, nil)
	if !ok {
		return
	}
	defer unlock()
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
	tab := strings.TrimSpace(p.Tab)
	sel := strings.TrimSpace(p.Selector)
	ref := strings.TrimSpace(p.Ref)
	if ref != "" {
		sel = snapRefSelector(ref)
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
	tid, unlock, ok := s.bindTabRPC(ctx, w, id, tab, &protocol.ErrData{
		Error:  "unknown tab id",
		Hint:   "invalid or stale tab short id",
		Method: protocol.MethodFill,
	})
	if !ok {
		return
	}
	defer unlock()
	if err := fill(ctx, conn, tid, sel, p.Text); err != nil {
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

// snapRefSelector expands a snapshot ref ("3" or "@3") to its attribute
// selector. Backslash must be escaped before the quote: otherwise a trailing
// backslash swallows the closing quote, and `\"` in a ref ends the string
// early so the rest of the ref is parsed as further selectors.
func snapRefSelector(ref string) string {
	ref = strings.TrimPrefix(ref, "@")
	return `[__bb_snap_ref="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(ref) + `"]`
}

func (s *Server) cdpHint(err error) string {
	if err == nil {
		return ""
	}
	if isCDPSessionLost(err) {
		s.fatalCDPLost(err)
	} else if errors.Is(err, context.DeadlineExceeded) {
		s.triggerBrowserProbe()
	}
	return err.Error()
}
