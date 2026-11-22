package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/runtime"
)

// transportKeywords are the message fragments the untyped fallback treats as a
// lost CDP transport. Page-controlled text containing them must never escalate.
var transportKeywords = []string{
	"eof",
	"EOF",
	"connection refused",
	"connection reset",
	"broken pipe",
	"use of closed network connection",
	"websocket",
	"WebSocket",
	"channel closed",
	"could not dial",
}

func TestErrorKindClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want ErrorKind
	}{
		"nil":         {nil, ErrorRequest},
		"timeout":     {context.DeadlineExceeded, ErrorTimeout},
		"canceled":    {context.Canceled, ErrorRequest},
		"target gone": {errors.New("No target with given id"), ErrorTargetGone},
		"transport":   {errors.New("websocket: close 1006"), ErrorTransport},
		"request":     {errors.New("invalid selector"), ErrorRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ErrorKindOf(tc.err); got != tc.want {
				t.Fatalf("kind = %v, want %v", got, tc.want)
			}
		})
	}
}

func pageException(desc string) *runtime.ExceptionDetails {
	return &runtime.ExceptionDetails{Text: "Uncaught", Exception: &runtime.RemoteObject{Description: desc}}
}

// errorForms returns err as callers may see it: raw, fmt-wrapped, and wrapped
// by the Session boundary helper.
func errorForms(err error) map[string]error {
	return map[string]error{
		"raw":       err,
		"wrapped":   fmt.Errorf("eval adapter: %w", err),
		"cdp typed": wrapCDPError("Runtime.evaluate", "T1", err),
		"cdp typed then wrapped": fmt.Errorf("outer: %w",
			wrapCDPError("Runtime.evaluate", "T1", fmt.Errorf("inner: %w", err))),
	}
}

func TestPageExceptionIsNeverTransport(t *testing.T) {
	descs := []string{
		"InvalidStateError: Failed to execute 'send' on 'WebSocket': Still in CONNECTING state.",
		"Error: unexpected EOF while parsing feed",
		"SyntaxError: Failed to execute 'querySelector' on 'Document': 'div[data-kind=websocket' is not a valid selector.",
		// Target-gone wording is page-controlled too and must stay request-local.
		"Error: Target closed",
	}
	for _, kw := range transportKeywords {
		descs = append(descs, "Error: page says "+kw)
	}
	for _, desc := range descs {
		for form, err := range errorForms(pageException(desc)) {
			t.Run(desc+"/"+form, func(t *testing.T) {
				if got := ErrorKindOf(err); got != ErrorRequest {
					t.Fatalf("kind = %v, want ErrorRequest for %v", got, err)
				}
			})
		}
	}
	// The exception text itself (no remote object) is equally untrusted.
	bare := &runtime.ExceptionDetails{Text: "websocket EOF broken pipe"}
	if got := ErrorKindOf(bare); got != ErrorRequest {
		t.Fatalf("bare exception kind = %v, want ErrorRequest", got)
	}
}

func TestProtocolErrorIsNeverTransport(t *testing.T) {
	for _, kw := range transportKeywords {
		perr := &cdproto.Error{Code: -32000, Message: "Cannot find context: " + kw}
		for form, err := range errorForms(perr) {
			t.Run(kw+"/"+form, func(t *testing.T) {
				if got := ErrorKindOf(err); got != ErrorRequest {
					t.Fatalf("kind = %v, want ErrorRequest for %v", got, err)
				}
			})
		}
	}
}

func TestProtocolErrorKeepsTargetGone(t *testing.T) {
	for _, msg := range []string{
		"No target with given id found",
		"No session with given id",
		"Target closed",
		// Target-gone wins even when the reply also echoes a transport keyword.
		"No target with given id: websocket",
	} {
		perr := &cdproto.Error{Code: -32602, Message: msg}
		for form, err := range errorForms(perr) {
			t.Run(msg+"/"+form, func(t *testing.T) {
				if got := ErrorKindOf(err); got != ErrorTargetGone {
					t.Fatalf("kind = %v, want ErrorTargetGone for %v", got, err)
				}
			})
		}
	}
}

func TestTypedCDPErrorKindIsAuthoritative(t *testing.T) {
	for _, kind := range []ErrorKind{ErrorRequest, ErrorTimeout, ErrorTargetGone, ErrorTransport} {
		for _, kw := range append([]string{"plain failure", "No target with given id"}, transportKeywords...) {
			typed := &CDPError{Kind: kind, Op: "Runtime.evaluate", Target: "T1", Err: errors.New(kw)}
			for form, err := range map[string]error{"raw": typed, "wrapped": fmt.Errorf("outer: %w", typed)} {
				t.Run(fmt.Sprintf("%d/%s/%s", kind, kw, form), func(t *testing.T) {
					if got := ErrorKindOf(err); got != kind {
						t.Fatalf("kind = %v, want %v for %v", got, kind, err)
					}
				})
			}
		}
	}
}

func TestRealTransportErrorsStayTransport(t *testing.T) {
	errs := map[string]error{
		"io.EOF":             io.EOF,
		"unexpected EOF":     io.ErrUnexpectedEOF,
		"net.ErrClosed":      net.ErrClosed,
		"EPIPE":              &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE},
		"ECONNRESET":         &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
		"ECONNREFUSED":       &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		"closed conn text":   errors.New("write tcp 127.0.0.1:1->127.0.0.1:9222: use of closed network connection"),
		"websocket close":    errors.New("websocket: close 1006 (abnormal closure)"),
		"chromedp dial":      errors.New("could not dial \"ws://127.0.0.1:9222\""),
		"chromedp chan":      errors.New("channel closed"),
		"reset by peer":      errors.New("read: connection reset by peer"),
		"broken pipe text":   errors.New("write: broken pipe"),
		"refused text":       errors.New("dial tcp 127.0.0.1:9222: connect: connection refused"),
		"unexpected eof msg": errors.New("unexpected EOF"),
	}
	for name, base := range errs {
		for form, err := range errorForms(base) {
			t.Run(name+"/"+form, func(t *testing.T) {
				if got := ErrorKindOf(err); got != ErrorTransport {
					t.Fatalf("kind = %v, want ErrorTransport for %v", got, err)
				}
			})
		}
	}
}

func TestWrapCDPErrorPreservesNilAndTyped(t *testing.T) {
	if err := wrapCDPError("Runtime.evaluate", "T1", nil); err != nil {
		t.Fatalf("wrap(nil) = %v", err)
	}
	exc := pageException("Error: websocket EOF")
	err := wrapCDPError("Runtime.evaluate", "T1", exc)
	var typed *CDPError
	if !errors.As(err, &typed) || typed.Kind != ErrorRequest {
		t.Fatalf("wrapped page exception = %#v, want typed ErrorRequest", err)
	}
	var got *runtime.ExceptionDetails
	if !errors.As(err, &got) || got != exc {
		t.Fatal("wrapped error lost the original exception details")
	}
	if again := wrapCDPError("other", "T2", err); again != err {
		t.Fatal("re-wrapping a typed error must keep the original classification")
	}
}
