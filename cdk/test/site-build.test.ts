import { mkdirSync, mkdtempSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { HASH_LENGTH, buildSite } from '../lib/site-build';
import { SITE_DIR } from '../lib/member-service-stack';

/** A throwaway site from `{ path: content }`. */
const site = (files: Record<string, string>) => {
  const dir = mkdtempSync(join(tmpdir(), 'site-build-test-'));
  for (const [f, body] of Object.entries(files)) {
    mkdirSync(dirname(join(dir, f)), { recursive: true });
    writeFileSync(join(dir, f), body);
  }
  return dir;
};
const read = (dir: string, f: string) => readFileSync(join(dir, f), 'utf8');
const H = `[0-9a-f]{${HASH_LENGTH}}`;
const BASE = {
  'index.html': '<link rel="stylesheet" href="/styles.css">\n<script type="module" src="/js/app.js"></script>',
  'styles.css': "body { font: url('/assets/f.woff2'); }",
  'assets/f.woff2': 'font',
  'js/app.js': "import { x } from './lib.js';\nimport q from './vendor/q.js';\nx(q);",
  'js/lib.js': "export const x = (v) => v;",
  'js/vendor/q.js': 'export default 1;',
};

test('renames JS and CSS, rewrites the references, leaves the rest', () => {
  const b = buildSite(site(BASE));
  const name = (re: string) => b.hashed.find((h) => new RegExp(`^${re}$`).test(h))!;
  const app = name(`js/app\\.${H}\\.js`);
  const lib = name(`js/lib\\.${H}\\.js`);
  const q = name(`js/vendor/q\\.${H}\\.js`);
  const css = name(`styles\\.${H}\\.css`);
  expect(b.hashed).toEqual([app, lib, q, css].sort());

  expect(read(b.dir, 'index.html')).toBe(`<link rel="stylesheet" href="/${css}">\n<script type="module" src="/${app}"></script>`);
  expect(read(b.dir, app)).toBe(`import { x } from './${lib.slice(3)}';\nimport q from './${q.slice(3)}';\nx(q);`);
  expect(read(b.dir, css)).toBe(BASE['styles.css']);
  expect(read(b.dir, 'assets/f.woff2')).toBe('font');
  expect(readdirSync(join(b.dir, 'js')).sort()).toEqual([app.slice(3), lib.slice(3), 'vendor'].sort());
});

test('a change to an imported module renames its importers', () => {
  const a = buildSite(site(BASE));
  const b = buildSite(site({ ...BASE, 'js/lib.js': 'export const x = (v) => v + 1;' }));
  const changed = a.hashed.filter((h) => !b.hashed.includes(h));
  expect(changed.map((h) => h.replace(new RegExp(`\\.${H}\\.`), '.'))).toEqual(['js/app.js', 'js/lib.js']);
});

test('same source, same directory', () => {
  expect(buildSite(site(BASE)).dir).toBe(buildSite(site(BASE)).dir);
});

test('refuses references it cannot rewrite', () => {
  expect(() => buildSite(site({ ...BASE, 'index.html': '<script src="/js/gone.js"></script>' }))).toThrow(/gone\.js, which is not in the site/);
  expect(() => buildSite(site({ ...BASE, 'js/lib.js': "import './gone.js';" }))).toThrow(/imports js\/gone\.js/);
  expect(() => buildSite(site({ ...BASE, 'js/lib.js': "await import('./app.js');" }))).toThrow(/outside a static import/);
  expect(() => buildSite(site({ ...BASE, 'js/lib.js': "import './app.js';" }))).toThrow(/import cycle/);
});

test('skips files the deployment excludes', () => {
  const b = buildSite(site({ ...BASE, 'js/.app.js.swp': 'x', '.env': 'x' }));
  expect(readdirSync(b.dir)).not.toContain('.env');
  expect(readdirSync(join(b.dir, 'js'))).not.toContain('.app.js.swp');
});

test('the real site builds, and every page names only hashed JS and CSS', () => {
  const b = buildSite(SITE_DIR);
  expect(b.hashed.length).toBeGreaterThan(0);
  for (const page of ['index.html', '404.html', 'connect/index.html', 'signin/index.html', 'home/index.html']) {
    const refs = [...read(b.dir, page).matchAll(/(?:src|href)="\/([^"]+\.(?:js|css))"/g)].map((m) => m[1]);
    expect(refs.length).toBeGreaterThan(0);
    for (const r of refs) expect(b.hashed).toContain(r);
  }
});
