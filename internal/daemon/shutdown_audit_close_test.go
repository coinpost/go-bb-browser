package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The production entry point (ListenAndServe) closes the RPC log last. A close
// that cannot confirm the log — e.g. the state-dir sync a rotation still owes
// keeps failing — must be part of what ListenAndServe returns (cmd/bb-daemon
// logs it and exits non-zero), without replacing the error that ended the
// service.

var errLogCloseUnconfirmed = errors.New("rpc log rotated but state dir sync failed: input/output error")

func newShutdownTestServer(t *testing.T, listen string) *Server {
	t.Helper()
	cfg := Config{DebuggerURL: "127.0.0.1:9222", ListenAddr: listen, StateDir: t.TempDir()}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SkipBrowserAttach = true
	return srv
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestListenAndServeReportsAuditCloseErrorOnCleanShutdown(t *testing.T) {
	addr := freeLoopbackAddr(t)
	srv := newShutdownTestServer(t, addr)
	closes := 0
	srv.auditCloseHook = func(err error) error {
		closes++
		if err != nil {
			t.Errorf("store close failed on its own: %v", err)
		}
		if closes == 1 {
			return errLogCloseUnconfirmed
		}
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	up := false
	for range 200 {
		res, err := http.Get("http://" + addr + "/live")
		if err == nil {
			res.Body.Close()
			up = res.StatusCode == http.StatusOK
			if up {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !up {
		cancel()
		t.Fatal("daemon did not come up")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errLogCloseUnconfirmed) {
			t.Fatalf("clean shutdown with a failing log close returned %v", err)
		}
		if !strings.Contains(err.Error(), "close rpc log") {
			t.Fatalf("error does not name the close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown timed out")
	}
	if closes != 1 {
		t.Fatalf("log closed %d times by ListenAndServe, want 1", closes)
	}
	// Closing again (tests, defensive callers) stays quiet.
	if err := srv.closeAudit(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestListenAndServeKeepsServiceErrorNextToAuditCloseError(t *testing.T) {
	// The listen address is taken: the service error is the listen failure.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	srv := newShutdownTestServer(t, busy.Addr().String())
	srv.auditCloseHook = func(error) error { return errLogCloseUnconfirmed }

	err = srv.ListenAndServe(context.Background())
	if err == nil {
		t.Fatal("listen on a busy address succeeded")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("service error lost: %v", err)
	}
	if !errors.Is(err, errLogCloseUnconfirmed) {
		t.Fatalf("log close error lost: %v", err)
	}
	msg := err.Error()
	if li, ci := strings.Index(msg, "listen:"), strings.Index(msg, "close rpc log"); li < 0 || ci < 0 || li > ci {
		t.Fatalf("service error must come first, then the close error: %q", msg)
	}

	// Without a close error the service error is returned as before.
	srv2 := newShutdownTestServer(t, busy.Addr().String())
	err = srv2.ListenAndServe(context.Background())
	if err == nil || !errors.As(err, &opErr) || strings.Contains(err.Error(), "close rpc log") {
		t.Fatalf("plain listen failure: %v", err)
	}
}
