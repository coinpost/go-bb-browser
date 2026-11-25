package browser

import (
	"context"
	"testing"
	"time"

	"github.com/coinpost/go-bb-browser/internal/timeout"
)

// The daemon lets a creation outlive its RPC by a bounded settlement and says
// so with the deadline of the context it passes. The session must keep that
// deadline: cutting the round trip at timeout.Operation dropped the reply that
// names the created tab exactly when the RPC's own 60s ended.
func TestCreateTargetContextHonoursTheCallerDeadline(t *testing.T) {
	sess, stop := context.WithCancel(context.Background())
	defer stop()

	want := time.Now().Add(timeout.Operation + 6*time.Second)
	caller, cancelCaller := context.WithDeadline(context.Background(), want)
	defer cancelCaller()
	ctx, cancel := createTargetContext(sess, caller)
	defer cancel()
	if got, ok := ctx.Deadline(); !ok || !got.Equal(want) {
		t.Errorf("round trip deadline = %v (%v), want the caller's %v, beyond timeout.Operation", got, ok, want)
	}

	// A shorter caller deadline is kept as well.
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	sctx, scancel := createTargetContext(sess, short)
	defer scancel()
	if got, _ := sctx.Deadline(); time.Until(got) > time.Second {
		t.Errorf("round trip deadline %v ignores the caller's 1s", time.Until(got))
	}

	// Without a caller deadline the default bound applies.
	before := time.Now()
	dctx, dcancel := createTargetContext(sess, context.Background())
	defer dcancel()
	got, ok := dctx.Deadline()
	if !ok || got.Before(before.Add(timeout.Operation)) || got.After(time.Now().Add(timeout.Operation)) {
		t.Errorf("default round trip deadline = %v (%v), want timeout.Operation from now", got, ok)
	}

	// The session's end still ends the round trip.
	stop()
	if ctx.Err() == nil || dctx.Err() == nil {
		t.Error("a round trip context outlived the session")
	}
}
