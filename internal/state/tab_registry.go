package state

import (
	"slices"
	"sync"

	"github.com/chromedp/cdproto/target"
)

// TabRegistry is the set of page targets the daemon knows about (INV-3).
//
// A tab is identified on every layer by its complete CDP target id, used
// verbatim: the registry derives no alias from it and never normalizes it, so
// an id either names exactly one registered target or nothing at all.
type TabRegistry struct {
	mu sync.RWMutex

	selected target.ID // empty if none

	// gen is a monotonic registration generation. targetGen holds every
	// registered target with the generation at which it was last registered
	// (or explicitly re-registered), so a full enumeration requested earlier
	// cannot remove it.
	gen       uint64
	targetGen map[target.ID]uint64
}

// EnumerationToken marks the moment a full target enumeration was requested.
// The zero value is the oldest possible token: applying a snapshot with it
// registers targets but never removes one.
type EnumerationToken struct {
	gen uint64
}

type RemovedTab struct {
	TargetID target.ID
}

func NewTabRegistry() *TabRegistry {
	return &TabRegistry{
		targetGen: make(map[target.ID]uint64),
	}
}

// BeginEnumeration must be called BEFORE the browser is asked for its target
// list. The returned token makes ApplyEnumeration ignore the absence of any
// target registered after this point: such a target may have been created
// after the browser took the snapshot, so the snapshot says nothing about it.
func (r *TabRegistry) BeginEnumeration() EnumerationToken {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return EnumerationToken{gen: r.gen}
}

// SyncPageTargets updates registration from the current set of page-type targets.
// Targets that disappeared are removed (INV-6: the id stops resolving).
func (r *TabRegistry) SyncPageTargets(infos []*target.Info) []TabSnapshot {
	tabs, _ := r.SyncPageTargetsDetailed(infos)
	return tabs
}

// SyncPageTargetsDetailed also reports removals so callers can clear every
// target-scoped resource through one lifecycle path (INV-6).
//
// infos is treated as current as of this call, so every registered target
// missing from it is removed. Use BeginEnumeration + ApplyEnumeration whenever
// the list comes from a browser round trip that other registrations can overlap.
func (r *TabRegistry) SyncPageTargetsDetailed(infos []*target.Info) ([]TabSnapshot, []RemovedTab) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applyLocked(EnumerationToken{gen: r.gen}, infos)
}

// ApplyEnumeration aligns the registry with a snapshot requested at tok.
// Targets missing from infos are removed (and reported) only if they were last
// registered at or before tok; targets registered later are kept untouched.
// The returned snapshots describe exactly the page targets in infos.
func (r *TabRegistry) ApplyEnumeration(tok EnumerationToken, infos []*target.Info) ([]TabSnapshot, []RemovedTab) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applyLocked(tok, infos)
}

func (r *TabRegistry) applyLocked(tok EnumerationToken, infos []*target.Info) ([]TabSnapshot, []RemovedTab) {

	present := make(map[target.ID]struct{}, len(infos))
	for _, info := range infos {
		if info == nil || info.TargetID == "" || !isPageTarget(info) {
			continue
		}
		present[info.TargetID] = struct{}{}
	}

	var removed []RemovedTab
	for id, gen := range r.targetGen {
		if _, ok := present[id]; ok {
			continue
		}
		if gen > tok.gen {
			// Registered after the enumeration was requested: the snapshot may
			// simply predate this target. A later enumeration or its lifecycle
			// event removes it if it is really gone.
			continue
		}
		removed = append(removed, RemovedTab{TargetID: id})
		r.removeLocked(id)
	}

	ids := make([]target.ID, 0, len(present))
	for id := range present {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b target.ID) int {
		if x, y := string(a), string(b); x < y {
			return -1
		} else if x > y {
			return 1
		}
		return 0
	})
	out := make([]TabSnapshot, 0, len(ids))
	for _, id := range ids {
		r.ensureLocked(id)
		out = append(out, TabSnapshot{
			TargetID: id,
			Title:    tabTitle(infos, id),
			URL:      tabURL(infos, id),
		})
	}
	return out, removed
}

// RemoveTarget removes one target and reports whether it was registered.
func (r *TabRegistry) RemoveTarget(id target.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targetGen[id]; !ok {
		return false
	}
	r.removeLocked(id)
	return true
}

// RegisterPageTarget registers a CDP target id without syncing from the browser.
// Use after Target.createTarget when GetTargets may not yet list the new tab.
// It always advances the target's generation, also when the target is already
// registered, so enumerations already in flight cannot remove it.
func (r *TabRegistry) RegisterPageTarget(id target.ID) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.targetGen[id] = r.gen
}

func tabTitle(infos []*target.Info, id target.ID) string {
	for _, info := range infos {
		if info != nil && info.TargetID == id {
			return info.Title
		}
	}
	return ""
}

func tabURL(infos []*target.Info, id target.ID) string {
	for _, info := range infos {
		if info != nil && info.TargetID == id {
			return info.URL
		}
	}
	return ""
}

// isPageTarget matches top-level browsing targets we expose as tabs.
func isPageTarget(info *target.Info) bool {
	if info == nil {
		return false
	}
	switch info.Type {
	case "page", "tab":
		return true
	default:
		return false
	}
}

// TabSnapshot is one row for API responses after sync.
type TabSnapshot struct {
	TargetID target.ID
	Title    string
	URL      string
}

func (r *TabRegistry) removeLocked(id target.ID) {
	delete(r.targetGen, id)
	if r.selected == id {
		r.selected = ""
	}
}

// ensureLocked registers id with a fresh generation unless it is already
// registered (caller holds lock).
func (r *TabRegistry) ensureLocked(id target.ID) {
	if _, ok := r.targetGen[id]; ok {
		return
	}
	r.gen++
	r.targetGen[id] = r.gen
}

// Has reports whether id is a registered page target.
func (r *TabRegistry) Has(id target.ID) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id == "" {
		return false
	}
	_, ok := r.targetGen[id]
	return ok
}

// Lookup resolves a client-supplied tab id (INV-3). The id must be the complete
// CDP target id of a registered page target, byte for byte: it is not trimmed,
// case-folded, or matched by prefix/suffix, and nothing is substituted when it
// does not match.
func (r *TabRegistry) Lookup(tab string) (target.ID, bool) {
	id := target.ID(tab)
	if !r.Has(id) {
		return "", false
	}
	return id, true
}

// Select marks id as the focused tab or returns false if it is not registered.
func (r *TabRegistry) Select(id target.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == "" {
		return false
	}
	if _, ok := r.targetGen[id]; !ok {
		return false
	}
	r.selected = id
	return true
}

// Selected returns the current focus target id, or "".
func (r *TabRegistry) Selected() target.ID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.selected
}
