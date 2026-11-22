package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/yiplee/go-bb-browser/pkg/protocol"
)

// Pool spreads JSON-RPC calls across multiple [Client] backends (one bb-daemon
// each). The existing single-daemon [Client] API is unchanged; Pool exposes the
// same Health / Call / typed RPC surface and picks a backend per request.
//
// Tab IDs (Pool-only): callers see `<backendKey>:<daemonShortId>`. The default
// backend key is the 0-based constructor index as a decimal string ("0", "1",
// …); [NewPoolBackends] can set a stable name instead. Pool strips the prefix
// before talking to that daemon and prefixes TabNew, TabList, TabFocus, and
// bound-method results. Cross-daemon native short-id collisions are therefore
// distinct at this API. Malformed ids and unknown keys are rejected; routing
// is derived from the prefix. An explicit tab map records tabs created via
// TabNew or observed via TabList/TabFocus (for [Pool.ClientForTab],
// [Pool.ForgetTab], and close/unbind).
//
// Tab affinity (hard invariant): a tab lives on exactly one daemon. Later
// tab-scoped RPCs (Eval, Goto, …) go only to the backend named by the prefix.
// The pool never retries or forwards a pinned tab to a different daemon, even
// if the owner is down.
//
// Policy
//
//   - Startup: [NewPool] does not probe daemons. Construction succeeds even if
//     some or all backends are currently down (they are skipped later).
//   - Unbound requests (health checks, tab_new, and Call for methods without a
//     tab id, other than tab_list / tab_focus): pick the backend with the
//     fewest in-flight calls (round-robin among ties). On any error, try the
//     next backend rather than failing the whole pool. If the caller context
//     is done, the walk stops. If every backend fails, the error is
//     [*AllFailedError]. Failover here never moves an existing tab; tab_new
//     creates a new tab on the backend that succeeds.
//   - tab_new spread: tab_new has its own round-robin cursor, so health checks
//     and other unbound calls made between creates do not shift where tabs
//     open. The backend is chosen per attempt (fewest in-flight among those
//     this request has not tried, ties from the cursor) and every attempt
//     moves the cursor just past the backend it took. A down backend's share
//     is therefore split across the healthy ones instead of all going to its
//     ring neighbour, and a create that fails over while others are running
//     does not re-take or skip their turns.
//   - tab_list: query every backend in constructor order, prefix each id, and
//     merge. A down backend is skipped (its tabs are omitted). If every
//     backend fails, the error is [*AllFailedError]. Listed ids are bound so
//     later tab ops can use them. There is no pool-wide focus: Tab and Focus
//     are the first non-empty values in constructor order.
//     A successful reply is that backend's authoritative tab set: bindings
//     for that backend which the reply's Tabs no longer name are retired
//     (later ops return [*UnknownTabError] without an RPC). A backend whose
//     list failed keeps its bindings, and so does one whose reply has no
//     tabs array (missing, null or not an array): that reply counts as a
//     failure, only an explicit [] empties a backend. See "Binding
//     lifecycle" below.
//   - tab_focus: first successful backend in constructor order (skip down);
//     return prefixed ids. Not a pool-wide focus. It is a partial
//     observation: it binds the ids it names and never retires others.
//   - Bound requests: parse the prefix, then lookup in the tab map. Unknown
//     tab ids return [*UnknownTabError]; malformed / unknown-prefix ids return
//     [*InvalidTabIDError] (no silent fallback).
//
// # Binding lifecycle
//
// A binding is added by a successful tab_new, tab_list or tab_focus and
// removed by a successful tab_close, [Pool.ForgetTab], or a later successful
// tab_list on the same backend that no longer names the tab. Only the Tabs
// array of a list is authoritative; its Tab / Focus metadata neither binds
// nor keeps a tab alive.
//
// Replies can arrive out of order, so the pool keeps a logical clock (ticked
// under a short lock that is never held across network I/O). Every tab_list /
// tab_focus takes its generation from that clock when it is sent, and what it
// reports counts as evidence of that generation, whenever the reply lands:
//
//   - Per backend the pool remembers the generation of the newest list it has
//     adopted. A list or focus sent before that one is ignored when it lands
//     late: it can neither keep or add a tab the newer list dropped (even an
//     id the pool never bound) nor retire anything.
//   - Adopting a list retires the backend's bindings whose latest evidence is
//     older than the list's generation. A tab created by tab_new while the
//     list was in flight, or confirmed by a tab_list / tab_focus sent after
//     it, survives, and such a newer observation re-binds a live tab even if
//     an older list landed first and retired it.
//   - tab_close and [Pool.ForgetTab] take effect when they happen: an
//     observation already in flight does not re-bind the id, one sent
//     afterwards does if the daemon still reports it. The marks needed for
//     this are kept only while an observation older than the removal is in
//     flight on that backend; each returning observation reclaims the marks
//     older than the earliest one still in flight, so polling that always
//     overlaps does not accumulate them.
//
// Limits: generations order requests by when this pool sent them, not by when
// the daemon took the snapshot; two overlapping requests can be served in the
// other order, and then the later-sent one still wins. A tab closed outside
// the pool stays bound until the next successful list of its backend, as do
// tabs of a backend whose lists keep failing; until then calls on such an id
// reach the daemon and fail there. A late, ignored list is still returned to
// its caller, so that result may name ids the pool does not bind.
type Pool struct {
	clients  []*Client
	keys     []string
	byKey    map[string]int
	inFlight []atomic.Int64
	rr       atomic.Uint64 // tie-break cursor for health checks and other unbound calls

	// createMu guards createNext, the tab_new tie-break cursor (ring position
	// after the backend most recently handed a create attempt). It is held
	// only while picking a backend, never across HTTP. See pickCreate.
	createMu   sync.Mutex
	createNext int

	mu sync.Mutex
	// tabs maps external tab id → owning backend and the generation of the
	// newest evidence that the tab exists.
	tabs map[string]tabBinding
	// clock is the pool-local logical time. Every tab_new bind, observation
	// start and explicit removal takes a fresh tick, so ticks are unique and
	// totally ordered. Guarded by mu, like everything below.
	clock uint64
	// observing holds, per backend, the generations of the in-flight
	// tab_list / tab_focus calls.
	observing []map[uint64]struct{}
	// listGen is, per backend, the generation of the newest adopted tab_list
	// (0: none yet). Observations older than it are ignored.
	listGen []uint64
	// removed holds, per backend, ids explicitly unbound (tab_close /
	// ForgetTab) while an observation was in flight there: id → logical time
	// of the removal. A mark only matters to observations older than it, so
	// marks older than the earliest in-flight observation are reclaimed and
	// the map is nil whenever nothing is in flight.
	removed []map[string]uint64
}

// tabBinding is one entry of the pool tab map.
type tabBinding struct {
	idx int // backend index
	// epoch is the generation of the newest evidence for the tab: the
	// logical time tab_new bound it, or the generation (send time) of the
	// newest tab_list / tab_focus that reported it.
	epoch uint64
}

// errTabListNoTabs reports a tab_list result without a tabs array.
var errTabListNoTabs = errors.New("daemonclient: tab_list result has no tabs array")

// PoolBackend is one named member of a [Pool]. Key is the prefix in external
// tab ids (`<key>:<daemonShortId>`). If Key is empty, [NewPoolBackends] uses
// the 0-based index as a decimal string. Keys must be unique and must not
// contain ':'.
type PoolBackend struct {
	Key    string
	Client *Client
}

// NewPool wraps the given daemon clients. Each [Client] has its own BaseURL and
// headers (use [WithHeader] / [WithHeaders] for per-daemon credentials such as
// Cloudflare Access). Order is preserved for [Pool.Clients]. Backend keys
// default to "0", "1", … in that order.
func NewPool(clients ...*Client) (*Pool, error) {
	backends := make([]PoolBackend, len(clients))
	for i, c := range clients {
		backends[i] = PoolBackend{Client: c}
	}
	return NewPoolBackends(backends...)
}

// NewPoolBackends is [NewPool] with explicit backend keys for Pool tab ids.
func NewPoolBackends(backends ...PoolBackend) (*Pool, error) {
	if len(backends) == 0 {
		return nil, ErrEmptyPool
	}
	clients := make([]*Client, len(backends))
	keys := make([]string, len(backends))
	byKey := make(map[string]int, len(backends))
	for i, b := range backends {
		if b.Client == nil {
			return nil, fmt.Errorf("daemonclient: pool client %d is nil", i)
		}
		if strings.TrimSpace(b.Client.BaseURL) == "" {
			return nil, fmt.Errorf("daemonclient: pool client %d has empty BaseURL", i)
		}
		key := strings.TrimSpace(b.Key)
		if key == "" {
			key = strconv.Itoa(i)
		}
		if strings.Contains(key, ":") {
			return nil, fmt.Errorf("daemonclient: pool backend key %q must not contain ':'", key)
		}
		if _, dup := byKey[key]; dup {
			return nil, fmt.Errorf("daemonclient: duplicate pool backend key %q", key)
		}
		clients[i] = b.Client
		keys[i] = key
		byKey[key] = i
	}
	return &Pool{
		clients:   clients,
		keys:      keys,
		byKey:     byKey,
		inFlight:  make([]atomic.Int64, len(clients)),
		tabs:      make(map[string]tabBinding),
		observing: make([]map[uint64]struct{}, len(clients)),
		listGen:   make([]uint64, len(clients)),
		removed:   make([]map[string]uint64, len(clients)),
	}, nil
}

// FormatTabID builds a Pool-facing tab id: `<backendKey>:<daemonShortId>`.
func FormatTabID(backendKey, daemonShortID string) string {
	return backendKey + ":" + daemonShortID
}

// SplitTabID parses a Pool-facing tab id. ok is false when tab is not
// `<backendKey>:<daemonShortId>` with both sides non-empty. The key is the
// substring before the first ':'; the rest is the daemon-native short id.
func SplitTabID(tab string) (backendKey, daemonShortID string, ok bool) {
	tab = strings.TrimSpace(tab)
	key, short, found := strings.Cut(tab, ":")
	if !found || key == "" || short == "" {
		return "", "", false
	}
	return key, short, true
}

// Len returns the number of backends.
func (p *Pool) Len() int {
	return len(p.clients)
}

// BackendKeys returns constructor-order keys used in Pool tab ids.
func (p *Pool) BackendKeys() []string {
	out := make([]string, len(p.keys))
	copy(out, p.keys)
	return out
}

// Clients returns the backends in constructor order. The slice and clients are
// shared with the pool (callers may set Headers / HTTP on a backend).
func (p *Pool) Clients() []*Client {
	return p.clients
}

// ClientForTab returns the backend pinned to tab, if any. tab must be a
// Pool-facing id (`<backendKey>:<daemonShortId>`) created via TabNew or
// observed via TabList / TabFocus, and not since closed, forgotten, or retired
// by a later TabList of its backend.
func (p *Pool) ClientForTab(tab string) (*Client, bool) {
	idx, ok := p.lookupTab(tab)
	if !ok {
		return nil, false
	}
	return p.clients[idx], true
}

// ForgetTab drops tab affinity without calling tab_close (for example after a
// daemon-side idle close). Subsequent ops for tab return [*UnknownTabError].
// A TabList / TabFocus already in flight does not re-bind it; one started
// afterwards does if the daemon still reports the tab.
func (p *Pool) ForgetTab(tab string) {
	p.unbind(tab)
}

// Health succeeds if at least one backend's [Client.Health] succeeds.
func (p *Pool) Health(ctx context.Context) error {
	_, err := p.HealthResult(ctx)
	return err
}

// HealthResult returns the first successful backend health body.
func (p *Pool) HealthResult(ctx context.Context) (protocol.HealthResult, error) {
	var last protocol.HealthResult
	err := p.walkUnbound(ctx, "health", func(ctx context.Context, c *Client) error {
		var err error
		last, err = c.HealthResult(ctx)
		return err
	})
	if err != nil {
		return protocol.HealthResult{}, err
	}
	return last, nil
}

// Ready succeeds if at least one backend's [Client.Ready] succeeds.
func (p *Pool) Ready(ctx context.Context) error {
	_, err := p.ReadyResult(ctx)
	return err
}

func (p *Pool) ReadyResult(ctx context.Context) (protocol.HealthResult, error) {
	var last protocol.HealthResult
	err := p.walkUnbound(ctx, "ready", func(ctx context.Context, c *Client) error {
		var err error
		last, err = c.ReadyResult(ctx)
		return err
	})
	if err != nil {
		return protocol.HealthResult{}, err
	}
	return last, nil
}

// Live succeeds if at least one backend's [Client.Live] succeeds.
func (p *Pool) Live(ctx context.Context) error {
	return p.walkUnbound(ctx, "live", func(ctx context.Context, c *Client) error {
		return c.Live(ctx)
	})
}

// Call routes a JSON-RPC method the same way typed Pool methods do.
func (p *Pool) Call(ctx context.Context, method string, params any, result any) error {
	tab := tabIDFromParams(params)
	switch method {
	case protocol.MethodTabNew:
		return p.callCreate(ctx, method, params, result)
	case protocol.MethodTabClose:
		return p.callClose(ctx, tab, method, params, result)
	case protocol.MethodTabList:
		return p.callTabList(ctx, params, result)
	case protocol.MethodTabFocus:
		return p.callTabFocus(ctx, params, result)
	}
	if tab != "" {
		return p.callBound(ctx, tab, method, params, result)
	}
	if methodRequiresTab(method) {
		return ErrTabRequired
	}
	return p.callUnbound(ctx, method, params, result)
}

func (p *Pool) callCreate(ctx context.Context, method string, params any, result any) error {
	if err := checkCreateResult(result); err != nil {
		return err
	}
	var lastErrs []error
	tried := make([]bool, len(p.clients))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		i, hold, ok := p.pickCreate(tried)
		if !ok {
			break
		}
		// Decode internally so bind does not depend on the caller's result pointer
		// (nil, map, or a typed struct are all valid Client.Call shapes).
		var raw json.RawMessage
		err := p.clients[i].Call(ctx, method, params, &raw)
		hold()
		if err != nil {
			lastErrs = append(lastErrs, err)
			continue
		}
		native := tabFromRawResult(raw)
		if native == "" {
			lastErrs = append(lastErrs, fmt.Errorf("daemonclient: tab_new returned empty tab id"))
			continue
		}
		ext := FormatTabID(p.keys[i], native)
		if err := p.bind(ext, i); err != nil {
			p.abandonCreated(ctx, i, native)
			lastErrs = append(lastErrs, err)
			continue
		}
		if result != nil {
			raw = setJSONTab(raw, ext)
			if err := json.Unmarshal(raw, result); err != nil {
				p.unbind(ext)
				p.abandonCreated(ctx, i, native)
				return fmt.Errorf("daemonclient: decode tab_new result: %w", err)
			}
		}
		return nil
	}
	return &AllFailedError{Op: method, Errs: lastErrs}
}

func (p *Pool) callClose(ctx context.Context, tab, method string, params any, result any) error {
	if tab == "" {
		return ErrTabRequired
	}
	err := p.callBound(ctx, tab, method, params, result)
	if err == nil {
		p.unbind(tab)
	}
	return err
}

func (p *Pool) callBound(ctx context.Context, tab, method string, params any, result any) error {
	idx, short, err := p.resolveTab(tab)
	if err != nil {
		return err
	}
	stripped, err := replaceJSONTab(params, short)
	if err != nil {
		return err
	}
	hold := p.track(idx)
	defer hold()
	if result == nil {
		return p.clients[idx].Call(ctx, method, stripped, nil)
	}
	var raw json.RawMessage
	if err := p.clients[idx].Call(ctx, method, stripped, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(setJSONTab(raw, tab), result); err != nil {
		return fmt.Errorf("daemonclient: decode %s result: %w", method, err)
	}
	return nil
}

func (p *Pool) callTabList(ctx context.Context, params any, result any) error {
	merged := protocol.TabListResult{Tabs: []protocol.TabListItem{}}
	var lastErrs []error
	anyOK := false
	for i := range p.clients {
		if err := ctx.Err(); err != nil {
			return err
		}
		var raw json.RawMessage
		started := p.beginObserve(i)
		hold := p.track(i)
		err := p.clients[i].Call(ctx, protocol.MethodTabList, params, &raw)
		hold()
		var one protocol.TabListResult
		if err == nil {
			one, err = decodeTabList(raw)
		}
		if err != nil {
			// A failed list, or a reply without a tabs array, says nothing
			// about the backend's tabs.
			p.endObserve(i, started)
			lastErrs = append(lastErrs, err)
			continue
		}
		anyOK = true
		one = prefixTabListResult(p.keys[i], one)
		// Only Tabs is the authoritative set; Tab / Focus are metadata.
		p.finishObserve(i, started, true, tabIDsFromList(one)...)
		merged.Tabs = append(merged.Tabs, one.Tabs...)
		if one.Seq > merged.Seq {
			merged.Seq = one.Seq
		}
		if merged.Tab == "" && one.Tab != "" {
			merged.Tab = one.Tab
		}
		if merged.Focus == "" && one.Focus != "" {
			merged.Focus = one.Focus
		}
	}
	if !anyOK {
		return &AllFailedError{Op: protocol.MethodTabList, Errs: lastErrs}
	}
	return copyResult(result, merged)
}

func (p *Pool) callTabFocus(ctx context.Context, params any, result any) error {
	var lastErrs []error
	for i := range p.clients {
		if err := ctx.Err(); err != nil {
			return err
		}
		var one protocol.TabFocusResult
		started := p.beginObserve(i)
		hold := p.track(i)
		err := p.clients[i].Call(ctx, protocol.MethodTabFocus, params, &one)
		hold()
		if err != nil {
			p.endObserve(i, started)
			lastErrs = append(lastErrs, err)
			continue
		}
		one.Tab = prefixPoolTab(p.keys[i], one.Tab)
		one.Focus = prefixPoolTab(p.keys[i], one.Focus)
		p.finishObserve(i, started, false, one.Tab, one.Focus)
		return copyResult(result, one)
	}
	return &AllFailedError{Op: protocol.MethodTabFocus, Errs: lastErrs}
}

func (p *Pool) callUnbound(ctx context.Context, method string, params any, result any) error {
	var lastErrs []error
	for _, i := range p.order(&p.rr) {
		if err := ctx.Err(); err != nil {
			return err
		}
		hold := p.track(i)
		err := p.clients[i].Call(ctx, method, params, result)
		hold()
		if err != nil {
			lastErrs = append(lastErrs, err)
			continue
		}
		return nil
	}
	return &AllFailedError{Op: method, Errs: lastErrs}
}

func (p *Pool) walkUnbound(ctx context.Context, op string, fn func(context.Context, *Client) error) error {
	var lastErrs []error
	for _, i := range p.order(&p.rr) {
		if err := ctx.Err(); err != nil {
			return err
		}
		hold := p.track(i)
		err := fn(ctx, p.clients[i])
		hold()
		if err != nil {
			lastErrs = append(lastErrs, err)
			continue
		}
		return nil
	}
	return &AllFailedError{Op: op, Errs: lastErrs}
}

func (p *Pool) track(i int) func() {
	p.inFlight[i].Add(1)
	return func() { p.inFlight[i].Add(-1) }
}

// pickCreate chooses the backend for one tab_new attempt and marks it tried
// and in flight: among backends this request has not tried, the one with the
// fewest in-flight calls, ties going to the first at or after the create
// cursor in ring order. The cursor then moves just past the chosen backend.
// ok is false once every backend has been tried.
//
// The choice is made per attempt, not as a walk fixed when the request starts,
// and each attempt moves the cursor exactly once whether or not it succeeds.
// So a failover consumes only the turn it actually takes: a failed backend's
// share rotates across the healthy ones (sequential creates differ by at most
// one), and a request that fails over late resumes from wherever concurrent
// creates left the cursor instead of re-taking or skipping their turns.
// Selection and the in-flight increment share createMu, so concurrent picks
// see each other; the returned release must be called when the attempt ends.
func (p *Pool) pickCreate(tried []bool) (idx int, release func(), ok bool) {
	n := len(p.clients)
	p.createMu.Lock()
	defer p.createMu.Unlock()
	best := -1
	var bestN int64
	for i := 0; i < n; i++ {
		c := (p.createNext + i) % n
		if tried[c] {
			continue
		}
		if v := p.inFlight[c].Load(); best < 0 || v < bestN {
			best, bestN = c, v
		}
	}
	if best < 0 {
		return 0, nil, false
	}
	tried[best] = true
	p.createNext = (best + 1) % n
	return best, p.track(best), true
}

// order returns backend indexes: least in-flight first, round-robin among ties
// (rr is the caller's tie-break cursor and advances by one), then the remaining
// backends in ring order (failover walk).
func (p *Pool) order(rr *atomic.Uint64) []int {
	n := len(p.clients)
	start := int((rr.Add(1) - 1) % uint64(n))
	best := start
	bestN := p.inFlight[start].Load()
	for i := 1; i < n; i++ {
		idx := (start + i) % n
		if v := p.inFlight[idx].Load(); v < bestN {
			best, bestN = idx, v
		}
	}
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = (best + i) % n
	}
	return out
}

// bind records external tab → idx. It is a no-op overwrite when the same
// backend already owns tab, and returns [*TabCollisionError] if a different
// backend does (defensive: prefixed ids make cross-daemon collisions
// impossible at this layer).
func (p *Pool) bind(tab string, idx int) error {
	tab = strings.TrimSpace(tab)
	if tab == "" {
		return fmt.Errorf("daemonclient: empty tab id")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.tabs[tab]; ok && prev.idx != idx {
		return &TabCollisionError{Tab: tab}
	}
	p.clock++
	p.tabs[tab] = tabBinding{idx: idx, epoch: p.clock}
	// A fresh tab_new outranks an earlier removal of the same id.
	delete(p.removed[idx], tab)
	return nil
}

// beginObserve marks the start of a tab_list / tab_focus on backend idx and
// returns its generation. Every call must be paired with exactly one
// endObserve or finishObserve. The lock is released before any network I/O.
func (p *Pool) beginObserve(idx int) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clock++
	if p.observing[idx] == nil {
		p.observing[idx] = make(map[uint64]struct{})
	}
	p.observing[idx][p.clock] = struct{}{}
	return p.clock
}

// endObserve ends the observation of generation started without applying
// anything (failed call or unusable reply).
func (p *Pool) endObserve(idx int, started uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endObserveLocked(idx, started)
}

// endObserveLocked drops the observation of generation started from the
// in-flight set and reclaims the removal marks no remaining observation can
// need: all of them when the backend goes idle, otherwise those older than
// the earliest observation still in flight.
func (p *Pool) endObserveLocked(idx int, started uint64) {
	delete(p.observing[idx], started)
	if len(p.observing[idx]) == 0 {
		p.observing[idx] = nil
		p.removed[idx] = nil
		return
	}
	if len(p.removed[idx]) == 0 {
		return
	}
	var earliest uint64
	for g := range p.observing[idx] {
		if earliest == 0 || g < earliest {
			earliest = g
		}
	}
	if earliest < started {
		return // an older observation is still in flight: nothing new to reclaim
	}
	for tab, at := range p.removed[idx] {
		if at < earliest {
			delete(p.removed[idx], tab)
		}
	}
}

// finishObserve applies a successful observation of backend idx sent at
// generation started, then ends it. tabs are the external ids the backend
// reported; when authoritative (tab_list) they are its whole set.
//
// Everything is judged by started, never by when the reply landed:
//
//   - An observation older than the newest adopted list is ignored.
//   - A reported id is bound with evidence generation started, unless it was
//     explicitly removed after started (tab_close / ForgetTab).
//   - An authoritative list retires the backend's bindings whose evidence is
//     older than started. Bindings from a tab_new that completed, or an
//     observation that was sent, after started are newer evidence and stay.
func (p *Pool) finishObserve(idx int, started uint64, authoritative bool, tabs ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.endObserveLocked(idx, started)
	if started < p.listGen[idx] {
		return
	}
	for _, tab := range tabs {
		tab = strings.TrimSpace(tab)
		if tab == "" {
			continue
		}
		if at, gone := p.removed[idx][tab]; gone && at > started {
			continue
		}
		prev, ok := p.tabs[tab]
		if ok && (prev.idx != idx || prev.epoch >= started) {
			continue // other backend (defensive, see bind) or newer evidence
		}
		p.tabs[tab] = tabBinding{idx: idx, epoch: started}
	}
	if !authoritative {
		return
	}
	p.listGen[idx] = started
	for tab, b := range p.tabs {
		if b.idx == idx && b.epoch < started {
			delete(p.tabs, tab)
		}
	}
}

// markRemovedLocked remembers that tab was explicitly removed from backend
// idx at logical time at, for the benefit of observations still in flight
// there (all of them are older than at). With none in flight nothing is
// recorded: an observation sent later is newer evidence.
func (p *Pool) markRemovedLocked(idx int, tab string, at uint64) {
	if len(p.observing[idx]) == 0 {
		return
	}
	if p.removed[idx] == nil {
		p.removed[idx] = make(map[string]uint64)
	}
	p.removed[idx][tab] = at
}

// abandonCreated closes a tab on idx without touching the pool map (the id may
// already belong to another backend). Best-effort: a cancelled caller context
// must not skip cleanup. tab is the daemon-native short id.
func (p *Pool) abandonCreated(ctx context.Context, idx int, tab string) {
	tab = strings.TrimSpace(tab)
	if tab == "" || idx < 0 || idx >= len(p.clients) {
		return
	}
	hold := p.track(idx)
	defer hold()
	_, _ = p.clients[idx].TabClose(context.WithoutCancel(ctx), protocol.TabCloseParams{Tab: tab})
}

func (p *Pool) unbind(tab string) {
	tab = strings.TrimSpace(tab)
	if tab == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// The prefix names the backend even when tab is not bound yet (an
	// in-flight observation might be about to bind it).
	idx := -1
	if b, ok := p.tabs[tab]; ok {
		idx = b.idx
	} else if key, _, ok := SplitTabID(tab); ok {
		if i, known := p.byKey[key]; known {
			idx = i
		}
	}
	delete(p.tabs, tab)
	if idx >= 0 {
		p.clock++
		p.markRemovedLocked(idx, tab, p.clock)
	}
}

func (p *Pool) lookupTab(tab string) (int, bool) {
	tab = strings.TrimSpace(tab)
	if tab == "" {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.tabs[tab]
	return b.idx, ok
}

// resolveTab parses a Pool-facing tab id, checks the prefix against this pool,
// and requires the id to be in the created-tab map. The prefix selects the
// backend (source of truth); the map is "created via this pool".
func (p *Pool) resolveTab(tab string) (idx int, short string, err error) {
	tab = strings.TrimSpace(tab)
	key, short, ok := SplitTabID(tab)
	if !ok {
		return 0, "", &InvalidTabIDError{Tab: tab, Reason: "malformed; want <backendKey>:<daemonShortId>"}
	}
	idx, ok = p.byKey[key]
	if !ok {
		return 0, "", &InvalidTabIDError{Tab: tab, Reason: fmt.Sprintf("unknown backend key %q", key)}
	}
	mapped, inMap := p.lookupTab(tab)
	if !inMap {
		return 0, "", &UnknownTabError{Tab: tab}
	}
	if mapped != idx {
		// Prefix wins if the map ever disagrees (should not happen).
		return idx, short, nil
	}
	return idx, short, nil
}

func tabIDFromParams(params any) string {
	return jsonTabField(params)
}

func tabFromRawResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var peek struct {
		Tab string `json:"tab"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return ""
	}
	return strings.TrimSpace(peek.Tab)
}

func jsonTabField(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return tabFromRawResult(b)
}

// replaceJSONTab swaps only the tab field. Other values stay raw JSON so
// integers above 2^53 (seq, since, cursor, nested event seqs) are not rounded
// through float64.
func replaceJSONTab(params any, short string) (any, error) {
	if params == nil {
		return map[string]any{"tab": short}, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m["tab"], err = json.Marshal(short)
	if err != nil {
		return nil, fmt.Errorf("marshal tab: %w", err)
	}
	return m, nil
}

func prefixPoolTab(backendKey, daemonShortID string) string {
	daemonShortID = strings.TrimSpace(daemonShortID)
	if daemonShortID == "" {
		return ""
	}
	return FormatTabID(backendKey, daemonShortID)
}

func prefixTabListResult(key string, r protocol.TabListResult) protocol.TabListResult {
	r.Tab = prefixPoolTab(key, r.Tab)
	r.Focus = prefixPoolTab(key, r.Focus)
	if r.Tabs == nil {
		r.Tabs = []protocol.TabListItem{}
		return r
	}
	out := make([]protocol.TabListItem, len(r.Tabs))
	for i, item := range r.Tabs {
		item.Tab = prefixPoolTab(key, item.Tab)
		out[i] = item
	}
	r.Tabs = out
	return r
}

// decodeTabList decodes one backend's tab_list result and requires an
// explicit tabs array. A missing, null or non-array tabs is an error rather
// than an empty set, so a malformed reply can never empty a backend.
func decodeTabList(raw json.RawMessage) (protocol.TabListResult, error) {
	var peek struct {
		Tabs json.RawMessage `json:"tabs"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return protocol.TabListResult{}, fmt.Errorf("daemonclient: decode tab_list result: %w", err)
	}
	if tabs := bytes.TrimSpace(peek.Tabs); len(tabs) == 0 || tabs[0] != '[' {
		return protocol.TabListResult{}, errTabListNoTabs
	}
	var out protocol.TabListResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return protocol.TabListResult{}, fmt.Errorf("daemonclient: decode tab_list result: %w", err)
	}
	return out, nil
}

func tabIDsFromList(r protocol.TabListResult) []string {
	ids := make([]string, 0, len(r.Tabs))
	for _, item := range r.Tabs {
		ids = append(ids, item.Tab)
	}
	return ids
}

func copyResult(dst any, src any) error {
	if dst == nil {
		return nil
	}
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("daemonclient: decode result: %w", err)
	}
	return nil
}

// setJSONTab rewrites only the tab field and leaves every other value untouched.
func setJSONTab(raw json.RawMessage, tab string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return raw
	}
	tabJSON, err := json.Marshal(tab)
	if err != nil {
		return raw
	}
	m["tab"] = tabJSON
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// checkCreateResult rejects result shapes that cannot hold a tab_new object
// before any RPC is sent, so a successful create is never left unbound.
func checkCreateResult(result any) error {
	if result == nil {
		return nil
	}
	rv := reflect.ValueOf(result)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrUnusableTabNewResult
	}
	probe := reflect.New(rv.Type().Elem()).Interface()
	if err := json.Unmarshal([]byte(`{"tab":"x","seq":1}`), probe); err != nil {
		return fmt.Errorf("%w: %v", ErrUnusableTabNewResult, err)
	}
	return nil
}
