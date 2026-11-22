package state

import (
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/chromedp/cdproto/target"
)

// TabRegistry maps CDP target ids to stable short ids for the HTTP API (INV-3).
type TabRegistry struct {
	mu sync.RWMutex

	targetToShort map[target.ID]string
	shortToTarget map[string]target.ID
	selected      string // short id; empty if none

	// gen is a monotonic registration generation. targetGen records the
	// generation at which each target was last registered (or explicitly
	// re-registered), so a full enumeration requested earlier cannot remove it.
	gen       uint64
	targetGen map[target.ID]uint64

	// regSeq numbers registrations. targetReg holds the number of each target's
	// current registration; unlike targetGen it never changes while the target
	// stays registered, so it tells one lifecycle of a target id from the next.
	regSeq    uint64
	targetReg map[target.ID]uint64
}

// TabRegistration identifies one registration of a target: the span between
// the target entering the registry and leaving it. A target id that is removed
// and registered again, with the same short id or another, gets a new one, so
// comparing registrations (IsCurrent) is the only way to tell that a tab
// resolved earlier is still the same tab. Equal target or short id strings do
// not show that. The zero value is no registration.
type TabRegistration struct {
	TargetID target.ID
	ShortID  string
	seq      uint64
}

// EnumerationToken marks the moment a full target enumeration was requested.
// The zero value is the oldest possible token: applying a snapshot with it
// registers targets but never removes one.
type EnumerationToken struct {
	gen uint64
}

type RemovedTab struct {
	TargetID target.ID
	ShortID  string
}

func NewTabRegistry() *TabRegistry {
	return &TabRegistry{
		targetToShort: make(map[target.ID]string),
		shortToTarget: make(map[string]target.ID),
		targetGen:     make(map[target.ID]uint64),
		targetReg:     make(map[target.ID]uint64),
	}
}

// Registration resolves a short id to the registration it currently names (INV-3).
func (r *TabRegistry) Registration(shortID string) (TabRegistration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	shortID = strings.TrimSpace(shortID)
	if shortID == "" {
		return TabRegistration{}, false
	}
	id, ok := r.shortToTarget[shortID]
	if !ok {
		return TabRegistration{}, false
	}
	return TabRegistration{TargetID: id, ShortID: shortID, seq: r.targetReg[id]}, true
}

// RegistrationOf returns the current registration of a target id.
func (r *TabRegistry) RegistrationOf(id target.ID) (TabRegistration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	short, ok := r.targetToShort[id]
	if !ok || id == "" {
		return TabRegistration{}, false
	}
	return TabRegistration{TargetID: id, ShortID: short, seq: r.targetReg[id]}, true
}

// IsCurrent reports whether reg is still the registration of its target. A
// registration keeps its short id for as long as it lasts, so a current one
// also still owns its alias.
func (r *TabRegistry) IsCurrent(reg TabRegistration) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isCurrentLocked(reg)
}

func (r *TabRegistry) isCurrentLocked(reg TabRegistration) bool {
	if reg.seq == 0 {
		return false
	}
	return r.targetReg[reg.TargetID] == reg.seq
}

// RemoveRegistration removes reg's target only while reg is still its
// registration. It reports false, changing nothing, when that registration is
// already gone, whether or not the target id has been registered again since.
func (r *TabRegistry) RemoveRegistration(reg TabRegistration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.isCurrentLocked(reg) {
		return false
	}
	r.removeLocked(reg.TargetID)
	return true
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
// Targets that disappeared are removed (INV-6: release short id and clear association).
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
		if info == nil || !isPageTarget(info) {
			continue
		}
		present[info.TargetID] = struct{}{}
	}

	var removed []RemovedTab
	for id, short := range r.targetToShort {
		if _, ok := present[id]; ok {
			continue
		}
		if r.targetGen[id] > tok.gen {
			// Registered after the enumeration was requested: the snapshot may
			// simply predate this target. A later enumeration or its lifecycle
			// event removes it if it is really gone.
			continue
		}
		removed = append(removed, RemovedTab{TargetID: id, ShortID: short})
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
		short := r.assignShortLocked(id)
		out = append(out, TabSnapshot{
			ShortID:  short,
			TargetID: id,
			Title:    tabTitle(infos, id),
			URL:      tabURL(infos, id),
		})
	}
	return out, removed
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
// It always advances the target's generation, also when the target is already
// registered, so enumerations already in flight cannot remove it.
func (r *TabRegistry) RegisterPageTarget(id target.ID) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	short := r.assignShortLocked(id)
	r.gen++
	r.targetGen[id] = r.gen
	return short
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
	delete(r.targetGen, id)
	delete(r.targetReg, id)
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

// registerLocked records a NEW registration and stamps it with a fresh generation.
func (r *TabRegistry) registerLocked(id target.ID, short string) {
	r.targetToShort[id] = short
	r.shortToTarget[short] = id
	r.gen++
	r.targetGen[id] = r.gen
	r.regSeq++
	r.targetReg[id] = r.regSeq
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

// Select marks shortID as the focused tab or returns false if unknown.
func (r *TabRegistry) Select(shortID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	shortID = strings.TrimSpace(shortID)
	if shortID == "" {
		return false
	}
	if _, ok := r.shortToTarget[shortID]; !ok {
		return false
	}
	r.selected = shortID
	return true
}

// Selected returns the current focus short id, or "".
func (r *TabRegistry) Selected() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.selected
}
