package store

import (
	"time"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

func isLifecycleAction(action string) bool {
	return action == ActionManagedTabOpen || action == ActionManagedTabRetire
}

// applyManagedUpdate folds one log record into the managed set, which maps the
// complete CDP target id of each managed tab to its last-activity time:
//
//   - managed_tab_open registers (or replaces) the target;
//   - managed_tab_retire removes exactly that target;
//   - any other successful tab-related record (an RPC audit line) refreshes the
//     last-activity time of the managed target named by rec.Target, if any.
//
// Audit lines are written asynchronously and may arrive after the tab's
// lifecycle ended, so they can neither add nor remove membership. The identity
// comes from rec.Target alone: a record without it (written by a daemon that
// named tabs by short alias, or for a request that was never resolved) is
// ignored — neither a legacy alias nor the tab the client asked for in the
// request body proves which target was operated on.
func applyManagedUpdate(managed map[string]time.Time, rec LogRecord) {
	if managed == nil || !rec.OK || rec.Target == "" {
		return
	}
	switch rec.Action {
	case ActionManagedTabOpen:
		managed[rec.Target] = rec.Time
	case ActionManagedTabRetire:
		delete(managed, rec.Target)
	default:
		if !protocol.IsTabRelatedMethod(rec.Action) || rec.Action == protocol.MethodTabList {
			return
		}
		if at, ok := managed[rec.Target]; ok && rec.Time.After(at) {
			managed[rec.Target] = rec.Time
		}
	}
}

// isUnidentifiedTabNew reports a successful tab_new audit line that carries no
// complete target id. Only daemons that named tabs by short alias wrote those;
// Open counts them to warn that the tabs they created are not adopted.
func isUnidentifiedTabNew(rec LogRecord) bool {
	return rec.OK && rec.Action == protocol.MethodTabNew && rec.Target == ""
}
