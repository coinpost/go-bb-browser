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
// Every piece of evidence that a target exists — an event (RegisterEvent), a
// creation reply (CommitCreated), a listing (ApplyEnumeration) — is judged by
// the same rules whether or not the target is already known:
//   - it is refused when the target was reported gone after the instant the
//     evidence is about (doomed, the removed set of its window, a destroyed
//     notification read later, newer absence);
//   - it never renews seen, but an earlier proof lowers it: the target keeps
//     the earliest instant it is proven to have existed at, whichever proof
//     the daemon got to first;
//   - an enumeration that did not list the target and could not be ordered
//     against it is remembered on the target (unlisted) and in the windows it
//     overlapped (listed), so the conflict is settled by the entry that learns
//     the order later: gone when seen turns out to be older, verify otherwise.
//
// Evidence that a target existed is not lost because the target is not known
// yet: a page event read about an unknown target is remembered with its read
// instant (announced) until the target is registered, reported gone, or an
// enumeration requested after that instant is applied. Such an enumeration
// settles it either way — listing it registers it; not listing it proves it
// gone exactly as for a known target, which is recorded in the windows in
// flight so that a creation reply still on its way cannot register it.
//
// There is no history: the registry holds the known targets, the Windows of
// the requests in flight, the destroyed notifications read but not yet handled
// by the queue consumer and the announcements no enumeration has answered yet,
// nothing else.
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

	// destroyedRead holds, per target, the read stamp of its destroyed
	// notification while that notification waits in the lifecycle queue: an
	// "exists" event about the target that was read before it is older than
	// the destruction and registers nothing (RegisterEvent), also when the
	// target was not known when the notification was read. An entry is
	// released when the consumer handles the notification
	// (RemoveDestroyedTarget) and, with all the others, whenever lastAbsence
	// advances, which outdates every event read so far: it is bounded by the
	// destroyed notifications in the queue.
	destroyedRead map[target.ID]uint64

	// announced holds, per target that is NOT known, the earliest instant at
	// which a page event (created, info changed) read from the connection
	// proved it existed. The proof is kept while the event waits in the queue
	// and also when the consumer could not register the target from it
	// (RegisterEvent refused: the caller requests an enumeration), because
	// what it must be compared with may be applied before the target is
	// registered: an enumeration requested after that instant that does not
	// list the target proves it gone (applyLocked). An entry is released when
	// the target becomes known (its instant becomes seen), when the target is
	// reported gone, and by the first applied enumeration that was requested
	// after the instant. Every entry therefore belongs to an event still
	// queued or to an enumeration that event made the daemon request.
	announced map[target.ID]uint64
}

// knownTarget is a page target the browser reported and that has not been
// observed gone. It resolves (Has, Lookup) unless it is suspended or doomed.
type knownTarget struct {
	// seen is the earliest instant by which the target is proven to have
	// existed: the read stamp of an event about it, the application of the
	// enumeration or the commit of the creation that registered it. Never
	// renewed; lowered when an earlier proof is applied later (RegisterEvent).
	seen uint64
	// unlisted is the latest opening instant of an applied enumeration that
	// did not list the target while it could not be ordered against seen
	// (seen >= unlisted always holds for a target that is not doomed). When an
	// earlier proof lowers seen below it, that enumeration is ordered after
	// all: its snapshot is newer than the proof and the target is gone.
	unlisted uint64
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
		targets:       make(map[target.ID]*knownTarget),
		windows:       make(map[*Window]struct{}),
		destroyedRead: make(map[target.ID]uint64),
		announced:     make(map[target.ID]uint64),
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

// PendingDestroyed reports how many destroyed notifications the registry
// remembers as read but not yet handled by the queue consumer.
func (r *TabRegistry) PendingDestroyed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.destroyedRead)
}

// PendingAnnounced reports how many unknown targets the registry remembers a
// page event about (see announced).
func (r *TabRegistry) PendingAnnounced() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.announced)
}

// ReportedGone reports whether id was reported gone while w was open: by a
// command reply, a destroyed notification, or an enumeration — the one w was
// opened for included. w may have ended; what it recorded until then stays.
func (r *TabRegistry) ReportedGone(w *Window, id target.ID) bool {
	if w == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, gone := w.removed[id]
	return gone
}

// provenLocked returns the earliest instant id is proven to have existed at,
// given a proof at at, and releases the announcement about it: the caller
// registers id with the result as seen (caller holds lock).
func (r *TabRegistry) provenLocked(id target.ID, at uint64) uint64 {
	if read, ok := r.announced[id]; ok {
		delete(r.announced, id)
		return min(read, at)
	}
	return at
}

// absenceLocked records that evidence which did not come through the
// lifecycle queue was applied at now: every event read so far is older than
// it and can no longer register an unknown target, so the per-target
// destroyed stamps, which say the same about one target, are released (caller
// holds lock).
func (r *TabRegistry) absenceLocked(now uint64) {
	r.lastAbsence = now
	clear(r.destroyedRead)
}

// unlists reports whether an enumeration applied while w was open did not list
// id: absence evidence that cannot be ordered against a reply of w naming id.
func (w *Window) unlists(id target.ID) bool {
	if w == nil || !w.narrowed {
		return false
	}
	_, listed := w.listed[id]
	return !listed
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
//     after this snapshot, or destroyed before it?). verify is set, the
//     opening of w is remembered on the target (unlisted) in case an earlier
//     proof of its existence is still on its way, and meanwhile a first-hand
//     target stays as it is — this is what protects a concurrent tab_new from
//     an earlier, even empty, snapshot — while a discovery is suspended: it
//     stops resolving but is neither removed nor reported, so nothing that
//     belongs to it is released.
//
// A doomed target is removed whether listed or not: its destroyed notification
// was read and this is the pending cleanup.
//
// A target that is not known but announced (a page event about it was read,
// see announced) before w opened is judged the same way: not listed, it is
// gone — nothing is registered, so nothing is reported in removed, but it is
// recorded in the windows in flight, and a Target.createTarget reply that is
// still on its way cannot register it; listed, it is registered below with the
// announcement as seen. An announcement read after w opened cannot be ordered
// against this reply and is kept for the enumeration its event requests.
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
		if w.removed == nil {
			w.removed = make(map[target.ID]struct{})
		}
		gone := w.removed
		for id, k := range r.targets {
			_, listed := present[id]
			switch {
			case k.doomed, !listed && k.seen < w.opened:
				removed = append(removed, RemovedTab{TargetID: id})
				r.removeLocked(id)
				gone[id] = struct{}{}
			case listed:
			default:
				verify = true
				k.unlisted = max(k.unlisted, w.opened)
				if !k.firstHand {
					k.suspended = true
				}
			}
		}
		slices.SortFunc(removed, func(a, b RemovedTab) int { return cmp.Compare(a.TargetID, b.TargetID) })

		// Existence proofs about targets that are not known, read before this
		// enumeration was requested: the snapshot is newer than each of them.
		for id, read := range r.announced {
			if read > w.opened {
				continue
			}
			if _, listed := present[id]; !listed {
				delete(r.announced, id)
				r.noteRemovedLocked(id)
			}
		}

		for id := range present {
			if _, isGone := gone[id]; isGone {
				continue
			}
			contradicted := w.unlists(id)
			k := r.targets[id]
			switch {
			case k == nil:
				r.targets[id] = &knownTarget{seen: r.provenLocked(id, now), suspended: contradicted}
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
		r.absenceLocked(now)
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
	return r.remove(id, true, 0)
}

// RemoveEventTarget is RemoveTarget for a report that arrived through the
// lifecycle event queue: the target was reported as not a page. The queue is
// ordered, so events about the target that the browser sent earlier have been
// handled already and later ones are newer than this report: other queued
// events are not invalidated by it.
func (r *TabRegistry) RemoveEventTarget(id target.ID) bool {
	return r.remove(id, false, 0)
}

// RemoveDestroyedTarget is RemoveEventTarget for the destroyed notification
// that was read at stamp: the consumer got to it, so every older event about
// the target has been handled and the registry stops remembering the read
// (unless a newer destroyed notification about the same id is still queued).
func (r *TabRegistry) RemoveDestroyedTarget(stamp uint64, id target.ID) bool {
	return r.remove(id, false, stamp)
}

func (r *TabRegistry) remove(id target.ID, outOfQueue bool, destroyedStamp uint64) bool {
	if id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.tickLocked()
	_, known := r.targets[id]
	r.removeLocked(id)
	if outOfQueue {
		r.absenceLocked(now)
	} else if read, ok := r.destroyedRead[id]; ok && read <= destroyedStamp {
		delete(r.destroyedRead, id)
	}
	return known
}

// CommitCreated registers the target a Target.createTarget reply names and
// ends w. w must have been opened BEFORE the creation was requested: the
// target exists in the browser, and can be listed, used, closed and destroyed,
// before its creator sees the reply.
//
// The same checks apply whether or not the target is already known (an
// enumeration or its creation event may have got there first):
//   - A target reported gone while w was open is not registered, whether or not
//     it is known again right now: the reply predates that removal (INV-6).
//     Neither is a doomed one.
//   - A known target keeps its seen, so an enumeration requested after that
//     still removes it when it does not list it; an unknown one is registered
//     with seen = now, or with the instant its creation event was read when
//     that is still remembered (announced). Either way it is first-hand from here on — the daemon
//     knows it was created while w was open — and resolves.
//   - verify asks for a fresh enumeration when absence evidence exists that
//     cannot be ordered against the creation: the target was suspended, or an
//     enumeration applied while w was open did not list it. Neither is held
//     against the new target (the snapshot may predate it), but the caller
//     must ask the browser again before it reports the creation as a success.
//
// An unknown target is not registered through a window that had ended before
// (nothing recorded what happened since); for a known one such a commit only
// records that the daemon created it.
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
	k := r.targets[id]
	switch {
	case k == nil && w != nil && !live:
		return false, false
	case k == nil:
		k = &knownTarget{seen: r.provenLocked(id, r.tickLocked())}
		r.targets[id] = k
	case k.doomed:
		return false, false
	}
	verify = k.suspended || w.unlists(id)
	k.firstHand = true
	k.suspended = false
	return true, verify
}

// NoteEventRead is called for every lifecycle event at the moment it is read
// from the browser connection, in connection order and before the event is
// queued. It returns the event's stamp: its instant on the registry clock.
//
// page is the target a created / info changed event reports as a page (empty
// for other events). When that target is not known, the read is remembered
// (announced, earliest read wins) until the target is registered, reported
// gone, or answered by an enumeration requested after this instant: the proof
// that it existed at this instant must not depend on how soon the queue
// consumer gets to the event, because an enumeration requested later that
// does not list the target may be applied first.
//
// destroyed is the target a Target.targetDestroyed notification names (empty
// for other events). The report takes effect here, not when the queue gets to
// it: a known target is doomed — it stops resolving at once — and the id is
// recorded as removed in every window in flight, so neither an enumeration that
// still lists it nor the Target.createTarget reply that names it can register
// it. Known or not, the read is remembered until the consumer handles the
// notification, so an "exists" event about the target that was read before it
// and is still queued registers nothing either. Unregistering is left to the
// consumer because it also clears daemon state under the lifecycle mutex,
// which the connection reader must not take.
func (r *TabRegistry) NoteEventRead(page, destroyed target.ID) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	stamp := r.tickLocked()
	if destroyed != "" {
		r.doomLocked(destroyed)
		r.destroyedRead[destroyed] = stamp
	}
	if page != "" && r.targets[page] == nil {
		if _, ok := r.announced[page]; !ok {
			r.announced[page] = stamp
		}
	}
	return stamp
}

// doomLocked applies the knowledge that id is gone on a path that cannot take
// the lifecycle mutex: a known target stops resolving at once and is cleaned
// up by the consumer or by the next enumeration; the id is recorded as removed
// in every window in flight (caller holds lock).
func (r *TabRegistry) doomLocked(id target.ID) {
	if k := r.targets[id]; k != nil {
		k.doomed = true
	}
	delete(r.announced, id)
	r.noteRemovedLocked(id)
}

// EventDropped is called on the connection reader when the lifecycle queue was
// full and the event just stamped by NoteEventRead was not queued, BEFORE the
// overflow is made visible to the consumer. What was lost is unknown to the
// queue, so events still queued can no longer register a target that is not
// known (RegisterEvent); the enumeration the overflow requests does that.
func (r *TabRegistry) EventDropped() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.absenceLocked(r.tickLocked())
}

// RegisterEvent applies a lifecycle event (created, info changed) that reported
// the page target id as existing when it was read from the connection at
// stamp, and reports whether the event settled what it is evidence of. When it
// did not, the caller must request a fresh enumeration.
//
// The event proves the target existed at stamp, however late it is handled and
// whatever registered the target meanwhile:
//   - the target is doomed, or it is unknown and a destroyed notification about
//     it that was read after stamp is still queued: the event is older than
//     the destruction, registers nothing, and is settled by it (the consumer
//     gets to the notification next; no enumeration is needed);
//   - the target is known: it keeps the earlier of seen and stamp. A creation
//     committed, or an enumeration applied, after the event was read must not
//     stand in for the earlier proof — an enumeration requested after stamp
//     that does not list the target proves it gone. If such an enumeration was
//     applied already (unlisted > stamp), the target is gone now: it is doomed
//     and false is returned so the cleanup enumeration is requested;
//   - the target is unknown: it is registered first-hand with seen = stamp
//     (or an earlier read still remembered about it), unless an enumeration,
//     a removal learned from a command reply or a queue overflow was applied
//     after stamp: the event is older than that evidence and cannot bring an
//     id back (INV-6). The proof itself is not discarded with the refusal: an
//     enumeration requested after stamp that did not list the target has
//     already recorded it as gone (applyLocked), and one that could not be
//     ordered leaves the announcement in place for the enumeration the caller
//     requests now.
func (r *TabRegistry) RegisterEvent(stamp uint64, id target.ID) (settled bool) {
	if id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.targets[id]
	switch {
	case k == nil:
		if r.destroyedRead[id] > stamp {
			return true
		}
		if r.lastAbsence > stamp {
			return false
		}
		r.targets[id] = &knownTarget{seen: r.provenLocked(id, stamp), firstHand: true}
	case k.doomed:
	case stamp < k.seen:
		k.seen = stamp
		if k.seen < k.unlisted {
			r.doomLocked(id)
			return false
		}
	}
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
	r.targets[id] = &knownTarget{seen: r.provenLocked(id, r.tickLocked()), firstHand: true}
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
	delete(r.announced, id)
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
