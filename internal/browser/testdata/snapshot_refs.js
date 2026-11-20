// Runs the snapshot script (argv[2], with options already substituted) twice
// against a minimal fake DOM — whole page, then scoped to #form — and prints
// one line per ref of the second snapshot: "<ref> <ids of nodes carrying it>".
const fs = require('fs');
const script = fs.readFileSync(process.argv[2], 'utf8');

function el(tag, id, kids) {
  return {
    tagName: tag, id, children: kids || [], attrs: {}, innerText: tag + id,
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    setAttribute(k, v) { this.attrs[k] = v; },
    removeAttribute(k) { delete this.attrs[k]; },
    getBoundingClientRect() { return { width: 1, height: 1 }; },
  };
}
const form = el('FORM', 'form', [el('INPUT', 'user'), el('BUTTON', 'submit')]);
const body = el('BODY', 'body', [el('A', 'nav1'), el('A', 'nav2'), el('BUTTON', 'delete'), form]);
const all = [];
(function walk(e) { all.push(e); e.children.forEach(walk); })(body);

global.window = { getComputedStyle: () => ({ visibility: 'visible', display: 'block', opacity: '1' }) };
global.location = { href: 'https://example.test/' };
global.document = {
  title: 't', body,
  querySelector: (s) => all.find((e) => '#' + e.id === s) || null,
  querySelectorAll: () => all.filter((e) => '__bb_snap_ref' in e.attrs),
};

const run = (opts) => eval(script.replace('__OPTS__', JSON.stringify(opts)));
run({});
const second = run({ selectorScope: '#form' });
for (const ref of Object.keys(second.refs).sort()) {
  console.log(ref, all.filter((e) => e.attrs.__bb_snap_ref === ref).map((e) => e.id).join(','));
}
