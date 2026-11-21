package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// snapshotStub is the in-page snapshot script. Ref lifecycle:
//
//   - The scope root is resolved first, so selector_scope may be a ref selector
//     of the previous snapshot; an invalid selector throws before anything is
//     touched and the previous snapshot stays usable.
//   - Every snapshot that returns a result (even an empty one) then invalidates
//     all earlier refs: it unmarks the nodes the previous snapshot marked
//     (reachable even while detached, so reinserting them restores nothing) and
//     any other marked node found in the document.
//   - Ref numbers are plain integers that are never reused within a document:
//     the counter lives on the document object and so restarts at 1 only with a
//     new document (navigation / reload), where the old marks are gone too.
//
// Bookkeeping is one non-enumerable document property holding the counter and
// the nodes of the latest snapshot only.
const snapshotStub = `
(function(opts){
opts = opts || {};
var interactiveOnly = !!opts.interactiveOnly;
var pruneEmpty = !!opts.pruneEmpty;
var maxDepth = opts.maxDepth ? Number(opts.maxDepth) : 0;
var scopeSel = opts.selectorScope ? String(opts.selectorScope) : '';

function trim(s){ return (s||'').replace(/\s+/g,' ').trim(); }

function isInteractive(el){
  var t = el.tagName;
  if (t === 'BUTTON' || t === 'A' || t === 'SELECT' || t === 'TEXTAREA') return true;
  if (t === 'INPUT') return true;
  var role = el.getAttribute && el.getAttribute('role');
  if (role === 'button' || role === 'link' || role === 'checkbox' || role === 'radio' ||
      role === 'option' || role === 'menuitem') return true;
  return false;
}

function visible(el){
  if (!el.getBoundingClientRect) return false;
  var r = el.getBoundingClientRect();
  if (!r.width && !r.height) return false;
  var st = window.getComputedStyle(el);
  if (st.visibility === 'hidden' || st.display === 'none' || Number(st.opacity) === 0) return false;
  return true;
}

function attrs(el){
  var tag = el.tagName.toLowerCase();
  var bits = [tag];
  if (el.type) bits.push('type="'+String(el.type)+'"');
  if (el.name) bits.push('name="'+String(el.name)+'"');
  if (el.placeholder) bits.push('placeholder="'+String(el.placeholder)+'"');
  if (tag === 'a' && el.href) bits.push('href="'+String(el.href)+'"');
  return '[' + bits.join(' ') + ']';
}

var attrName = '__bb_snap_ref';
var stateProp = '__bbSnapState';

function refState(){
  var st = document[stateProp];
  if (!st || typeof st.next !== 'number' || !(st.next >= 1) || !st.marked || typeof st.marked.length !== 'number') {
    st = { next: 1, marked: [] };
    Object.defineProperty(document, stateProp, { value: st, configurable: true, writable: true, enumerable: false });
  }
  return st;
}

function invalidateRefs(st){
  function unmark(el){
    if (!el || !el.getAttribute || !el.hasAttribute(attrName)) return;
    var v = Number(el.getAttribute(attrName));
    if (Math.floor(v) === v && v >= st.next && v < 9007199254740991) st.next = v + 1;
    el.removeAttribute(attrName);
  }
  var prev = st.marked;
  st.marked = [];
  for (var i = 0; i < prev.length; i++) unmark(prev[i]);
  var stale = document.querySelectorAll('[' + attrName + ']');
  for (var j = 0; j < stale.length; j++) unmark(stale[j]);
}

function snapshotEl(el, depth, st){
  if (!el || !el.tagName) return null;
  var tag = el.tagName.toLowerCase();
  if (tag === 'script' || tag === 'style' || tag === 'noscript') return null;
  if (!visible(el)) return null;
  if (maxDepth && depth > maxDepth) return null;

  var childSnaps = [];
  var ch = el.children || [];
  for (var i = 0; i < ch.length; i++) {
    var cs = snapshotEl(ch[i], depth + 1, st);
    if (cs) childSnaps.push(cs);
  }

  var ia = isInteractive(el);
  if (interactiveOnly && !ia && childSnaps.length === 0) return null;

  var txt = trim(el.innerText||'');
  var label = '';
  if (txt.length > 120) txt = txt.slice(0,117)+'...';
  if (tag === 'input' || tag === 'textarea') {
    if (el.value) label = trim(String(el.value)).slice(0,80);
  }
  if (!label && txt) label = txt;

  var entry = null;
  if (!(pruneEmpty && childSnaps.length===0 && !ia && !label)) {
    var id = st.next++;
    el.setAttribute(attrName, String(id));
    st.marked.push(el);
    entry = {
      id: id,
      lineDepth: depth,
      head: '@' + id + ' ' + attrs(el) + (label ? ' ' + JSON.stringify(label) : ''),
      selector: '[' + attrName + '="' + String(id).replace(/"/g,'\\"') + '"]'
    };
  }

  return { entry: entry, kids: childSnaps };
}

function flatten(tree, lines, refs){
  if (!tree) return;
  if (tree.entry){
    var pad = '';
    for (var i=0;i<tree.entry.lineDepth;i++) pad += '  ';
    lines.push(pad + tree.entry.head);
    refs[String(tree.entry.id)] = tree.entry.selector;
  }
  for (var j=0;j<tree.kids.length;j++) flatten(tree.kids[j], lines, refs);
}

var root = scopeSel ? document.querySelector(scopeSel) : document.body;
var st = refState();
invalidateRefs(st);
if (!root) {
  return { title: document.title||'', url: location.href, text: '', refs: {} };
}
var tree = snapshotEl(root, 0, st);
var lines = [];
var refs = {};
flatten(tree, lines, refs);
var text = '页面: ' + (document.title||'') + '\nURL: ' + location.href + '\n\n' + lines.join('\n');
return { title: document.title||'', url: location.href, text: text, refs: refs };
})(%s)
`

// SnapshotOpts configures compact DOM snapshot for @ref → selector mapping.
type SnapshotOpts struct {
	InteractiveOnly bool
	PruneEmpty      bool
	MaxDepth        int
	SelectorScope   string
}

type snapshotPayload struct {
	Title string            `json:"title"`
	URL   string            `json:"url"`
	Text  string            `json:"text"`
	Refs  map[string]string `json:"refs"`
}

// SnapshotRefAttr is the DOM attribute snapshot uses to mark referenced nodes.
const SnapshotRefAttr = "__bb_snap_ref"

// SnapshotRefSelector expands a snapshot ref ("12" or "@12") into the CSS
// selector that click / fill resolve it with. It is the same selector the
// snapshot result reports in refs.
func SnapshotRefSelector(ref string) string {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "@")
	return fmt.Sprintf(`[%s="%s"]`, SnapshotRefAttr, strings.ReplaceAll(ref, `"`, `\"`))
}

// snapshotExpr builds the JavaScript expression evaluated in the page.
func snapshotExpr(opts SnapshotOpts) (string, error) {
	optMap := map[string]any{
		"interactiveOnly": opts.InteractiveOnly,
		"pruneEmpty":      opts.PruneEmpty,
		"selectorScope":   opts.SelectorScope,
	}
	if opts.MaxDepth > 0 {
		optMap["maxDepth"] = opts.MaxDepth
	}
	optJSON, err := json.Marshal(optMap)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(snapshotStub, string(optJSON)), nil
}

// Snapshot captures a compact tree snapshot and assigns __bb_snap_ref attributes for selectors.
// Refs of earlier snapshots of the same document stop resolving; see snapshotStub.
func (s *Session) Snapshot(tabID target.ID, opts SnapshotOpts) (title, url, text string, refs map[string]string, err error) {
	return s.SnapshotContext(context.Background(), tabID, opts)
}

func (s *Session) SnapshotContext(ctx context.Context, tabID target.ID, opts SnapshotOpts) (title, url, text string, refs map[string]string, err error) {
	if s == nil {
		return "", "", "", nil, errNilSession()
	}
	tabCtx, err := s.tabChromeCtxContext(ctx, tabID)
	if err != nil {
		return "", "", "", nil, err
	}
	expr, err := snapshotExpr(opts)
	if err != nil {
		return "", "", "", nil, err
	}

	var raw []byte
	err = runCDPWithContext(tabCtx, ctx, chromedp.Evaluate(expr, &raw))
	if err != nil {
		return "", "", "", nil, err
	}
	var p snapshotPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", "", nil, err
	}
	if p.Refs == nil {
		p.Refs = map[string]string{}
	}
	return p.Title, p.URL, p.Text, p.Refs, nil
}
