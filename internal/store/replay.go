package store

import (
	"time"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// managedEntry is one managed tab, keyed in the managed map by full target id.
type managedEntry struct {
	tab string // short id at registration time (alias only)
	at  time.Time
}

func isLifecycleAction(action string) bool {
	return action == ActionManagedTabOpen || action == ActionManagedTabRetire
}

// applyManagedUpdate folds one log record into the managed set, which is keyed
// by full CDP target id:
//
//   - managed_tab_open registers (or replaces) the target;
//   - managed_tab_retire removes exactly that target;
//   - any other successful tab-related record (an RPC audit line) refreshes the
//     last-activity time of the managed target named by rec.Target, if any.
//
// Audit lines are written asynchronously and may arrive after the tab's
// lifecycle ended or its short id was reused, so they can neither add nor
// remove membership, and a record without a target id (including tab_new /
// tab_close lines written by versions that only stored the short id) is
// ignored: an alias cannot prove which tab it meant.
func applyManagedUpdate(managed map[string]managedEntry, rec LogRecord) {
	if managed == nil || !rec.OK || rec.Target == "" {
		return
	}
	switch rec.Action {
	case ActionManagedTabOpen:
		managed[rec.Target] = managedEntry{tab: rec.Tab, at: rec.Time}
	case ActionManagedTabRetire:
		delete(managed, rec.Target)
	default:
		if !protocol.IsTabRelatedMethod(rec.Action) || rec.Action == protocol.MethodTabList {
			return
		}
		if ent, ok := managed[rec.Target]; ok && rec.Time.After(ent.at) {
			ent.at = rec.Time
			managed[rec.Target] = ent
		}
	}
}

// trackLegacyManaged mirrors the pre-identity replay rules (tab_new adds a
// short id, tab_close removes it) for records that carry no target id, only so
// Open can report how many unverifiable managed tabs it is not restoring.
func trackLegacyManaged(legacy map[string]struct{}, rec LogRecord) {
	if !rec.OK || rec.Target != "" {
		return
	}
	tab := rec.Tab
	if tab == "" {
		tab = tabFromRequestBody(rec.Body)
	}
	if tab == "" {
		return
	}
	switch rec.Action {
	case protocol.MethodTabNew:
		legacy[tab] = struct{}{}
	case protocol.MethodTabClose:
		delete(legacy, tab)
	}
}
