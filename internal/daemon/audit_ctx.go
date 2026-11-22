package daemon

import (
	"context"
	"encoding/json"
	"time"
)

type auditMeta struct {
	action   string
	body     json.RawMessage
	senderIP string
	at       time.Time
	// target is the full CDP target id the request acted on, when resolved.
	target string
	// pinned is the full target id a handler resolved its tab to and then acted
	// on. When set, a success is credited to this target and the short id is not
	// looked up again: by the time the reply is written it may name another tab.
	pinned string
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
