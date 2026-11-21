package state

import (
	"math"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/chromedp/cdproto/target"
)

// TabRegistry maps CDP target ids to short ids for the HTTP API (INV-3).
//
// A short id is an alias, and it is reused: once its tab is gone the next page
// whose target id ends the same way gets it. The identity of a tab is its full
// target id. Whatever commits a decision that was prepared outside the lock
// (apply an enumeration, select a tab, name the focused tab in a response) goes
// through one method here that checks, in a single critical section, that the
// registry still agrees with what the decision was based on.
type TabRegistry struct {
	mu sync.RWMutex

	targetToShort map[target.ID]string
	shortToTarget map[string]target.ID
	selected      string // short id; empty if none

	// epoch counts registry changes: registrations and removals. registeredAt
	// is the epoch at which each target was registered.
	epoch        uint64
	registeredAt map[target.ID]uint64

	// enums are the enumerations in flight, and removedAt the epoch at which a
	// target was removed while at least one of them was. An enumeration is older
	// than every change made after it began, so on apply it neither removes a
	// target registered since nor registers one removed since.
	//
	// removedAt is owned by the enumerations in flight: an entry is recorded
	// only while one exists and is dropped as soon as none in flight began
	// before the removal, so the map is empty whenever enums is. Every
	// enumeration ends, by Sync or by End.
	enums     map[*Enumeration]struct{}
	removedAt map[target.ID]uint64
}

type RemovedTab struct {
	TargetID target.ID
	ShortID  string
}

// Enumeration marks one request for the browser's target list, from before it
// is sent until its reply is applied (TabRegistry.Sync) or given up (End).
type Enumeration struct {
	r     *TabRegistry
	epoch uint64
	ended bool // guarded by r.mu
}

// SyncResult is the registry right after an enumeration was applied, read in
// the same critical section: Tabs is every registered tab and Focus the
// selected short id ("" if none), which is always one of Tabs.
type SyncResult struct {
	Tabs    []TabSnapshot
	Removed []RemovedTab
	// Kept are the registered targets the enumeration did not list but which
	// were registered after it began. They are part of Tabs.
	Kept  []target.ID
	Focus string
}

func NewTabRegistry() *TabRegistry {
	return &TabRegistry{
		targetToShort: make(map[target.ID]string),
		shortToTarget: make(map[string]target.ID),
		registeredAt:  make(map[target.ID]uint64),
		enums:         make(map[*Enumeration]struct{}),
		removedAt:     make(map[target.ID]uint64),
	}
}

// BeginEnumeration is called before asking the browser for its targets. The
// caller must end the enumeration on every path: hand it to Sync with the
// reply, or call End (error, cancellation). `defer e.End()` covers both.
func (r *TabRegistry) BeginEnumeration() *Enumeration {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := &Enumeration{r: r, epoch: r.epoch}
	r.enums[e] = struct{}{}
	return e
}

// End gives the enumeration up. It is a no-op once the enumeration has ended,
// and on nil.
func (e *Enumeration) End() {
	if e == nil {
		return
	}
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	e.r.endEnumerationLocked(e)
}

func (r *TabRegistry) endEnumerationLocked(e *Enumeration) {
	if e.ended {
		return
	}
	e.ended = true
	delete(r.enums, e)
	// A removal matters only to an enumeration that began before it.
	oldest := uint64(math.MaxUint64)
	for cur := range r.enums {
		oldest = min(oldest, cur.epoch)
	}
	for id, at := range r.removedAt {
		if at <= oldest {
			delete(r.removedAt, id)
		}
	}
}

// PendingEnumerations reports the enumerations in flight and the removals
// remembered for them. Both are zero when no enumeration is in flight.
func (r *TabRegistry) PendingEnumerations() (inFlight, removals int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.enums), len(r.removedAt)
}

// SyncPageTargets updates registration from the current set of page-type targets.
// Targets that disappeared are removed (INV-6: release short id and clear association).
func (r *TabRegistry) SyncPageTargets(infos []*target.Info) []TabSnapshot {
	return r.Sync(infos, nil).Tabs
}

// SyncPageTargetsDetailed also reports removals so callers can clear every
// target-scoped resource through one lifecycle path (INV-6).
func (r *TabRegistry) SyncPageTargetsDetailed(infos []*target.Info) ([]TabSnapshot, []RemovedTab) {
	res := r.Sync(infos, nil)
	return res.Tabs, res.Removed
}

// Sync applies the reply to enumeration e and ends e. With a nil e the list is
// taken as current (nothing changed since it was read).
//
// Otherwise the reply may be older than changes made since e began, and for a
// target changed since then the registry is right and the reply is not:
//
//   - A target registered after e began (tab_new, a targetCreated event) and
//     missing from infos is kept, and is part of Tabs (title and URL unknown).
//   - A target removed after e began (closed, a targetDestroyed event) and
//     still listed in infos stays removed. Registering it again would bring a
//     dead tab back, and under another short id if a new page has taken its
//     old one meanwhile.
//
// The next enumeration, begun after those changes, is authoritative for them.
func (r *TabRegistry) Sync(infos []*target.Info, e *Enumeration) SyncResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	epoch := uint64(math.MaxUint64)
	if e != nil {
		epoch = e.epoch
	}
	var res SyncResult

	present := make(map[target.ID]struct{}, len(infos))
	for _, info := range infos {
		if info == nil || !isPageTarget(info) {
			continue
		}
		if _, registered := r.targetToShort[info.TargetID]; !registered && r.removedAt[info.TargetID] > epoch {
			continue
		}
		present[info.TargetID] = struct{}{}
	}

	for id, short := range r.targetToShort {
		if _, ok := present[id]; !ok {
			if r.registeredAt[id] > epoch {
				res.Kept = append(res.Kept, id)
				continue
			}
			res.Removed = append(res.Removed, RemovedTab{TargetID: id, ShortID: short})
			r.removeLocked(id)
		}
	}

	ids := make([]target.ID, 0, len(present)+len(res.Kept))
	for id := range present {
		ids = append(ids, id)
	}
	ids = append(ids, res.Kept...)
	slices.SortFunc(ids, func(a, b target.ID) int {
		return strings.Compare(string(a), string(b))
	})
	res.Tabs = make([]TabSnapshot, 0, len(ids))
	for _, id := range ids {
		short := r.assignShortLocked(id)
		res.Tabs = append(res.Tabs, TabSnapshot{
			ShortID:  short,
			TargetID: id,
			Title:    tabTitle(infos, id),
			URL:      tabURL(infos, id),
		})
	}
	res.Focus = r.selected
	if e != nil {
		r.endEnumerationLocked(e)
	}
	return res
}

// RemoveTarget removes one target and returns its former short id.
func (r *TabRegistry) RemoveTarget(id target.ID) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	short, ok := r.targetToShort[id]
	if !ok {
		return "", false
	}
	r.removeLocked(id)
	return short, true
}

// RegisterPageTarget assigns a short id for a CDP target id without syncing from the browser.
// Use after Target.createTarget when GetTargets may not yet list the new tab.
func (r *TabRegistry) RegisterPageTarget(id target.ID) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.assignShortLocked(id)
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
	ShortID  string
	TargetID target.ID
	Title    string
	URL      string
}

func (r *TabRegistry) removeLocked(id target.ID) {
	short, ok := r.targetToShort[id]
	if !ok {
		return
	}
	delete(r.targetToShort, id)
	delete(r.shortToTarget, short)
	delete(r.registeredAt, id)
	r.epoch++
	if len(r.enums) > 0 {
		r.removedAt[id] = r.epoch
	}
	if r.selected == short {
		r.selected = ""
	}
}

// assignShortLocked returns an existing or new short id for target id (caller holds lock).
func (r *TabRegistry) assignShortLocked(id target.ID) string {
	if s, ok := r.targetToShort[id]; ok {
		return s
	}
	hexDigits := hexRunes(string(id))
	if len(hexDigits) == 0 {
		s := sanitizeShort(string(id))
		if oid, taken := r.shortToTarget[s]; taken && oid != id {
			s = string(id)
		}
		r.registerLocked(id, s)
		return s
	}
	for n := 4; n <= len(hexDigits); n++ {
		cand := string(hexDigits[len(hexDigits)-n:])
		if oid, taken := r.shortToTarget[cand]; taken && oid != id {
			continue
		}
		r.registerLocked(id, cand)
		return cand
	}
	// Extremely unlikely: fall back to full hex digit string.
	full := string(hexDigits)
	r.registerLocked(id, full)
	return full
}

func (r *TabRegistry) registerLocked(id target.ID, short string) {
	r.targetToShort[id] = short
	r.shortToTarget[short] = id
	r.epoch++
	r.registeredAt[id] = r.epoch
	// Registered again after a removal: the registration is the later change.
	delete(r.removedAt, id)
}

func hexRunes(s string) []rune {
	var out []rune
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			out = append(out, unicode.ToLower(r))
		case r >= 'a' && r <= 'f':
			out = append(out, r)
		case r >= 'A' && r <= 'F':
			out = append(out, unicode.ToLower(r))
		}
	}
	return out
}

func sanitizeShort(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		return s[len(s)-64:]
	}
	return s
}

// ShortForTarget returns the short id for a CDP target id, if registered.
func (r *TabRegistry) ShortForTarget(id target.ID) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id == "" {
		return "", false
	}
	short, ok := r.targetToShort[id]
	return short, ok
}

// Lookup validates short id and returns the CDP target id (INV-3).
func (r *TabRegistry) Lookup(shortID string) (target.ID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	shortID = strings.TrimSpace(shortID)
	if shortID == "" {
		return "", false
	}
	id, ok := r.shortToTarget[shortID]
	return id, ok
}

// SelectAlias marks the tab as focused if shortID still names target id, and
// reports whether it did. A request that resolved shortID to id earlier commits
// its selection with this: if the tab went away meanwhile, nothing is selected,
// whoever holds the short id now.
func (r *TabRegistry) SelectAlias(shortID string, id target.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	shortID = strings.TrimSpace(shortID)
	if cur, ok := r.shortToTarget[shortID]; !ok || shortID == "" || cur != id {
		return false
	}
	r.selected = shortID
	return true
}

// SelectTarget marks the tab of target id as focused and returns its short id,
// or false if the target is not registered. It cannot pick a different tab
// that has taken over a short id.
func (r *TabRegistry) SelectTarget(id target.ID) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	short, ok := r.targetToShort[id]
	if !ok {
		return "", false
	}
	r.selected = short
	return short, true
}

// Selected returns the current focus short id, or "".
func (r *TabRegistry) Selected() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.selected
}

// CommitFocus decides which tab a focus query answers with. tabs is an earlier
// Sync result and detected, if not empty, the target a probe of those tabs
// found in the foreground; the probe takes time, during which any of the tabs
// can go away and its short id pass to another page.
//
// In one critical section: detected becomes the selection if it is still
// registered. The answer is then the selected tab if there is one, else the
// first of tabs that is still registered under the short id it had. tab names
// that target and its current short id, with title and URL from tabs when the
// target is listed there; focus is the selected short id ("" if none). ok is
// false when there is no selection and none of tabs is left: nothing in the
// snapshot identifies a tab any more.
func (r *TabRegistry) CommitFocus(tabs []TabSnapshot, detected target.ID) (tab TabSnapshot, focus string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if short, registered := r.targetToShort[detected]; registered && detected != "" {
		r.selected = short
	}
	if r.selected != "" {
		tab = TabSnapshot{ShortID: r.selected, TargetID: r.shortToTarget[r.selected]}
		for _, sn := range tabs {
			if sn.TargetID == tab.TargetID {
				tab.Title, tab.URL = sn.Title, sn.URL
				break
			}
		}
		return tab, r.selected, true
	}
	for _, sn := range tabs {
		if short, registered := r.targetToShort[sn.TargetID]; registered && short == sn.ShortID {
			return sn, "", true
		}
	}
	return TabSnapshot{}, "", false
}
