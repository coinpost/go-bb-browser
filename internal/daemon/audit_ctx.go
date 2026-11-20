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
	// target is the full CDP target id the request operated on, once resolved.
	target target.ID
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
