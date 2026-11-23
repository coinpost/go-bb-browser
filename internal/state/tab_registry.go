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
//
// The browser is the authority and everything the registry learns from it is
// asynchronous evidence about one moment: a reply (Target.getTargets,
// Target.createTarget) describes some instant between its request and its
// application, a lifecycle event the instant it was read from the connection.
// "Gone" is permanent once observed (target ids are not reused within a browser
// session); "exists" is only true for its moment. The registry therefore orders
// every piece of existence evidence against the removals and absences applied
// after that moment, and it does so without any history: the evidence lives in
// the Windows of the requests that are still in flight and dies with them.
type TabRegistry struct {
	mu sync.RWMutex

	selected target.ID // empty if none

	// gen is the lifecycle clock: it advances on every first registration, every
	// removal report and every applied enumeration. targetGen holds every
	// registered target with the clock value of its first registration; later
	// evidence that the target exists never changes it.
	gen       uint64
	targetGen map[target.ID]uint64

	// discovered marks the registered targets whose only evidence of existence
	// is an enumeration reply (ApplyEnumeration registered them). A target the
	// daemon created itself (CommitCreated), or one a lifecycle event reported,
	// is not in it. See ApplyEnumeration for what the difference decides.
	discovered map[target.ID]struct{}

	// lastAbsence is the clock value of the latest evidence that targets are
	// not there which did not arrive through the lifecycle event queue: an
	// applied enumeration or a removal learned from a command reply
	// (RemoveTarget). Queued events are checked against it (RegisterEvent).
	lastAbsence uint64

	// windows are the browser requests in flight (see Window).
	windows map[*Window]struct{}
}

// Window is one browser request whose reply names targets, from just before it
// is sent until its reply is applied: a full target enumeration (ended by
// ApplyEnumeration) or the creation of a target (ended by CommitCreated). A
// request that fails or is given up must end its window with Abandon.
//
// While a window is open the registry records in it what happened meanwhile,
// because the reply may describe the browser as it was before that:
//   - removed: every target reported gone since the window opened;
//   - removed: also every target whose destroyed notification was read from
//     the connection since the window opened, even if the event queue dropped
//     it (NoteDestroyedRead);
//   - listed: the page targets that every enumeration applied since the window
//     opened did list (meaningful once narrowed is set).
//
// Nothing else is remembered about past removals or snapshots: with no window
// open the registry holds registered targets only.
type Window struct {
	gen      uint64 // lifecycle clock when the window opened
	open     bool
	removed  map[target.ID]struct{}
	narrowed bool
	listed   map[target.ID]struct{}
}

type RemovedTab struct {
	TargetID target.ID
	// Contested is set when the target was unregistered because two overlapping
	// enumerations disagree about it (see ApplyEnumeration), not because it is
	// known to be gone. The caller should request a fresh enumeration, which
	// registers the target again if it is really there.
	Contested bool
}

func NewTabRegistry() *TabRegistry {
	return &TabRegistry{
		targetGen:  make(map[target.ID]uint64),
		discovered: make(map[target.ID]struct{}),
		windows:    make(map[*Window]struct{}),
	}
}

// OpenWindow must be called BEFORE the browser is asked for its target list or
// for a new target. The caller must end the window on every path:
// ApplyEnumeration, CommitCreated or Abandon.
func (r *TabRegistry) OpenWindow() *Window {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.openLocked()
}

func (r *TabRegistry) openLocked() *Window {
	w := &Window{gen: r.gen, open: true}
	r.windows[w] = struct{}{}
	return w
}

// Abandon ends a window whose request produced no reply to apply (error,
// timeout, cancellation). It is a no-op for a window that already ended.
func (r *TabRegistry) Abandon(w *Window) {
	if w == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endLocked(w)
}

// endLocked closes w and reports whether it was open (caller holds lock). The
// evidence w collected stays readable by the caller, which holds the lock, but
// is no longer updated and is released with w.
func (r *TabRegistry) endLocked(w *Window) bool {
	if !w.open {
		return false
	}
	w.open = false
	delete(r.windows, w)
	return true
}

// OpenWindows reports how many windows are in flight. With none, the registry
// keeps no removal or snapshot evidence at all.
func (r *TabRegistry) OpenWindows() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.windows)
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
// missing from it is removed. Use OpenWindow + ApplyEnumeration whenever the
// list comes from a browser round trip that other lifecycle changes can overlap.
func (r *TabRegistry) SyncPageTargetsDetailed(infos []*target.Info) ([]TabSnapshot, []RemovedTab) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applyLocked(r.openLocked(), infos)
}

// ApplyEnumeration aligns the registry with the reply to the enumeration w was
// opened for, and ends w.
//
//   - A registered target the reply does not list is removed (and reported) if
//     it was registered before w opened: the snapshot is newer than a moment at
//     which the target existed, so it is gone.
//   - One registered after w opened is kept when the daemon knows first-hand
//     that it came into being meanwhile: it was created by tab_new
//     (CommitCreated) or reported by a lifecycle event. The snapshot may simply
//     predate it; this is what protects a concurrent tab_new from an earlier
//     snapshot.
//   - One registered after w opened only because another, overlapping
//     enumeration listed it (a discovery) is not protected. The two snapshots
//     contradict each other and cannot be ordered — the target may have been
//     opened after this snapshot or destroyed before it — so, as for an
//     unregistered target below, absence wins: the id stops resolving and the
//     removal is reported as Contested. It is not recorded as a removal in the
//     windows in flight (nothing says the target is gone), so a creation still
//     in flight commits normally, and the enumeration the caller requests
//     after the conflict registers the target again if it is alive.
//   - A listed target that is registered stays as it is.
//   - A listed target that is not registered is registered unless it was
//     reported gone while w was open, or an enumeration applied while w was
//     open did not list it. In both cases the reply may predate that evidence,
//     so it does not prove the target still exists: the id of a closed tab
//     never resolves again because of a late reply (INV-6). The second rule
//     holds whichever request was sent first: the order in which the browser
//     took two overlapping snapshots is unknown, so absence wins; a target that
//     is really there is registered by the next enumeration.
//
// The returned snapshots describe the page targets in infos that are
// registered when the call returns. Applying a window that already ended
// changes nothing and returns only what is registered.
func (r *TabRegistry) ApplyEnumeration(w *Window, infos []*target.Info) ([]TabSnapshot, []RemovedTab) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w == nil {
		w = r.openLocked()
	}
	return r.applyLocked(w, infos)
}

func (r *TabRegistry) applyLocked(w *Window, infos []*target.Info) ([]TabSnapshot, []RemovedTab) {
	live := r.endLocked(w)

	present := make(map[target.ID]*target.Info, len(infos))
	for _, info := range infos {
		if info == nil || info.TargetID == "" || !isPageTarget(info) {
			continue
		}
		if _, dup := present[info.TargetID]; !dup {
			present[info.TargetID] = info
		}
	}

	var removed []RemovedTab
	if live {
		for id, gen := range r.targetGen {
			if _, ok := present[id]; ok {
				continue
			}
			if gen <= w.gen {
				removed = append(removed, RemovedTab{TargetID: id})
				r.removeLocked(id)
				continue
			}
			if _, ok := r.discovered[id]; !ok {
				continue
			}
			// Contradicting snapshots: unregister without declaring the target
			// gone. The narrowing below keeps every enumeration still in flight
			// from registering it.
			removed = append(removed, RemovedTab{TargetID: id, Contested: true})
			r.unregisterLocked(id)
			r.gen++
		}
		slices.SortFunc(removed, func(a, b RemovedTab) int { return cmp.Compare(a.TargetID, b.TargetID) })
	}

	ids := make([]target.ID, 0, len(present))
	for id := range present {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]TabSnapshot, 0, len(ids))
	for _, id := range ids {
		if _, ok := r.targetGen[id]; !ok {
			if !live || !w.vouchesFor(id) {
				continue
			}
			r.gen++
			r.targetGen[id] = r.gen
			r.discovered[id] = struct{}{}
		}
		out = append(out, TabSnapshot{TargetID: id, Title: present[id].Title, URL: present[id].URL})
	}
	if live {
		// This snapshot is absence evidence for every reply still in flight.
		for o := range r.windows {
			o.narrow(present)
		}
		r.gen++
		r.lastAbsence = r.gen
	}
	return out, removed
}

// vouchesFor reports whether the reply of w can still be trusted that id, which
// it lists and which is not registered, exists.
func (w *Window) vouchesFor(id target.ID) bool {
	if _, gone := w.removed[id]; gone {
		return false
	}
	if w.narrowed {
		if _, ok := w.listed[id]; !ok {
			return false
		}
	}
	return true
}

// narrow intersects w.listed with the page targets of an enumeration applied
// while w is open.
func (w *Window) narrow(present map[target.ID]*target.Info) {
	if !w.narrowed {
		w.narrowed = true
		w.listed = make(map[target.ID]struct{}, len(present))
		for id := range present {
			w.listed[id] = struct{}{}
		}
		return
	}
	for id := range w.listed {
		if _, ok := present[id]; !ok {
			delete(w.listed, id)
		}
	}
}

// RemoveTarget applies the report that id is gone, learned from a command
// reply (Target.closeTarget succeeded or answered that the target does not
// exist), and reports whether it was registered.
//
// Every report counts, also for a target that is not registered: it is recorded
// in every window in flight, so a reply requested earlier cannot register the
// target, whether the registry never saw it or already removed it (tab_close
// confirmed, then the destroyed notification arrives after an enumeration that
// still lists the target was requested). Lifecycle events still queued were
// read before the reply and cannot register it either.
func (r *TabRegistry) RemoveTarget(id target.ID) bool {
	return r.remove(id, true)
}

// RemoveEventTarget is RemoveTarget for a report that arrived through the
// lifecycle event queue (target destroyed, or reported as not a page). The
// queue is ordered, so events about the target that the browser sent earlier
// have been handled already and later ones are newer than this report: other
// queued events are not invalidated by it.
func (r *TabRegistry) RemoveEventTarget(id target.ID) bool {
	return r.remove(id, false)
}

func (r *TabRegistry) remove(id target.ID, outOfQueue bool) bool {
	if id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, registered := r.targetGen[id]
	if registered {
		r.removeLocked(id)
	} else {
		r.noteRemovedLocked(id)
	}
	if outOfQueue {
		r.lastAbsence = r.gen
	}
	return registered
}

// CommitCreated registers the target a Target.createTarget reply names, ends w
// and reports whether the target is registered. w must have been opened BEFORE
// the creation was requested: the target exists in the browser, and can be
// listed, used, closed and destroyed, before its creator sees the reply.
//
//   - A target reported gone while w was open is not registered, whether or not
//     it is registered again right now: the reply predates that removal (INV-6).
//   - A target that is already registered (an enumeration or event got there
//     first) stays registered. Its registration is not renewed, so an
//     enumeration requested after it was first registered still removes it when
//     it no longer lists it. It is no longer a mere discovery, though: the
//     daemon now knows the target was created while w was open, so an
//     enumeration requested before it was first registered does not remove it.
//   - Otherwise it is registered. An enumeration applied meanwhile that did not
//     list it is not held against it: that snapshot may predate the creation.
func (r *TabRegistry) CommitCreated(w *Window, id target.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	live := w != nil && r.endLocked(w)
	if id == "" {
		return false
	}
	if w != nil {
		if _, gone := w.removed[id]; gone {
			return false
		}
	}
	if _, ok := r.targetGen[id]; ok {
		delete(r.discovered, id)
		return true
	}
	if w != nil && !live {
		// The window ended before: nothing recorded what happened since.
		return false
	}
	r.gen++
	r.targetGen[id] = r.gen
	return true
}

// NoteDestroyedRead records that the browser reported id destroyed, at the
// moment the notification is read from the connection and whether or not it
// then found room in the lifecycle event queue (queued).
//
// The report is recorded in every window in flight, exactly as a removal is: a
// reply requested before this moment may describe the browser as it was before
// the target went away, so it cannot register the target — neither an
// enumeration that still lists it nor the Target.createTarget reply that names
// it. This does not wait for the queue, and it is the only trace a notification
// leaves when the queue is full and drops it: without it a tab created and
// destroyed during an overflow would be committed by its late creation reply
// although the resync that follows the overflow never listed it.
//
// The registration itself is not touched here, because unregistering also
// clears daemon state under the lifecycle mutex and this runs on the
// connection reader: a queued notification is applied by the consumer
// (RemoveEventTarget), a dropped one by the resync the overflow requests, which
// is requested after this moment and therefore removes the target. A dropped
// notification is also evidence from outside the queue that events still
// queued predate, so those cannot register an unregistered target any more
// (see RegisterEvent).
func (r *TabRegistry) NoteDestroyedRead(id target.ID, queued bool) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noteRemovedLocked(id)
	if !queued {
		r.lastAbsence = r.gen
	}
}

// EventStamp is the lifecycle clock value to attach to a lifecycle event at the
// moment it is read from the browser connection (see RegisterEvent).
func (r *TabRegistry) EventStamp() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gen
}

// RegisterEvent registers the page target a lifecycle event (created, info
// changed) reported as existing when it was read from the connection at stamp,
// and reports whether the target is registered.
//
// Events wait in a queue, so by the time one is handled newer evidence may
// have been applied. An event for a target that is not registered is dropped
// when an enumeration, a removal learned from a command reply, or a destroyed
// notification the queue had to drop (NoteDestroyedRead) was applied after its
// stamp: it is older than that evidence and cannot bring an id back
// (INV-6). The connection delivers in order, so everything the browser sent
// before a reply carries a stamp from before that reply was applied; removals
// reported by events are ordered by the queue itself. A false result for a
// non-empty id therefore means "not known to exist any more": the caller should
// ask for a fresh enumeration, which registers the target if it is really
// there. A registered target is left as it is.
func (r *TabRegistry) RegisterEvent(stamp uint64, id target.ID) bool {
	if id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targetGen[id]; ok {
		return true
	}
	if r.lastAbsence > stamp {
		return false
	}
	r.gen++
	r.targetGen[id] = r.gen
	return true
}

// RegisterPageTarget registers id as existing now. It is RegisterEvent with a
// current stamp, for callers that hold first-hand, current evidence.
func (r *TabRegistry) RegisterPageTarget(id target.ID) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targetGen[id]; ok {
		return
	}
	r.gen++
	r.targetGen[id] = r.gen
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

// removeLocked unregisters id and records the removal (caller holds lock; id
// must be registered).
func (r *TabRegistry) removeLocked(id target.ID) {
	r.unregisterLocked(id)
	r.noteRemovedLocked(id)
}

// unregisterLocked makes id stop resolving without recording that it is gone
// (caller holds lock).
func (r *TabRegistry) unregisterLocked(id target.ID) {
	delete(r.targetGen, id)
	delete(r.discovered, id)
	if r.selected == id {
		r.selected = ""
	}
}

// noteRemovedLocked records that id was reported gone now, in every window in
// flight (caller holds lock).
func (r *TabRegistry) noteRemovedLocked(id target.ID) {
	r.gen++
	for w := range r.windows {
		if w.removed == nil {
			w.removed = make(map[target.ID]struct{})
		}
		w.removed[id] = struct{}{}
	}
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
