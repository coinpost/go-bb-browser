package store

import (
	"sort"
	"time"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// ManagedTab is one daemon-created tab as rpc.jsonl records it.
type ManagedTab struct {
	// Target is the full CDP target id: the tab's identity. Short ids are only
	// unique among the tabs open at one moment and can be handed to a different
	// tab by the next run, so nothing is recovered by short id alone.
	Target string
	// Short is the short id the tab had when the record was written.
	Short        string
	LastActivity time.Time
}

// managedLog is the managed-tab state replayed from rpc.jsonl.
type managedLog struct {
	// byTarget holds tabs whose records carry the full target id.
	byTarget map[string]ManagedTab
	// legacy holds tabs known by short id only: lines written before records
	// carried a target id. Their owner cannot be told from the log.
	legacy map[string]time.Time
}

func newManagedLog() *managedLog {
	return &managedLog{byTarget: make(map[string]ManagedTab), legacy: make(map[string]time.Time)}
}

func (m *managedLog) clone() *managedLog {
	out := newManagedLog()
	for k, v := range m.byTarget {
		out.byTarget[k] = v
	}
	for k, v := range m.legacy {
		out.legacy[k] = v
	}
	return out
}

// tabs returns the full-id entries sorted by target id.
func (m *managedLog) tabs() []ManagedTab {
	out := make([]ManagedTab, 0, len(m.byTarget))
	for _, tab := range m.byTarget {
		out = append(out, tab)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// shorts returns last activity by short id for every entry, either format.
func (m *managedLog) shorts() map[string]time.Time {
	out := make(map[string]time.Time, len(m.byTarget)+len(m.legacy))
	for short, at := range m.legacy {
		out[short] = at
	}
	for _, tab := range m.byTarget {
		out[tab.Short] = tab.LastActivity
	}
	return out
}

// apply folds one log record into the managed-tab state: tab_new registers a
// tab, tab_close releases it, and other tab-related methods refresh its
// last-activity time. A record that names a target only ever touches that
// target's entry; a record without one only touches a short-id (legacy) entry.
// So a line about one tab can never release or renew another that held the same
// short id, whatever order the two tabs' lines were written in.
func (m *managedLog) apply(rec LogRecord) {
	if m == nil || !rec.OK {
		return
	}
	tab := rec.Tab
	if tab == "" {
		tab = tabFromRequestBody(rec.Body)
	}
	switch rec.Action {
	case protocol.MethodTabNew:
		if rec.Target != "" {
			m.byTarget[rec.Target] = ManagedTab{Target: rec.Target, Short: tab, LastActivity: rec.Time}
		} else if tab != "" {
			m.legacy[tab] = rec.Time
		}
	case protocol.MethodTabClose:
		if rec.Target != "" {
			delete(m.byTarget, rec.Target)
		} else if tab != "" {
			delete(m.legacy, tab)
		}
	default:
		if !protocol.IsTabRelatedMethod(rec.Action) || rec.Action == protocol.MethodTabList {
			return
		}
		if rec.Target != "" {
			if cur, ok := m.byTarget[rec.Target]; ok {
				cur.LastActivity = rec.Time
				m.byTarget[rec.Target] = cur
			}
		} else if _, ok := m.legacy[tab]; ok {
			m.legacy[tab] = rec.Time
		}
	}
}
