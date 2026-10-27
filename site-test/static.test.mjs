// The site under the StaticSite construct's headers (vettid.org
// lib/constructs/static-site.ts): script-src and style-src 'self' with no
// inline code, Trusted Types (no HTML-string sinks), img/font/connect
// 'self'. And every local reference resolves to a file.

import assert from 'node:assert/strict';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { test } from 'node:test';

const web = new URL('../web/', import.meta.url).pathname;

function files(dir, out = []) {
  for (const name of readdirSync(dir)) {
    if (name.startsWith('.')) continue;
    const p = join(dir, name);
    if (statSync(p).isDirectory()) files(p, out);
    else out.push(p);
  }
  return out;
}

const all = files(web);
const html = all.filter((f) => f.endsWith('.html'));
const js = all.filter((f) => f.endsWith('.js') && !f.includes('/vendor/'));

test('HTML: no inline script, style or handlers; local references exist', () => {
  assert.ok(html.length >= 5);
  for (const f of html) {
    const s = readFileSync(f, 'utf8');
    const where = relative(web, f);
    assert.doesNotMatch(s, /<script(?![^>]*\bsrc=)[^>]*>/, `${where}: inline script`);
    assert.doesNotMatch(s, /<style\b|\sstyle="/, `${where}: inline style`);
    assert.doesNotMatch(s, /\son[a-z]+="/, `${where}: inline handler`);
    assert.match(s, /<meta name="referrer" content="no-referrer">/, `${where}: referrer`);
    for (const m of s.matchAll(/<(\w+)[^>]*\s(src|href)="([^"]+)"/g)) {
      const [, tag, attr, ref] = m;
      // Links may leave the site; anything the page loads may not.
      if (tag === 'a' && attr === 'href' && /^(https:\/\/vettid\.org\/?|mailto:|#)/.test(ref)) continue;
      assert.ok(ref.startsWith('/'), `${where}: ${ref} is not same-origin`);
      if (ref.endsWith('/')) assert.ok(existsSync(join(web, ref, 'index.html')), `${where}: ${ref}`);
      else assert.ok(existsSync(join(web, ref)), `${where}: ${ref}`);
    }
  }
});

test('JS: no HTML-string sinks or eval; imports resolve; fetch is same-origin', () => {
  for (const f of js) {
    const s = readFileSync(f, 'utf8');
    const where = relative(web, f);
    assert.doesNotMatch(s, /\.(innerHTML|outerHTML)\s*=|insertAdjacentHTML|document\.write|\beval\(|new Function\(/, where);
    for (const m of s.matchAll(/from '(\.[^']+)'/g)) assert.ok(existsSync(join(f, '..', m[1])), `${where}: ${m[1]}`);
    for (const m of s.matchAll(/fetch\(([^,)]+)/g)) assert.equal(m[1], 'path', `${where}: fetch(${m[1]})`);
  }
});

test('CSS: fonts and images are local', () => {
  for (const f of all.filter((x) => x.endsWith('.css'))) {
    const s = readFileSync(f, 'utf8');
    for (const m of s.matchAll(/url\(([^)]+)\)/g)) {
      const ref = m[1].replace(/['"]/g, '');
      assert.ok(!/^[a-z]+:/i.test(ref), `${relative(web, f)}: ${ref}`);
      assert.ok(existsSync(ref.startsWith('/') ? join(web, ref) : join(f, '..', ref)), `${relative(web, f)}: ${ref}`);
    }
  }
});
