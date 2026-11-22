// Minimal offline DOM used by snapshot_dom_test.go to execute the production
// snapshot script without a browser. It implements only what the script and
// the tests touch: element tree mutation, attributes, visibility, innerText
// and querySelector(All) for compound simple selectors
// (tag, #id, .class, [attr], [attr="value"]). Anything else is a SyntaxError,
// like an invalid selector in a real browser.
var window = this;

function FakeElement(tag) {
  this.tagName = String(tag).toUpperCase();
  this.children = [];
  this.parentNode = null;
  this.style = { display: '', visibility: '', opacity: '' };
  this._attrs = {};
  this._text = '';
}

FakeElement.prototype.getAttribute = function (name) {
  return Object.prototype.hasOwnProperty.call(this._attrs, name) ? this._attrs[name] : null;
};
FakeElement.prototype.hasAttribute = function (name) {
  return Object.prototype.hasOwnProperty.call(this._attrs, name);
};
FakeElement.prototype.setAttribute = function (name, value) {
  this._attrs[name] = String(value);
};
FakeElement.prototype.removeAttribute = function (name) {
  delete this._attrs[name];
};
FakeElement.prototype.removeChild = function (child) {
  var i = this.children.indexOf(child);
  if (i < 0) throw new Error('NotFoundError');
  this.children.splice(i, 1);
  child.parentNode = null;
  return child;
};
FakeElement.prototype.remove = function () {
  if (this.parentNode) this.parentNode.removeChild(this);
};
FakeElement.prototype.insertBefore = function (child, before) {
  child.remove();
  var i = before ? this.children.indexOf(before) : -1;
  if (i < 0) this.children.push(child); else this.children.splice(i, 0, child);
  child.parentNode = this;
  return child;
};
FakeElement.prototype.appendChild = function (child) {
  return this.insertBefore(child, null);
};
// cloneNode copies attributes, exactly like the real DOM does.
FakeElement.prototype.cloneNode = function (deep) {
  var c = new FakeElement(this.tagName);
  for (var k in this._attrs) c._attrs[k] = this._attrs[k];
  c._text = this._text;
  c.style = { display: this.style.display, visibility: this.style.visibility, opacity: this.style.opacity };
  if (deep) for (var i = 0; i < this.children.length; i++) c.appendChild(this.children[i].cloneNode(true));
  return c;
};
FakeElement.prototype._rendered = function () {
  for (var el = this; el; el = el.parentNode) {
    if (el.style.display === 'none') return false;
  }
  return this.isConnected;
};
FakeElement.prototype.getBoundingClientRect = function () {
  return this._rendered() ? { width: 10, height: 10 } : { width: 0, height: 0 };
};
FakeElement.prototype.querySelectorAll = function (sel) {
  var match = compileSelector(sel);
  var out = [];
  (function walk(el) {
    for (var i = 0; i < el.children.length; i++) {
      if (match(el.children[i])) out.push(el.children[i]);
      walk(el.children[i]);
    }
  })(this);
  return out;
};
FakeElement.prototype.querySelector = function (sel) {
  var all = this.querySelectorAll(sel);
  return all.length ? all[0] : null;
};
Object.defineProperty(FakeElement.prototype, 'isConnected', {
  get: function () {
    for (var el = this; el; el = el.parentNode) {
      if (el === document.documentElement) return true;
    }
    return false;
  }
});
Object.defineProperty(FakeElement.prototype, 'innerText', {
  get: function () {
    var parts = [];
    if (this._text) parts.push(this._text);
    for (var i = 0; i < this.children.length; i++) {
      var c = this.children[i];
      if (c.style.display === 'none' || c.style.visibility === 'hidden') continue;
      var t = c.innerText;
      if (t) parts.push(t);
    }
    return parts.join(' ');
  },
  set: function (v) { this._text = String(v); }
});
['id', 'type', 'name', 'placeholder', 'href', 'value'].forEach(function (prop) {
  Object.defineProperty(FakeElement.prototype, prop, {
    get: function () { var v = this.getAttribute(prop); return v === null ? '' : v; },
    set: function (v) { this.setAttribute(prop, v); }
  });
});

function compileSelector(sel) {
  sel = String(sel).trim();
  var re = /^(?:([a-zA-Z][a-zA-Z0-9]*)|#([\w-]+)|\.([\w-]+)|\[([\w-]+)(?:="((?:[^"\\]|\\.)*)")?\])/;
  var tests = [];
  var rest = sel;
  while (rest.length) {
    var m = re.exec(rest);
    if (!m) throw new SyntaxError("'" + sel + "' is not a valid selector");
    (function (m) {
      if (m[1] !== undefined) tests.push(function (el) { return el.tagName === m[1].toUpperCase(); });
      else if (m[2] !== undefined) tests.push(function (el) { return el.getAttribute('id') === m[2]; });
      else if (m[3] !== undefined) tests.push(function (el) {
        return (' ' + (el.getAttribute('class') || '') + ' ').indexOf(' ' + m[3] + ' ') >= 0;
      });
      else if (m[5] !== undefined) {
        var want = m[5].replace(/\\(.)/g, '$1');
        tests.push(function (el) { return el.getAttribute(m[4]) === want; });
      } else tests.push(function (el) { return el.hasAttribute(m[4]); });
    })(m);
    rest = rest.slice(m[0].length);
  }
  if (!tests.length) throw new SyntaxError("'' is not a valid selector");
  return function (el) {
    for (var i = 0; i < tests.length; i++) if (!tests[i](el)) return false;
    return true;
  };
}

var document = {
  title: 'Fake page',
  documentElement: new FakeElement('html'),
  createElement: function (tag) { return new FakeElement(tag); },
  querySelectorAll: function (sel) {
    var root = this.documentElement;
    var out = root.querySelectorAll(sel);
    if (compileSelector(sel)(root)) out.unshift(root);
    return out;
  },
  querySelector: function (sel) {
    var all = this.querySelectorAll(sel);
    return all.length ? all[0] : null;
  }
};
Object.defineProperty(document, 'body', {
  configurable: true,
  get: function () {
    var ch = this.documentElement.children;
    for (var i = 0; i < ch.length; i++) if (ch[i].tagName === 'BODY') return ch[i];
    return null;
  }
});
document.documentElement.appendChild(new FakeElement('body'));

var location = { href: 'https://fake.test/page' };
window.getComputedStyle = function (el) {
  return {
    display: el.style.display || 'block',
    visibility: el.style.visibility || 'visible',
    opacity: el.style.opacity === '' ? '1' : String(el.style.opacity)
  };
};

// --- helpers for tests -------------------------------------------------

// h('button', {id: 'x', text: 'Label'}, child...) builds an element.
function h(tag, props) {
  var el = document.createElement(tag);
  props = props || {};
  for (var k in props) {
    if (k === 'text') el.innerText = props[k]; else el.setAttribute(k, props[k]);
  }
  for (var i = 2; i < arguments.length; i++) el.appendChild(arguments[i]);
  return el;
}
function byId(id) {
  var el = document.querySelector('#' + id);
  if (!el) throw new Error('no element #' + id);
  return el;
}
// describe reports what a selector resolves to in the current document.
function describe(sel) {
  return document.querySelectorAll(sel).map(function (el) {
    return {
      id: el.id,
      tag: el.tagName.toLowerCase(),
      label: (el.value || el.innerText || '').replace(/\s+/g, ' ').trim(),
      attrs: JSON.parse(JSON.stringify(el._attrs))
    };
  });
}
