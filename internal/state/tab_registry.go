package state

import (
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/chromedp/cdproto/target"
)

// TabRegistry maps CDP target ids to short ids for the HTTP API (INV-3).
//
// Alias contract:
//
//   - Every live target claims each suffix of its hex digits that is at least
//     4 digits long (lower case), plus the always-unique "id=<target id>".
//     A target id with fewer than 4 hex digits claims its hex digits (or, with
//     none at all, the id itself) instead of suffixes.
//   - An alias resolves only while exactly one live target claims it. An alias
//     claimed by several targets is a hard miss for all of them (INV-3): it is
//     never handed to whichever target happened to register first.
//   - The short id reported for a target is its shortest resolvable claim. It
//     is a pure function of the set of live targets, so every registration
//     order and a registry rebuilt after a restart agree, and it is 4 digits
//     whenever no other live target shares them.
//
// The consequence, by design: when a later target collides with a registered
// one, the reported short id of the earlier target grows and its previous
// alias stops resolving until the collision is gone. Longer aliases stay valid
// as long as they are unambiguous. Closing a target releases its aliases
// (INV-6), so a later target may legitimately claim them; anything that must
// survive a tab's lifetime has to key on the full target id instead.
type TabRegistry struct {
	mu sync.RWMutex

	// targetToShort, aliasToTarget and ambiguous are derived from the keys of targetGen
	// by rebuildAliasesLocked and replaced together.
	targetToShort map[target.ID]string
	aliasToTarget map[string]target.ID
	ambiguous     map[string]struct{} // aliases claimed by several targets
	selected      target.ID           // empty if none

	// gen is a monotonic registration generation. targetGen records the
	// generation at which each target was last registered (or explicitly
	// re-registered), so a full enumeration requested earlier cannot remove it.
	// Its key set is the set of registered targets.
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
	ShortID  string
}

func NewTabRegistry() *TabRegistry {
	return &TabRegistry{
		targetToShort: make(map[target.ID]string),
		aliasToTarget: make(map[string]target.ID),
		targetGen:     make(map[target.ID]uint64),
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
		// short is the alias that was valid while the target was registered.
		removed = append(removed, RemovedTab{TargetID: id, ShortID: short})
	}
	changed := len(removed) > 0
	for _, tab := range removed {
		r.forgetLocked(tab.TargetID)
	}

	ids := make([]target.ID, 0, len(present))
	for id := range present {
		ids = append(ids, id)
		if _, ok := r.targetGen[id]; !ok {
			r.gen++
			r.targetGen[id] = r.gen
			changed = true
		}
	}
	if changed {
		r.rebuildAliasesLocked()
	}
	slices.SortFunc(ids, func(a, b target.ID) int {
		return strings.Compare(string(a), string(b))
	})
	out := make([]TabSnapshot, 0, len(ids))
	for _, id := range ids {
		out = append(out, TabSnapshot{
			ShortID:  r.targetToShort[id],
			TargetID: id,
			Title:    tabTitle(infos, id),
			URL:      tabURL(infos, id),
		})
	}
	return out, removed
}

// RemoveTarget removes one target and returns the short id it had.
func (r *TabRegistry) RemoveTarget(id target.ID) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	short, ok := r.targetToShort[id]
	if !ok {
		return "", false
	}
	r.forgetLocked(id)
	r.rebuildAliasesLocked()
	return short, true
}

// RegisterPageTarget registers a CDP target id without syncing from the
// browser and returns its current short id. Use after Target.createTarget when
// GetTargets may not yet list the new tab. Registering a target that collides
// with a registered one changes the short id of both (see TabRegistry).
// It always advances the target's generation, also when the target is already
// registered, so enumerations already in flight cannot remove it.
func (r *TabRegistry) RegisterPageTarget(id target.ID) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, known := r.targetGen[id]
	r.gen++
	r.targetGen[id] = r.gen
	if !known {
		r.rebuildAliasesLocked()
	}
	return r.targetToShort[id]
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

// forgetLocked drops id from the registered set. The caller rebuilds aliases.
func (r *TabRegistry) forgetLocked(id target.ID) {
	delete(r.targetGen, id)
	if r.selected == id {
		r.selected = ""
	}
}

// minAliasDigits is the shortest hex suffix offered as a short id.
const minAliasDigits = 4

// fullIDAliasPrefix introduces the alias carrying the complete target id. It
// holds a hex digit and a non-hex rune, so it can equal neither a hex suffix
// nor the alias of a target id without hex digits: it is unique per target.
const fullIDAliasPrefix = "id="

// aliasClaims lists every alias id may be addressed by, preferred first.
func aliasClaims(id target.ID) []string {
	hexDigits := hexRunes(string(id))
	claims := make([]string, 0, max(len(hexDigits)-minAliasDigits+1, 1)+1)
	switch {
	case len(hexDigits) >= minAliasDigits:
		for n := minAliasDigits; n <= len(hexDigits); n++ {
			claims = append(claims, string(hexDigits[len(hexDigits)-n:]))
		}
	case len(hexDigits) > 0:
		claims = append(claims, string(hexDigits))
	default:
		if s := sanitizeShort(string(id)); s != "" {
			claims = append(claims, s)
		}
	}
	return append(claims, fullIDAliasPrefix+string(id))
}

// rebuildAliasesLocked recomputes both alias maps from the registered set, so
// the result cannot depend on the order in which targets were registered.
func (r *TabRegistry) rebuildAliasesLocked() {
	claims := make(map[target.ID][]string, len(r.targetGen))
	claimants := make(map[string]int)
	for id := range r.targetGen {
		list := aliasClaims(id)
		claims[id] = list
		for _, alias := range list {
			claimants[alias]++
		}
	}
	targetToShort := make(map[target.ID]string, len(claims))
	aliasToTarget := make(map[string]target.ID, len(claimants))
	ambiguous := make(map[string]struct{})
	for alias, n := range claimants {
		if n > 1 {
			ambiguous[alias] = struct{}{}
		}
	}
	for id, list := range claims {
		for _, alias := range list {
			if claimants[alias] != 1 {
				continue
			}
			aliasToTarget[alias] = id
			if _, ok := targetToShort[id]; !ok {
				targetToShort[id] = alias
			}
		}
	}
	r.targetToShort = targetToShort
	r.aliasToTarget = aliasToTarget
	r.ambiguous = ambiguous
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

// ShortForTarget returns the current short id for a CDP target id, if registered.
func (r *TabRegistry) ShortForTarget(id target.ID) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id == "" {
		return "", false
	}
	short, ok := r.targetToShort[id]
	return short, ok
}

// Lookup resolves an alias to the one CDP target claiming it (INV-3). Unknown
// and ambiguous aliases both miss.
func (r *TabRegistry) Lookup(shortID string) (target.ID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lookupLocked(shortID)
}

// Ambiguous reports whether shortID fails to resolve because several live
// targets claim it, as opposed to no target claiming it.
func (r *TabRegistry) Ambiguous(shortID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.ambiguous[strings.TrimSpace(shortID)]
	return ok
}

func (r *TabRegistry) lookupLocked(shortID string) (target.ID, bool) {
	shortID = strings.TrimSpace(shortID)
	if shortID == "" {
		return "", false
	}
	id, ok := r.aliasToTarget[shortID]
	return id, ok
}

// Select marks the tab addressed by shortID as focused or returns false if the
// alias is unknown or ambiguous.
func (r *TabRegistry) Select(shortID string) bool {
	_, ok := r.SelectAlias(shortID)
	return ok
}

// SelectAlias is Select that also reports which target was selected, resolved
// under the same lock as the selection.
func (r *TabRegistry) SelectAlias(shortID string) (target.ID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.lookupLocked(shortID)
	if !ok || id == "" {
		return "", false
	}
	r.selected = id
	return id, true
}

// SelectTarget marks a registered target as focused. The selection is held by
// target id, so it follows the tab when a collision changes its short id.
func (r *TabRegistry) SelectTarget(id target.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.targetGen[id]; !ok || id == "" {
		return false
	}
	r.selected = id
	return true
}

// Selected returns the current short id of the focused tab, or "".
func (r *TabRegistry) Selected() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.selected == "" {
		return ""
	}
	return r.targetToShort[r.selected]
}
