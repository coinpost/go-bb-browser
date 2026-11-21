package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/chromedp/cdproto/target"
)

type auditMeta struct {
	action   string
	body     json.RawMessage
	senderIP string
	at       time.Time
	// bound is the full CDP target id the request was bound to: the tab it
	// runs on, whatever happens to the short id it was addressed by.
	bound target.ID
	// target is the target id written to the request's log record. It is set
	// from bound when the request succeeds.
	target target.ID
}

// bindAuditTarget records the target a request runs on.
func bindAuditTarget(ctx context.Context, tid target.ID) {
	if meta := auditMetaFrom(ctx); meta != nil {
		meta.bound = tid
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
