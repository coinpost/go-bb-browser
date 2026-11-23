package state

import (
	"cmp"
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

	// gen is a monotonic lifecycle generation: it advances on every
	// registration and on every removal. targetGen holds every registered
	// target with the generation at which it was last registered (or
	// explicitly re-registered), so a full enumeration requested earlier
	// cannot remove it.
	gen       uint64
	targetGen map[target.ID]uint64

	// Removals are remembered the same way, so an enumeration requested before
	// a target was removed cannot register it again (INV-6). removedGen holds
	// the generation of the latest removal of each recently removed target and
	// removedLog the same removals in order, oldest first, one entry per id: a
	// repeated removal moves the id to the end with a fresh generation. Only
	// the last maxRemovedTargets removed ids are kept; removedFloor is the
	// highest generation among those already forgotten, so an enumeration older
	// than it is known to be unverifiable instead of being trusted.
	removedGen   map[target.ID]uint64
	removedLog   []removedTarget
	removedFloor uint64

	// Every applied enumeration is a lifecycle point too: it confirms that the
	// page targets it does not list were absent from the browser. That also
	// holds for ids that are not registered (closed earlier, or never seen), for
	// which there is no removal to remember, so the applied snapshots themselves
	// are kept: appliedLog holds the page ids of the most recent applications
	// with the generation at which each was applied, oldest first. Consecutive
	// applications listing the same ids share one entry (the latest generation),
	// so the log only grows when the set of page targets changes. Only the last
	// maxAppliedEnumerations entries are kept; appliedFloor is the highest
	// generation among those already forgotten.
	appliedLog   []appliedEnumeration
	appliedFloor uint64
}

// maxRemovedTargets bounds the removal memory. It only has to cover the
// removals that overlap one browser round trip; an enumeration that more
// removals than this overtook registers no target it cannot vouch for.
const maxRemovedTargets = 1024

type removedTarget struct {
	id  target.ID
	gen uint64
}

// maxAppliedEnumerations bounds the memory of applied enumerations. Like
// maxRemovedTargets it only has to cover what overlaps one browser round trip:
// a reply that more target-set changes than this overtook registers no target
// it cannot vouch for.
const maxAppliedEnumerations = 64

type appliedEnumeration struct {
	gen     uint64
	present map[target.ID]struct{}
}

// EnumerationToken marks the moment a browser request was sent whose reply
// names targets: a full target enumeration (BeginEnumeration, ApplyEnumeration)
// or the creation of a target (BeginEnumeration, RegisterCreatedTarget). The
// reply is ordered against every registration, removal and applied enumeration
// that happened after the token was taken.
// The zero value is the oldest possible token: applying a snapshot with it
// never removes a target, and registers only targets that were never removed
// and that no remembered enumeration found absent.
type EnumerationToken struct {
	gen uint64
}

type RemovedTab struct {
	TargetID target.ID
}

func NewTabRegistry() *TabRegistry {
	return &TabRegistry{
		targetGen:  make(map[target.ID]uint64),
		removedGen: make(map[target.ID]uint64),
	}
}

// BeginEnumeration must be called BEFORE the browser is asked for its target
// list. The returned token makes ApplyEnumeration ignore what the snapshot
// says about any target whose registration changed after this point: the
// absence of a target registered since (it may have been created after the
// browser took the snapshot), the presence of a target removed since (the
// snapshot may predate its removal), and the presence of an unregistered target
// that another enumeration applied since did not list (the snapshot may predate
// that one).
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
// A page target in infos that is not registered is registered unless it was
// removed after tok, or an enumeration applied after tok did not list it: such
// a target is neither registered nor returned, so the id of a closed tab never
// resolves again because of a late reply (INV-6).
//
// The second rule is about the window between the request and this call, not
// about which of two requests was sent first: the order in which the browser
// took two overlapping snapshots is unknown, so a snapshot that lists an
// unregistered target and one that does not cannot be ordered. Absence wins
// (the target is not registered); if the target is really there, the next
// enumeration requested after this point, or its lifecycle event, registers it.
// The returned snapshots describe the page targets in infos that are
// registered when the call returns.
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
		if !r.ensureLocked(tok, id) {
			continue
		}
		out = append(out, TabSnapshot{
			TargetID: id,
			Title:    tabTitle(infos, id),
			URL:      tabURL(infos, id),
		})
	}
	// Recorded last, so this snapshot is not held against itself.
	r.noteAppliedLocked(present)
	return out, removed
}

// noteAppliedLocked records that a snapshot listing exactly present was applied
// now (caller holds lock; present is owned by the log from here on).
func (r *TabRegistry) noteAppliedLocked(present map[target.ID]struct{}) {
	r.gen++
	if n := len(r.appliedLog); n > 0 && sameTargets(r.appliedLog[n-1].present, present) {
		// The same absences confirmed again: one entry with the later
		// generation answers every question the two would.
		r.appliedLog[n-1].gen = r.gen
		return
	}
	r.appliedLog = append(r.appliedLog, appliedEnumeration{gen: r.gen, present: present})
	if len(r.appliedLog) > maxAppliedEnumerations {
		r.appliedFloor = r.appliedLog[0].gen
		// Shift instead of re-slicing so the backing array stays bounded and
		// the dropped snapshot is released.
		n := copy(r.appliedLog, r.appliedLog[1:])
		r.appliedLog[n] = appliedEnumeration{}
		r.appliedLog = r.appliedLog[:n]
	}
}

func sameTargets(a, b map[target.ID]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

// foundAbsentSinceLocked reports whether an enumeration applied after tok did
// not list id (caller holds lock).
func (r *TabRegistry) foundAbsentSinceLocked(tok EnumerationToken, id target.ID) bool {
	for i := len(r.appliedLog) - 1; i >= 0 && r.appliedLog[i].gen > tok.gen; i-- {
		if _, ok := r.appliedLog[i].present[id]; !ok {
			return true
		}
	}
	return false
}

// RemoveTarget removes one target and reports whether it was registered.
//
// The removal is remembered either way, and every call counts as a removal that
// happened now: a target the browser reported gone before the registry ever saw
// it must not be registered by an older snapshot, and neither must a target
// that was already removed here (tab_close confirmed) when its destroyed
// notification arrives after an enumeration that still lists it was requested.
// Repeated reports for one id refresh its single slot in the removal memory
// (non-page targets are reported on every info change), so they cannot crowd
// other removals out of it.
func (r *TabRegistry) RemoveTarget(id target.ID) bool {
	if id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targetGen[id]; !ok {
		r.noteRemovedLocked(id)
		return false
	}
	r.removeLocked(id)
	return true
}

// CreatedTarget is the outcome of RegisterCreatedTarget.
type CreatedTarget uint8

const (
	// CreatedRegistered: the target is registered.
	CreatedRegistered CreatedTarget = iota
	// CreatedRemoved: the target was removed after its creation was requested
	// (closed, destroyed, or dropped by an enumeration); it is not registered.
	CreatedRemoved
	// CreatedUnverifiable: removals newer than the creation request have been
	// forgotten, so the registry cannot tell whether this target was among
	// them; it is not registered. A current enumeration settles it.
	CreatedUnverifiable
)

// RegisterCreatedTarget registers the target a Target.createTarget reply names.
// tok must come from BeginEnumeration BEFORE the creation was requested, which
// orders the reply against removals: the target can already have been listed,
// used, closed and destroyed by the time its creator sees the reply, and such
// a reply must not bring the id back (INV-6). A target that is registered
// (again) when the reply arrives stays registered with a fresh generation.
//
// An enumeration applied meanwhile that did not list the target is not held
// against it: that snapshot may simply predate the creation.
func (r *TabRegistry) RegisterCreatedTarget(tok EnumerationToken, id target.ID) CreatedTarget {
	if id == "" {
		return CreatedRemoved
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targetGen[id]; !ok {
		if r.removedGen[id] > tok.gen {
			return CreatedRemoved
		}
		if r.removedFloor > tok.gen {
			return CreatedUnverifiable
		}
	}
	r.gen++
	r.targetGen[id] = r.gen
	return CreatedRegistered
}

// RegisterPageTarget registers a CDP target id without syncing from the browser,
// for a lifecycle event that reports the target as existing now. For the reply
// to a creation request use RegisterCreatedTarget.
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

// removeLocked unregisters id and remembers the removal with a fresh
// generation (caller holds lock; id must be registered).
func (r *TabRegistry) removeLocked(id target.ID) {
	delete(r.targetGen, id)
	if r.selected == id {
		r.selected = ""
	}
	r.noteRemovedLocked(id)
}

// noteRemovedLocked records that id was removed now (caller holds lock). The
// memory holds one entry per id: an earlier removal of the same id is replaced,
// not kept next to the new one, so removedLog stays in generation order with
// exactly the ids of removedGen.
func (r *TabRegistry) noteRemovedLocked(id target.ID) {
	if prev, ok := r.removedGen[id]; ok {
		// Superseded, not forgotten: removedFloor is unaffected.
		if i, found := slices.BinarySearchFunc(r.removedLog, prev, func(e removedTarget, gen uint64) int {
			return cmp.Compare(e.gen, gen)
		}); found {
			r.removedLog = slices.Delete(r.removedLog, i, i+1)
		}
	}
	r.gen++
	r.removedGen[id] = r.gen
	r.removedLog = append(r.removedLog, removedTarget{id: id, gen: r.gen})
	if len(r.removedLog) > maxRemovedTargets {
		oldest := r.removedLog[0]
		// Shift instead of re-slicing so the backing array stays bounded.
		r.removedLog = r.removedLog[:copy(r.removedLog, r.removedLog[1:])]
		// The log is in generation order, so this is the highest generation
		// forgotten so far.
		r.removedFloor = oldest.gen
		delete(r.removedGen, oldest.id)
	}
}

// ensureLocked makes sure id, listed by an enumeration requested at tok, is
// registered, and reports whether it is (caller holds lock). An unregistered
// id is registered with a fresh generation unless it was removed after tok
// or an enumeration applied after tok did not list it: the snapshot may
// predate that removal or that absence, so it does not prove the target
// still exists. When removals or applied enumerations newer than tok have
// already been forgotten (tok is older than removedFloor / appliedFloor) that
// cannot be ruled out for any id, so nothing is registered; a current
// enumeration or the target's lifecycle event registers a target that is
// really there.
func (r *TabRegistry) ensureLocked(tok EnumerationToken, id target.ID) bool {
	if _, ok := r.targetGen[id]; ok {
		return true
	}
	if r.removedGen[id] > tok.gen || r.removedFloor > tok.gen {
		return false
	}
	if r.appliedFloor > tok.gen || r.foundAbsentSinceLocked(tok, id) {
		return false
	}
	r.gen++
	r.targetGen[id] = r.gen
	return true
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
