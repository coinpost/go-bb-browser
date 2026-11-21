// 用法: node snapshot_stale_refs.js <repo>/internal/browser/snapshot.go
// 在最小 fake DOM 上执行 main 的 snapshotStub（原样从 Go 源码提取），验证 @ref 是否唯一指向
// 最新一次 snapshot 返回的元素。click/fill 的 ref 会被 daemon 展开为 [__bb_snap_ref="N"] 并用
// querySelector 语义（文档序第一个匹配）解析（internal/daemon/v1.go:768-771, 1190-1193）。
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const stub = src.match(/const snapshotStub = `([\s\S]*?)`/)[1];

function el(tag, opts = {}, kids = []) {
  const e = {
    tagName: tag.toUpperCase(), id: opts.id || '', attrs: {}, style: { visibility: 'visible', display: 'block', opacity: '1' },
    children: kids, innerText: opts.text || '', type: opts.type || '', name: '', placeholder: '', value: '',
    getAttribute(n) { return this.attrs[n] ?? null; },
    setAttribute(n, v) { this.attrs[n] = String(v); },
    getBoundingClientRect() { return this.style.display === 'none' ? { width: 0, height: 0 } : { width: 10, height: 10 }; },
  };
  return e;
}
function walk(root, fn) { const r = fn(root); if (r) return r; for (const c of root.children) { const x = walk(c, fn); if (x) return x; } return null; }
function makeDoc(body) {
  return {
    title: 't', body,
    querySelector(sel) {
      let m;
      if ((m = sel.match(/^#(.+)$/))) return walk(body, e => e.id === m[1] ? e : null);
      if ((m = sel.match(/^\[__bb_snap_ref="(.+)"\]$/))) return walk(body, e => e.attrs.__bb_snap_ref === m[1] ? e : null);
      throw new Error('unsupported selector ' + sel);
    },
  };
}
function snapshot(doc, opts) {
  const fn = new Function('document', 'window', 'location', 'return ' + stub.trim().replace('%s', JSON.stringify(opts)));
  return fn(doc, { getComputedStyle: e => e.style }, { href: 'https://example.test/' });
}
function refOf(result, needle) {
  const line = result.text.split('\n').find(l => l.includes(needle));
  return line.trim().match(/^@(\d+)/)[1];
}

let failed = 0;
function check(name, doc, result, needle, wantId) {
  const ref = refOf(result, needle);
  const hit = doc.querySelector(result.refs[ref]);
  const ok = hit && hit.id === wantId;
  console.log(`${ok ? 'PASS' : 'FAIL'} ${name}: latest snapshot says @${ref} = <${needle}>, selector ${result.refs[ref]} resolves to #${hit && hit.id}${ok ? '' : ` (want #${wantId})`}`);
  if (!ok) failed++;
}

// 场景 1：先全页 snapshot，再带 --scope 的 snapshot。
{
  const submit = el('button', { id: 'submit', text: 'Submit order' });
  const del = el('button', { id: 'delete-account', text: 'Delete account' });
  const form = el('div', { id: 'form' }, [submit]);
  const body = el('body', { id: 'body' }, [del, form]);
  const doc = makeDoc(body);
  snapshot(doc, {});                                     // 后序编号：delete=1 submit=2 form=3 body=4
  const second = snapshot(doc, { selectorScope: '#form' }); // submit=1 form=2；delete 仍带旧的 "1"
  check('scoped snapshot after full snapshot', doc, second, 'Submit order', 'submit');
}
// 场景 2：两次全页 snapshot 之间，一个元素变为不可见（不再被遍历，旧属性残留）。
{
  const banner = el('button', { id: 'cookie-accept', text: 'Accept cookies' });
  const pay = el('button', { id: 'pay', text: 'Pay now' });
  const body = el('body', { id: 'body' }, [banner, pay]);
  const doc = makeDoc(body);
  snapshot(doc, {});                 // banner=1 pay=2 body=3
  banner.style.display = 'none';     // 横幅被隐藏
  const second = snapshot(doc, {});  // pay=1 body=2；banner 仍带旧的 "1"
  check('element hidden between snapshots', doc, second, 'Pay now', 'pay');
}
process.exit(failed ? 1 : 0);
