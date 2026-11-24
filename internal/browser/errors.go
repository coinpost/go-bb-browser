package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
)

// ErrorKind separates transport failures from target-local and request-local
// failures. Only ErrorTransport is an immediate daemon-fatal condition.
type ErrorKind uint8

const (
	ErrorRequest ErrorKind = iota
	ErrorTimeout
	ErrorTargetGone
	ErrorTransport
)

type CDPError struct {
	Kind   ErrorKind
	Op     string
	Target target.ID
	Err    error
}

func (e *CDPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	where := e.Op
	if e.Target != "" {
		where += " target=" + string(e.Target)
	}
	if where == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %v", where, e.Err)
}

func (e *CDPError) Unwrap() error { return e.Err }

func wrapCDPError(op string, tid target.ID, err error) error {
	if err == nil {
		return nil
	}
	var typed *CDPError
	if errors.As(err, &typed) {
		return err
	}
	return &CDPError{Kind: ErrorKindOf(err), Op: op, Target: tid, Err: err}
}

// ErrorKindOf classifies typed and raw chromedp/CDP errors.
//
// Trusted types come first; message text is consulted only for errors that
// carry no type information, because page scripts control the text of JS
// exceptions (and of selectors echoed back in them) and must not be able to
// make a request-local failure look like a lost transport.
func ErrorKindOf(err error) ErrorKind {
	if err == nil {
		return ErrorRequest
	}
	var typed *CDPError
	if errors.As(err, &typed) {
		return typed.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ErrorRequest
	}
	// A JS exception was delivered over a working connection; its text is
	// entirely page-controlled.
	var exc *runtime.ExceptionDetails
	if errors.As(err, &exc) {
		return ErrorRequest
	}
	// A protocol-level error reply also proves the transport is alive. It may
	// still report that the addressed target/session no longer exists.
	var perr *cdproto.Error
	if errors.As(err, &perr) {
		if isTargetGoneMessage(strings.ToLower(perr.Message)) {
			return ErrorTargetGone
		}
		return ErrorRequest
	}
	if isTransportError(err) {
		return ErrorTransport
	}
	// Compatibility fallback for untyped errors (chromedp and the websocket
	// layer mostly return plain strings).
	msg := strings.ToLower(err.Error())
	switch {
	case isTargetGoneMessage(msg):
		return ErrorTargetGone
	case isTransportMessage(msg):
		return ErrorTransport
	default:
		return ErrorRequest
	}
}

// ConfirmedTargetAbsent reports whether err proves that the target itself
// (identified by its complete target.ID on a target-ID-addressed CDP command)
// no longer exists in the browser. Session-only errors and page-session
// execution failures return ("", false) so they are never mistaken for target
// destruction.
func ConfirmedTargetAbsent(err error) (target.ID, bool) {
	var typed *CDPError
	if !errors.As(err, &typed) || typed.Kind != ErrorTargetGone || typed.Target == "" || typed.Err == nil {
		return "", false
	}
	switch typed.Op {
	case "Target.attachToTarget", "Target.attachObserver", "Target.closeTarget":
	default:
		return "", false
	}
	if errors.Is(typed.Err, context.Canceled) || errors.Is(typed.Err, context.DeadlineExceeded) {
		return "", false
	}
	var exc *runtime.ExceptionDetails
	if errors.As(typed.Err, &exc) {
		return "", false
	}
	var perr *cdproto.Error
	if errors.As(typed.Err, &perr) {
		return typed.Target, isTargetAbsentMessage(strings.ToLower(perr.Message))
	}
	return typed.Target, isTargetAbsentMessage(strings.ToLower(typed.Err.Error()))
}

func isTransportError(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

func isTargetAbsentMessage(msg string) bool {
	return strings.Contains(msg, "no target") ||
		strings.Contains(msg, "target not found") ||
		strings.Contains(msg, "target closed") ||
		strings.Contains(msg, "page has been closed") ||
		strings.Contains(msg, "no page") ||
		strings.Contains(msg, "no such target")
}

func isTargetGoneMessage(msg string) bool {
	return isTargetAbsentMessage(msg) ||
		strings.Contains(msg, "no session with given id")
}

func isTransportMessage(msg string) bool {
	return strings.Contains(msg, "eof") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "websocket") ||
		strings.Contains(msg, "channel closed") ||
		strings.Contains(msg, "could not dial")
}
