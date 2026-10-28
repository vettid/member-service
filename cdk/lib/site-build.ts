import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, renameSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, posix, relative, sep } from 'node:path';
import { HASHED_ASSET, HASH_LENGTH, isExcludedSitePath } from './static-site';

export { HASH_LENGTH };

/** Where builds are kept: in the CDK app's own (git-ignored) output directory. */
export const BUILD_ROOT = join(__dirname, '..', 'cdk.out', 'site-build');

export interface SiteBuild {
  /** Directory with the published files: the source with JS and CSS renamed. */
  readonly dir: string;
  /** Site-relative paths of the renamed files (`js/connect.<hash>.js`), sorted. */
  readonly hashed: string[];
}

// References rewritten to the hashed names. HTML: an absolute,
// double-quoted src or href to a .js or .css file. JS: the relative
// specifier of a static import or export (`from './x.js'`, `import
// './x.js'`). Any other form that names a JS or CSS file (a relative or
// single-quoted attribute, a query string, an absolute or dynamic import,
// a CSS @import) fails the build rather than loading a missing file. The
// check is textual: a comment that quotes such a name fails it too.
const HTML_REF = /\b(src|href)="(\/[^"?#]+\.(?:js|css))"/g;
const HTML_ANY = /\b(?:src|href)\s*=\s*["']?[^"'\s>]*\.(?:js|css)(?:[?#][^"'\s>]*)?["']?/gi;
const JS_IMPORT = /(\bfrom\s*|\bimport\s*)(['"])(\.\.?\/[^'"]+\.js)\2/g;
const JS_LOOSE = /(['"`])[^'"`\s]*\.(?:js|css)(?:[?#][^'"`\s]*)?\1/g;
// Not rewritten either: any dynamic import (static ones have no
// parenthesis), and a source map reference (map files keep their names).
const JS_DYNAMIC = /\bimport\s*\(|sourceMappingURL=/g;
const CSS_LOOSE = /@import|url\(\s*['"]?[^)'"]*\.(?:js|css)\b/gi;

/**
 * Copies the site at `srcDir` to a build directory, renaming every JS and
 * CSS file to `<name>.<hash>.<ext>` and rewriting the references to them in
 * the HTML pages and in JS imports. A module's hash covers its content
 * after its own imports were rewritten, so a change to a module renames
 * every module that imports it, directly or not. The hashed files can then
 * be cached as immutable; the HTML pages, which name them, revalidate.
 *
 * The result depends only on the source, and the build directory is named
 * after the source's hash, so repeated synths reuse it and the CDK asset
 * hash stays the same.
 */
export function buildSite(srcDir: string): SiteBuild {
  const files = walk(srcDir);
  const read = new Map(files.map((f) => [f, readFileSync(join(srcDir, ...f.split('/')))]));
  const text = (f: string) => read.get(f)!.toString('utf8');

  const assets = files.filter((f) => /\.(js|css)$/.test(f));
  const renamed = new Map<string, string>(); // site path -> hashed site path
  const out = new Map<string, Buffer>(); // site path (after rename) -> content

  // JS modules in dependency order (imports first); CSS has no references
  // to other hashed files (its url()s name fonts and images).
  const deps = new Map<string, string[]>();
  for (const f of assets.filter((a) => a.endsWith('.js'))) {
    const src = text(f);
    const imports = [...src.matchAll(JS_IMPORT)].map((m) => resolve(f, m[3]));
    for (const i of imports) {
      if (!read.has(i)) throw new Error(`site build: ${f} imports ${i}, which is not in the site`);
    }
    const loose = [...(src.replace(JS_IMPORT, '').match(JS_LOOSE) ?? []), ...(src.match(JS_DYNAMIC) ?? [])];
    if (loose.length) throw new Error(`site build: ${f} names ${loose.join(', ')} outside a static import`);
    deps.set(f, imports);
  }
  const done = new Set<string>();
  const visit = (f: string, path: string[]) => {
    if (done.has(f)) return;
    if (path.includes(f)) throw new Error(`site build: import cycle ${[...path, f].join(' -> ')}`);
    for (const d of deps.get(f) ?? []) visit(d, [...path, f]);
    let body = read.get(f)!;
    if (f.endsWith('.js')) {
      body = Buffer.from(text(f).replace(JS_IMPORT, (_, kw, q, spec) => `${kw}${q}${relativeSpec(f, renamed.get(resolve(f, spec))!)}${q}`));
    }
    const name = hashedName(f, body);
    renamed.set(f, name);
    out.set(name, body);
    done.add(f);
  };
  for (const f of assets) visit(f, []);
  for (const f of assets.filter((a) => a.endsWith('.css'))) {
    const loose = text(f).match(CSS_LOOSE);
    if (loose) throw new Error(`site build: ${f} names ${loose.join(', ')}, which the build doesn't rewrite`);
  }

  for (const f of files) {
    if (renamed.has(f)) continue;
    if (!f.endsWith('.html')) {
      out.set(f, read.get(f)!);
      continue;
    }
    const html = text(f).replace(HTML_REF, (_, attr, ref) => {
      const name = renamed.get(ref.slice(1));
      if (!name) throw new Error(`site build: ${f} refers to ${ref}, which is not in the site`);
      return `${attr}="/${name}"`;
    });
    // After the rewrite, every JS or CSS reference must be one it made.
    const loose = (html.match(HTML_ANY) ?? []).filter((m) => {
      const ref = /^(?:src|href)="(\/[^"?#]+)"$/.exec(m)?.[1];
      return !ref || !HASHED_ASSET.test(ref);
    });
    if (loose.length) throw new Error(`site build: ${f} names ${loose.join(', ')}, which the build doesn't rewrite`);
    out.set(f, Buffer.from(html));
  }

  const digest = createHash('sha256');
  for (const [f, body] of [...out].sort(([a], [b]) => (a < b ? -1 : 1))) {
    digest.update(`${f}\0${body.length}\0`).update(body);
  }
  const root = BUILD_ROOT;
  const dir = join(root, digest.digest('hex').slice(0, 32));
  if (existsSync(dir) && !holds(dir, out)) rmSync(dir, { recursive: true, force: true });
  if (!existsSync(dir)) {
    // Written aside and renamed into place, so a concurrent synth never
    // sees a half-written directory.
    mkdirSync(root, { recursive: true });
    const tmp = mkdtempSync(join(root, 'build-'));
    for (const [f, body] of out) {
      const p = join(tmp, ...f.split('/'));
      mkdirSync(dirname(p), { recursive: true });
      writeFileSync(p, body);
    }
    try {
      renameSync(tmp, dir);
    } catch (e) {
      rmSync(tmp, { recursive: true, force: true });
      if (!existsSync(dir)) throw e;
    }
  }
  return { dir, hashed: [...renamed.values()].sort() };
}

/** Site-relative paths of every file to publish, `/`-separated, without SITE_EXCLUDE matches. */
function walk(dir: string): string[] {
  const all = (d: string): string[] =>
    readdirSync(d).flatMap((n) => {
      const p = join(d, n);
      return statSync(p).isDirectory() ? all(p) : [relative(dir, p).split(sep).join('/')];
    });
  return all(dir).filter((p) => !isExcludedSitePath(p)).sort();
}

/** True if `dir` holds exactly the files in `out`, byte for byte. */
function holds(dir: string, out: Map<string, Buffer>): boolean {
  try {
    const have = walk(dir);
    return have.length === out.size && have.every((f) => out.get(f)?.equals(readFileSync(join(dir, ...f.split('/')))));
  } catch {
    return false;
  }
}

function resolve(from: string, spec: string): string {
  return posix.normalize(posix.join(posix.dirname(from), spec));
}

function relativeSpec(from: string, to: string): string {
  const r = posix.relative(posix.dirname(from), to);
  return r.startsWith('.') ? r : `./${r}`;
}

function hashedName(f: string, body: Buffer): string {
  const hash = createHash('sha256').update(body).digest('hex').slice(0, HASH_LENGTH);
  return f.replace(/\.(js|css)$/, `.${hash}.$1`);
}
