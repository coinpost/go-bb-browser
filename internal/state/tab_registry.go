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
// asynchronous evidence about one moment. The registry orders that evidence on
// one clock, on which every window opening, every lifecycle event read from the
// connection and every application of evidence is a distinct instant:
//   - a lifecycle event is evidence about the instant it was READ (its stamp),
//     however late it is handled;
//   - a reply (Target.getTargets, Target.createTarget) is evidence about some
//     instant between the opening of its Window and its application.
//
// "Gone" is permanent once observed (target ids are not reused within a browser
// session); "exists" is only true for its moment. For every known target the
// registry therefore keeps seen, the earliest instant by which it is proven to
// have existed, and never renews it: an enumeration requested after seen that
// does not list the target proves it gone. Absence that cannot be ordered
// against seen proves nothing either way; it always makes the registry ask for
// a fresh enumeration (the verify results below), which can be ordered.
//
// There is no history: the registry holds the known targets and the Windows of
// the requests in flight, nothing else.
type TabRegistry struct {
	mu sync.Mutex

	selected target.ID // empty if none

	clock   uint64
	targets map[target.ID]*knownTarget

	// lastAbsence is the instant of the latest evidence that targets are not
	// there which did not arrive through the lifecycle event queue: an applied
	// enumeration, a removal learned from a command reply (RemoveTarget), or an
	// overflow of the queue (EventDropped). Queued events are checked against
	// it (RegisterEvent).
	lastAbsence uint64

	// windows are the browser requests in flight (see Window).
	windows map[*Window]struct{}
}

// knownTarget is a page target the browser reported and that has not been
// observed gone. It resolves (Has, Lookup) unless it is suspended or doomed.
type knownTarget struct {
	// seen is the earliest instant by which the target is proven to have
	// existed: the read stamp of the event, the application of the enumeration
	// or the commit of the creation that registered it. Never renewed.
	seen uint64
	// firstHand: the daemon created the target (CommitCreated) or a lifecycle
	// event reported it. Otherwise it is a discovery: only enumerations list it.
	firstHand bool
	// suspended: a discovery contradicted by an enumeration that cannot be
	// ordered against it. Nothing says the target is gone, so everything that
	// hangs off it is kept; it merely does not resolve until an enumeration
	// that can be ordered lists it again or proves it gone.
	suspended bool
	// doomed: its destroyed notification was read from the connection. It is
	// gone; only the cleanup, which needs the lifecycle mutex of the daemon, is
	// still pending (RemoveEventTarget or the next ApplyEnumeration).
	doomed bool
}

func (k *knownTarget) resolves() bool { return !k.suspended && !k.doomed }

// Window is one browser request whose reply names targets, from just before it
// is sent until its reply is applied: a full target enumeration (ended by
// ApplyEnumeration) or the creation of a target (ended by CommitCreated). A
// request that fails or is given up must end its window with Abandon.
//
// While a window is open the registry records in it what happened meanwhile,
// because the reply may describe the browser as it was before that:
//   - removed: every target reported gone since the window opened — by a
//     command reply, by an enumeration, or by a destroyed notification at the
//     moment it was read from the connection (NoteEventRead), queued or not;
//   - listed: the page targets that every enumeration applied since the window
//     opened did list (meaningful once narrowed is set).
type Window struct {
	opened   uint64 // clock instant at which the window opened
	open     bool
	removed  map[target.ID]struct{}
	narrowed bool
	listed   map[target.ID]struct{}
}

// RemovedTab is a target the registry stopped knowing because the browser
// reported it gone. Only such a target may have its resources released.
type RemovedTab struct {
	TargetID target.ID
}

func NewTabRegistry() *TabRegistry {
	return &TabRegistry{
		targets: make(map[target.ID]*knownTarget),
		windows: make(map[*Window]struct{}),
	}
}

// tickLocked advances the clock and returns the new instant (caller holds lock).
func (r *TabRegistry) tickLocked() uint64 {
	r.clock++
	return r.clock
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
	w := &Window{opened: r.tickLocked(), open: true}
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.windows)
}

// SyncPageTargets applies infos as an enumeration that is current as of this
// call (window opened and applied in one step): every known target missing
// from it is removed (INV-6). Use OpenWindow + ApplyEnumeration whenever the
// list comes from a browser round trip that other lifecycle changes can overlap.
func (r *TabRegistry) SyncPageTargets(infos []*target.Info) []TabSnapshot {
	tabs, _ := r.SyncPageTargetsDetailed(infos)
	return tabs
}

// SyncPageTargetsDetailed is SyncPageTargets that also reports the removals.
func (r *TabRegistry) SyncPageTargetsDetailed(infos []*target.Info) ([]TabSnapshot, []RemovedTab) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tabs, removed, _ := r.applyLocked(r.openLocked(), infos)
	return tabs, removed
}

// ApplyEnumeration aligns the registry with the reply to the enumeration w was
// opened for, and ends w. The snapshot was taken at some instant between the
// opening of w and now.
//
// A known target the reply does not list:
//   - seen before w opened: removed — it existed, the snapshot is newer, it is
//     gone. The removal is recorded in the windows in flight;
//   - seen after w opened: the two pieces of evidence cannot be ordered (born
//     after this snapshot, or destroyed before it?). verify is set, and
//     meanwhile a first-hand target stays as it is — this is what protects a
//     concurrent tab_new from an earlier, even empty, snapshot — while a
//     discovery is suspended: it stops resolving but is neither removed nor
//     reported, so nothing that belongs to it is released.
//
// A doomed target is removed whether listed or not: its destroyed notification
// was read and this is the pending cleanup.
//
// A listed target:
//   - reported gone while w was open (close reply, destroyed notification, an
//     enumeration that removed it): not registered, the reply predates that;
//   - unknown: registered as a discovery. If an enumeration applied while w was
//     open did not list it, the two snapshots contradict each other in the
//     other application order: it is registered suspended and verify is set;
//   - suspended: resolves again unless an enumeration applied while w was open
//     did not list it (verify is set again);
//   - otherwise left as it is.
//
// removed lists the targets observed gone; tabs the page targets of infos that
// resolve when the call returns. verify asks the caller to request a fresh
// enumeration: it is opened after every seen involved, so it settles what this
// one could not. Applying a window that already ended changes nothing.
func (r *TabRegistry) ApplyEnumeration(w *Window, infos []*target.Info) (tabs []TabSnapshot, removed []RemovedTab, verify bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w == nil {
		w = r.openLocked()
	}
	return r.applyLocked(w, infos)
}

func (r *TabRegistry) applyLocked(w *Window, infos []*target.Info) ([]TabSnapshot, []RemovedTab, bool) {
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
	verify := false
	if live {
		now := r.tickLocked()
		gone := w.removed
		for id, k := range r.targets {
			_, listed := present[id]
			switch {
			case k.doomed, !listed && k.seen < w.opened:
				removed = append(removed, RemovedTab{TargetID: id})
				r.removeLocked(id)
				if gone == nil {
					gone = make(map[target.ID]struct{})
				}
				gone[id] = struct{}{}
			case listed:
			default:
				verify = true
				if !k.firstHand {
					k.suspended = true
				}
			}
		}
		slices.SortFunc(removed, func(a, b RemovedTab) int { return cmp.Compare(a.TargetID, b.TargetID) })

		for id := range present {
			if _, isGone := gone[id]; isGone {
				continue
			}
			contradicted := false
			if w.narrowed {
				_, ok := w.listed[id]
				contradicted = !ok
			}
			k := r.targets[id]
			switch {
			case k == nil:
				r.targets[id] = &knownTarget{seen: now, suspended: contradicted}
				verify = verify || contradicted
			case k.suspended && contradicted:
				verify = true
			case k.suspended:
				k.suspended = false
			}
		}

		// This snapshot is absence evidence for every reply still in flight.
		for o := range r.windows {
			o.narrow(present)
		}
		r.lastAbsence = now
	}

	out := make([]TabSnapshot, 0, len(present))
	for id, info := range present {
		if k := r.targets[id]; k != nil && k.resolves() {
			out = append(out, TabSnapshot{TargetID: id, Title: info.Title, URL: info.URL})
		}
	}
	slices.SortFunc(out, func(a, b TabSnapshot) int { return cmp.Compare(a.TargetID, b.TargetID) })
	return out, removed, verify
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
// exist), and reports whether the target was known.
//
// Every report counts, also for a target that is not known: it is recorded in
// every window in flight, so a reply requested earlier cannot register the
// target. Lifecycle events still queued were read before the reply and cannot
// register it either.
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
	now := r.tickLocked()
	_, known := r.targets[id]
	r.removeLocked(id)
	if outOfQueue {
		r.lastAbsence = now
	}
	return known
}

// CommitCreated registers the target a Target.createTarget reply names and
// ends w. w must have been opened BEFORE the creation was requested: the
// target exists in the browser, and can be listed, used, closed and destroyed,
// before its creator sees the reply.
//
//   - A target reported gone while w was open is not registered, whether or not
//     it is known again right now: the reply predates that removal (INV-6).
//   - A target that is already known (an enumeration or event got there first)
//     keeps its seen, so an enumeration requested after that still removes it
//     when it does not list it. It becomes first-hand — the daemon now knows it
//     was created while w was open — and, if it was suspended, resolves again.
//   - Otherwise it is registered first-hand.
//
// verify asks for a fresh enumeration when absence evidence exists that cannot
// be ordered against the creation: the target was suspended, or an enumeration
// applied while w was open did not list it. Neither is held against the new
// target (the snapshot may predate it), but the browser is asked again.
func (r *TabRegistry) CommitCreated(w *Window, id target.ID) (ok, verify bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	live := w != nil && r.endLocked(w)
	if id == "" {
		return false, false
	}
	if w != nil {
		if _, gone := w.removed[id]; gone {
			return false, false
		}
	}
	if k := r.targets[id]; k != nil {
		if k.doomed {
			return false, false
		}
		verify = k.suspended
		k.firstHand = true
		k.suspended = false
		return true, verify
	}
	if w != nil && !live {
		// The window ended before: nothing recorded what happened since.
		return false, false
	}
	r.targets[id] = &knownTarget{seen: r.tickLocked(), firstHand: true}
	if w != nil && w.narrowed {
		_, listed := w.listed[id]
		verify = !listed
	}
	return true, verify
}

// NoteEventRead is called for every lifecycle event at the moment it is read
// from the browser connection, in connection order and before the event is
// queued. It returns the event's stamp: its instant on the registry clock.
//
// destroyed is the target a Target.targetDestroyed notification names (empty
// for other events). The report takes effect here, not when the queue gets to
// it: a known target is doomed — it stops resolving at once — and the id is
// recorded as removed in every window in flight, so neither an enumeration that
// still lists it nor the Target.createTarget reply that names it can register
// it. Unregistering is left to the consumer because it also clears daemon
// state under the lifecycle mutex, which the connection reader must not take.
func (r *TabRegistry) NoteEventRead(destroyed target.ID) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	stamp := r.tickLocked()
	if destroyed != "" {
		if k := r.targets[destroyed]; k != nil {
			k.doomed = true
		}
		r.noteRemovedLocked(destroyed)
	}
	return stamp
}

// EventDropped is called on the connection reader when the lifecycle queue was
// full and the event just stamped by NoteEventRead was not queued, BEFORE the
// overflow is made visible to the consumer. What was lost is unknown to the
// queue, so events still queued can no longer register a target that is not
// known (RegisterEvent); the enumeration the overflow requests does that.
func (r *TabRegistry) EventDropped() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastAbsence = r.tickLocked()
}

// RegisterEvent applies a lifecycle event (created, info changed) that reported
// the page target id as existing when it was read from the connection at
// stamp, and reports whether the target is known afterwards. When it is not,
// the caller must request a fresh enumeration.
//
// A target that is already known is left as it is. An unknown one is
// registered first-hand with seen = stamp — the instant the event was read, so
// an enumeration requested after that and not listing it still removes it —
// unless an enumeration, a removal learned from a command reply or a queue
// overflow was applied after stamp: the event is older than that evidence and
// cannot bring an id back (INV-6).
func (r *TabRegistry) RegisterEvent(stamp uint64, id target.ID) (known bool) {
	if id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targets[id]; ok {
		return true
	}
	if r.lastAbsence > stamp {
		return false
	}
	r.targets[id] = &knownTarget{seen: stamp, firstHand: true}
	return true
}

// RegisterPageTarget registers id first-hand as existing now, for callers that
// hold current evidence.
func (r *TabRegistry) RegisterPageTarget(id target.ID) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targets[id]; ok {
		return
	}
	r.targets[id] = &knownTarget{seen: r.tickLocked(), firstHand: true}
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

// removeLocked forgets id and records that it was reported gone in every
// window in flight (caller holds lock).
func (r *TabRegistry) removeLocked(id target.ID) {
	delete(r.targets, id)
	if r.selected == id {
		r.selected = ""
	}
	r.noteRemovedLocked(id)
}

func (r *TabRegistry) noteRemovedLocked(id target.ID) {
	for w := range r.windows {
		if w.removed == nil {
			w.removed = make(map[target.ID]struct{})
		}
		w.removed[id] = struct{}{}
	}
}

// Has reports whether id resolves: a known page target that is neither
// suspended nor doomed.
func (r *TabRegistry) Has(id target.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.targets[id]
	return k != nil && k.resolves()
}

// Known reports whether the registry holds id at all, resolving or not. State
// that hangs off a target (idle tracking, managed records, buffers, the
// browser attachment) lives as long as the target is known: a suspended target
// keeps all of it.
func (r *TabRegistry) Known(id target.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.targets[id] != nil
}

// Lookup resolves a client-supplied tab id (INV-3). The id must be the complete
// CDP target id of a resolving page target, byte for byte: it is not trimmed,
// case-folded, or matched by prefix/suffix, and nothing is substituted when it
// does not match.
func (r *TabRegistry) Lookup(tab string) (target.ID, bool) {
	id := target.ID(tab)
	if !r.Has(id) {
		return "", false
	}
	return id, true
}

// Select marks id as the focused tab or returns false if it does not resolve.
func (r *TabRegistry) Select(id target.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := r.targets[id]; k == nil || !k.resolves() {
		return false
	}
	r.selected = id
	return true
}

// Selected returns the focused target id, or "" when there is none or it does
// not resolve at the moment.
func (r *TabRegistry) Selected() target.ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := r.targets[r.selected]; k == nil || !k.resolves() {
		return ""
	}
	return r.selected
}
