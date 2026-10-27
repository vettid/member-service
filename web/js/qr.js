// QR codes drawn in the page (VAULT-MESSAGING §11.11.2: no third-party QR
// service). The encoder is the vendored qrcode-generator 2.0.4 (MIT, see
// js/vendor/qrcode-generator.LICENSE.txt); this file only turns its module
// grid into an SVG built with createElementNS (no HTML strings: CSP and
// Trusted Types).

import qrcode from './vendor/qrcode-generator.js';

const SVG = 'http://www.w3.org/2000/svg';
/** Quiet zone in modules (the QR standard asks for 4). */
const QUIET = 4;

/**
 * The module grid for an ASCII payload, byte mode, error correction M,
 * smallest version that fits. Returns an array of rows of booleans (true = dark).
 */
export function qrModules(text, level = 'M') {
  if (typeof text !== 'string' || !/^[\x20-\x7e]*$/.test(text)) throw new Error('qrModules: printable ASCII only');
  const qr = qrcode(0, level);
  qr.addData(text, 'Byte');
  qr.make();
  const n = qr.getModuleCount();
  const rows = [];
  for (let r = 0; r < n; r += 1) {
    const row = new Array(n);
    for (let c = 0; c < n; c += 1) row[c] = qr.isDark(r, c);
    rows.push(row);
  }
  return rows;
}

/** One SVG path for the dark modules: a horizontal run per stretch of dark cells. */
export function qrPath(modules) {
  const parts = [];
  modules.forEach((row, r) => {
    let c = 0;
    while (c < row.length) {
      if (!row[c]) {
        c += 1;
        continue;
      }
      const start = c;
      while (c < row.length && row[c]) c += 1;
      parts.push(`M${start + QUIET} ${r + QUIET}h${c - start}v1h${start - c}z`);
    }
  });
  return parts.join('');
}

/**
 * <svg role="img"> with the code: always dark on light, whatever the theme
 * (scanners expect that), scaled by CSS. `label` is its accessible name.
 */
export function qrSvg(modules, label) {
  const size = modules.length + 2 * QUIET;
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', `0 0 ${size} ${size}`);
  svg.setAttribute('class', 'qr');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', label);
  svg.setAttribute('shape-rendering', 'crispEdges');
  const bg = document.createElementNS(SVG, 'rect');
  bg.setAttribute('class', 'qr-light');
  bg.setAttribute('width', String(size));
  bg.setAttribute('height', String(size));
  const fg = document.createElementNS(SVG, 'path');
  fg.setAttribute('class', 'qr-dark');
  fg.setAttribute('d', qrPath(modules));
  svg.append(bg, fg);
  return svg;
}
