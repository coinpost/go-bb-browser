package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/yiplee/go-bb-browser/internal/browser"
	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

type selectorRecordingConn struct {
	*fakeConn
	clicked []string
	filled  []string
}

func (c *selectorRecordingConn) Click(_ target.ID, selector string) error {
	c.clicked = append(c.clicked, selector)
	return nil
}

func (c *selectorRecordingConn) Fill(_ target.ID, selector, _ string) error {
	c.filled = append(c.filled, selector)
	return nil
}

// click / fill must resolve @ref with exactly the selector snapshot reports in
// refs (browser.SnapshotRefSelector); the browser-side regression tests assert
// that this selector matches only the element of the latest snapshot.
func TestV1ClickFillResolveRefWithSnapshotSelector(t *testing.T) {
	conn := &selectorRecordingConn{fakeConn: &fakeConn{}}
	srv := newIdleTestServer(conn.fakeConn, time.Minute)
	srv.tabHook = conn
	tab := registerTestTab(srv, target.ID("ABCDEF123456"))

	rec := postRPC(t, srv, rpcReq(protocol.MethodSnapshot, map[string]any{"tab": tab}, 1))
	var snap struct {
		Result protocol.SnapshotResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Result.Refs) == 0 {
		t.Fatalf("snapshot returned no refs: %s", rec.Body.String())
	}
	for ref, want := range snap.Result.Refs {
		if got := browser.SnapshotRefSelector(ref); got != want {
			t.Fatalf("SnapshotRefSelector(%q) = %q, snapshot refs say %q", ref, got, want)
		}
		for _, form := range []string{ref, "@" + ref, " @" + ref + " "} {
			conn.clicked, conn.filled = nil, nil
			postRPC(t, srv, rpcReq(protocol.MethodClick, map[string]any{"tab": tab, "ref": form}, 2))
			postRPC(t, srv, rpcReq(protocol.MethodFill, map[string]any{"tab": tab, "ref": form, "text": "x"}, 3))
			if len(conn.clicked) != 1 || conn.clicked[0] != want {
				t.Errorf("click ref %q used selectors %q, want [%q]", form, conn.clicked, want)
			}
			if len(conn.filled) != 1 || conn.filled[0] != want {
				t.Errorf("fill ref %q used selectors %q, want [%q]", form, conn.filled, want)
			}
		}
	}

	// A plain CSS selector is passed through untouched.
	conn.clicked = nil
	postRPC(t, srv, rpcReq(protocol.MethodClick, map[string]any{"tab": tab, "selector": "#go"}, 4))
	if len(conn.clicked) != 1 || conn.clicked[0] != "#go" {
		t.Errorf("click selector passed as %q", conn.clicked)
	}
}
