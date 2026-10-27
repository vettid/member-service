// A tiny DOM for running the site's page scripts under node:test: the
// elements of a page's HTML by id (with their form controls), and what
// dom.js, ui.js and qr.js use. No HTML is ever parsed into nodes beyond
// that; the pages only build nodes with createElement.

import { readFileSync } from 'node:fs';

class Node {
  constructor() {
    this.children = [];
    this.parent = null;
  }
  append(...kids) {
    for (const k of kids) {
      const n = typeof k === 'string' ? new Text(k) : k;
      n.parent = this;
      this.children.push(n);
    }
  }
  replaceChildren(...kids) {
    this.children = [];
    this.append(...kids);
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
  }
  set textContent(v) {
    this.children = [];
    if (v !== '') this.append(new Text(String(v)));
  }
}

class Text extends Node {
  constructor(t) {
    super();
    this.text = t;
  }
  get textContent() {
    return this.text;
  }
}

class Element extends Node {
  constructor(tag, attrs = {}) {
    super();
    this.tagName = tag.toUpperCase();
    this.attrs = { ...attrs };
    this.hidden = 'hidden' in attrs;
    this.disabled = false;
    this.required = 'required' in attrs;
    this.value = '';
    this.listeners = {};
    this.focused = false;
    this.controls = [];
  }
  get id() {
    return this.attrs.id;
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
  }
  getAttribute(k) {
    return k in this.attrs ? this.attrs[k] : null;
  }
  removeAttribute(k) {
    delete this.attrs[k];
  }
  set className(v) {
    this.attrs.class = v;
  }
  get className() {
    return this.attrs.class ?? '';
  }
  set href(v) {
    this.attrs.href = v;
  }
  get href() {
    return this.attrs.href;
  }
  addEventListener(ev, fn) {
    (this.listeners[ev] ??= []).push(fn);
  }
  async dispatch(ev) {
    const e = { preventDefault() {}, target: this };
    for (const fn of this.listeners[ev] ?? []) await fn(e);
  }
  focus() {
    this.focused = true;
  }
  select() {}
  reset() {
    for (const c of this.controls) c.value = '';
  }
  get elements() {
    const out = {};
    for (const c of this.controls) out[c.attrs.name ?? c.id] = c;
    return out;
  }
  querySelectorAll(sel) {
    if (sel !== 'button') throw new Error(`fakedom: selector ${sel}`);
    return this.controls.filter((c) => c instanceof HTMLButtonElement);
  }
  querySelector(sel) {
    if (sel !== 'button[type="submit"]') throw new Error(`fakedom: selector ${sel}`);
    return this.controls.find((c) => c instanceof HTMLButtonElement && c.attrs.type === 'submit') ?? null;
  }
}

class HTMLButtonElement extends Element {}

function make(tag, attrs) {
  return tag === 'button' ? new HTMLButtonElement(tag, attrs) : new Element(tag, attrs);
}

function attrsOf(s) {
  const out = {};
  for (const m of s.matchAll(/([a-zA-Z-:]+)(?:="([^"]*)")?/g)) out[m[1]] = m[2] ?? '';
  return out;
}

/** load(html file): installs document, window and friends; returns the page. */
export function load(file, { search = '' } = {}) {
  const html = readFileSync(file, 'utf8');
  const byId = new Map();
  const forms = [];
  for (const m of html.matchAll(/<(\w+)\s([^>]*?)>|<\/form>/g)) {
    if (m[0] === '</form>') {
      forms.pop();
      continue;
    }
    const attrs = attrsOf(m[2]);
    const node = make(m[1], attrs);
    if (m[1] === 'form') forms.push(node);
    if (forms.length && (m[1] === 'input' || m[1] === 'button')) forms.at(-1).controls.push(node);
    if (attrs.id) byId.set(attrs.id, node);
    if (m[1] === 'form') byId.set(attrs.id, node);
  }
  const page = {
    byId,
    replaced: null,
    get: (id) => {
      const n = byId.get(id);
      if (!n) throw new Error(`fakedom: no #${id}`);
      return n;
    },
  };
  globalThis.Node = Node;
  globalThis.HTMLButtonElement = HTMLButtonElement;
  globalThis.document = {
    getElementById: (id) => byId.get(id) ?? null,
    createElement: (tag) => make(tag, {}),
    createElementNS: (_ns, tag) => make(tag, {}),
    createTextNode: (t) => new Text(t),
  };
  globalThis.location = {
    search,
    pathname: file.replace(/.*\/web/, '').replace(/index\.html$/, ''),
    replace: (to) => {
      page.replaced = to;
    },
  };
  globalThis.history = { replaceState() {} };
  return page;
}

/** Scripted fetch: routes['GET /api/x'] is a list of [status, body] answers, used in turn (the last repeats). */
export function api(routes) {
  const calls = [];
  globalThis.fetch = async (path, init) => {
    const key = `${init.method} ${path}`;
    calls.push({ key, body: init.body ? JSON.parse(init.body) : undefined, headers: init.headers });
    const list = routes[key];
    if (!list) throw new Error(`fakedom: no route ${key}`);
    const [status, body] = list.length > 1 ? list.shift() : list[0];
    if (status === 0) throw new TypeError('network'); // status 0: the network failed
    return { ok: status >= 200 && status < 300, status, text: async () => (body === undefined ? '' : JSON.stringify(body)), headers: new Map() };
  };
  return calls;
}

/**
 * Timers and Date.now run by hand: tick() runs every timer due now;
 * advance(ms) moves the clock. pending() counts live timers, so a test
 * can see a poll that doubled up or kept running.
 */
export function clock(start = Date.parse('2026-10-27T12:00:00Z')) {
  let now = start;
  let next = 0;
  const timers = new Map();
  globalThis.setTimeout = (fn) => {
    timers.set(++next, fn);
    return next;
  };
  globalThis.clearTimeout = (id) => timers.delete(id);
  Date.now = () => now;
  return {
    async tick() {
      const due = [...timers.entries()];
      timers.clear();
      for (const [, fn] of due) await fn();
      await settle();
    },
    advance(ms) {
      now += ms;
    },
    pending: () => timers.size,
  };
}

export async function settle() {
  for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
}

let n = 0;
/** Runs a page script afresh (its own module instance). */
export async function run(script) {
  await import(new URL(`../web/js/${script}?${++n}`, import.meta.url));
  await settle();
}
