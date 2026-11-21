package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/chromedp/cdproto/target"
)

// auditMeta is the per-request record. It is only touched by the goroutine
// handling the request.
type auditMeta struct {
	action   string
	body     json.RawMessage
	senderIP string
	at       time.Time

	// target is the full CDP target id the request was resolved to, captured
	// while the tab was still registered. The short id in the request is only
	// an alias; the log needs the identity (see store.LogRecord).
	target target.ID
	// tabHold is the operation lock taken by lockTab for this request, so
	// resolveTab can make it cover the target it actually resolves.
	tabHold *tabLockHold
}

// setAuditTarget records which target the current request operates on.
func setAuditTarget(ctx context.Context, tid target.ID) {
	if m := auditMetaFrom(ctx); m != nil && tid != "" {
		m.target = tid
	}
}

type auditCtxKey struct{}

func contextWithAudit(ctx context.Context, m *auditMeta) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, auditCtxKey{}, m)
}

func auditMetaFrom(ctx context.Context) *auditMeta {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(auditCtxKey{}).(*auditMeta)
	return m
}
