// Tiny DOM helpers. All text goes in via text nodes; there is deliberately no
// HTML-string path anywhere in this site (CSP + house style).

// Set as properties rather than attributes.
const PROPS = new Set(['disabled', 'hidden', 'required', 'readOnly', 'checked']);

/**
 * el('button', { class: 'btn', type: 'button', on: { click: fn } }, 'Label')
 * - `class` -> className, `for` -> htmlFor, `on` -> addEventListener map
 * - null / undefined / false attribute values are skipped
 * - children: Nodes, strings/numbers (as text), arrays (flattened), null/false skipped
 */
export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs ?? {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'for') node.htmlFor = v;
    else if (k === 'on') for (const [ev, fn] of Object.entries(v)) node.addEventListener(ev, fn);
    else if (PROPS.has(k)) node[k] = v;
    else node.setAttribute(k, v === true ? '' : String(v));
  }
  return add(node, children);
}

export function add(node, children) {
  for (const c of [children].flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

/** Replace a node's children (same child rules as el()). */
export function fill(node, ...children) {
  node.replaceChildren();
  return add(node, children);
}

export const $ = (id) => document.getElementById(id);

/** ISO-8601 -> "October 1, 2026" (viewer's locale) inside a <time>. */
export function date(iso) {
  if (!iso) return el('span', { class: 'muted' }, '—');
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return el('span', {}, iso);
  const text = new Intl.DateTimeFormat(undefined, { dateStyle: 'long' }).format(d);
  return el('time', { datetime: iso }, text);
}

/** Status chip. tone: ok | warn | err | mute */
export function chip(text, tone = 'mute') {
  return el('span', { class: `chip chip-${tone}` }, text);
}

export function mono(text, extraClass = '') {
  return el('code', { class: `mono ${extraClass}`.trim() }, text ?? '—');
}

/** Read a query-string parameter from the current URL. */
export function param(name) {
  return new URLSearchParams(location.search).get(name);
}
