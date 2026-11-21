package store

import (
	"time"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// managedTab is the replayed state of one daemon-created tab.
type managedTab struct {
	at    time.Time // last activity
	short string    // last short id seen for it; audit only, never an identity
}

// applyManagedUpdate folds one log record into the managed-tab activity map:
// tab_new registers a tab, tab_close releases it, and other tab-related methods
// refresh its last-activity time.
//
// The map is keyed by the full CDP target id recorded with the request. A
// record without one is skipped: its short id may name a different tab by the
// time the log is replayed, so it must neither grant nor refresh nor revoke
// managed status.
func applyManagedUpdate(managed map[string]managedTab, rec LogRecord) {
	if managed == nil || !rec.OK || rec.Target == "" {
		return
	}
	short := rec.Tab
	if short == "" {
		short = tabFromRequestBody(rec.Body)
	}
	switch rec.Action {
	case protocol.MethodTabNew:
		managed[rec.Target] = managedTab{at: rec.Time, short: short}
	case protocol.MethodTabClose:
		delete(managed, rec.Target)
	default:
		if !protocol.IsTabRelatedMethod(rec.Action) || rec.Action == protocol.MethodTabList {
			return
		}
		if tab, isManaged := managed[rec.Target]; isManaged {
			tab.at = rec.Time
			if short != "" {
				tab.short = short
			}
			managed[rec.Target] = tab
		}
	}
}
