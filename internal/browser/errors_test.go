package browser

import (
	"context"
	"errors"
	"testing"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/runtime"
)

func TestErrorKindClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want ErrorKind
	}{
		"timeout":     {context.DeadlineExceeded, ErrorTimeout},
		"target gone": {errors.New("No target with given id"), ErrorTargetGone},
		"transport":   {errors.New("websocket: close 1006"), ErrorTransport},
		"request":     {errors.New("invalid selector"), ErrorRequest},
		// Replies delivered by the browser must not be read as transport loss,
		// whatever text the page or protocol put in them.
		"page exception mentioning websocket": {&runtime.ExceptionDetails{
			Text:      "Uncaught",
			Exception: &runtime.RemoteObject{Description: "Error: WebSocket is closed\n    at https://ex/websocket.js:1:1"},
		}, ErrorRequest},
		"page exception mentioning typeof": {&runtime.ExceptionDetails{
			Text:      "Uncaught",
			Exception: &runtime.RemoteObject{Description: "TypeError: typeof x is not a function"},
		}, ErrorRequest},
		"wrapped page exception": {wrapCDPError("Runtime.evaluate", "T1", &runtime.ExceptionDetails{
			Text: "Uncaught Error: connection reset",
		}), ErrorRequest},
		"protocol error mentioning eof":  {&cdproto.Error{Code: -32000, Message: "Unexpected EOF in selector"}, ErrorRequest},
		"protocol error for gone target": {&cdproto.Error{Code: -32602, Message: "No target with given id found"}, ErrorTargetGone},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ErrorKindOf(tc.err); got != tc.want {
				t.Fatalf("kind = %v, want %v", got, tc.want)
			}
		})
	}
}
