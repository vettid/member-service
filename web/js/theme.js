// Theme glyph in the header: a moon while the page is light (click -> dark),
// a sun while it is dark (click -> light). With no stored choice the page
// follows the OS, live. A click stores the explicit choice in localStorage
// ('vettid-theme') — the only storage this site uses, a per-device
// convenience; every access is guarded. js/theme-init.js applies a stored
// choice before first paint.

const KEY = 'vettid-theme';
const SVG = 'http://www.w3.org/2000/svg';
const os = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;

function stored() {
  try {
    const v = window.localStorage.getItem(KEY);
    return v === 'light' || v === 'dark' ? v : null;
  } catch {
    return null;
  }
}

/** The theme actually showing: explicit choice, else the OS (dark if unknown). */
function current() {
  const explicit = document.documentElement.getAttribute('data-theme');
  if (explicit === 'light' || explicit === 'dark') return explicit;
  return os && !os.matches ? 'light' : 'dark';
}

function svgEl(tag, attrs) {
  const node = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  return node;
}

// Simple, standard glyphs (24x24 grid, stroke = currentColor).
function moon() {
  return [svgEl('path', { d: 'M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z' })];
}
function sun() {
  return [
    svgEl('circle', { cx: '12', cy: '12', r: '4' }),
    ...['M12 2v2', 'M12 20v2', 'M2 12h2', 'M20 12h2', 'M4.9 4.9l1.4 1.4', 'M17.7 17.7l1.4 1.4', 'M4.9 19.1l1.4-1.4', 'M17.7 6.3l1.4-1.4']
      .map((d) => svgEl('path', { d })),
  ];
}

function render(btn) {
  const dark = current() === 'dark';
  const svg = svgEl('svg', { viewBox: '0 0 24 24', 'aria-hidden': 'true', focusable: 'false', class: 'theme-icon' });
  svg.append(...(dark ? sun() : moon()));
  btn.replaceChildren(svg);
  const label = dark ? 'Switch to light theme' : 'Switch to dark theme';
  btn.setAttribute('aria-label', label);
  btn.title = label;
}

const btn = document.getElementById('theme-toggle');
if (btn) {
  // A stale or invalid stored value must not leave a half-applied state.
  const choice = stored();
  if (choice) document.documentElement.setAttribute('data-theme', choice);
  render(btn);
  btn.hidden = false;
  btn.addEventListener('click', () => {
    const next = current() === 'dark' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', next);
    try {
      window.localStorage.setItem(KEY, next);
    } catch {
      /* not persisted; still applies for this page view */
    }
    render(btn);
  });
  // Follow live OS changes while no explicit choice is stored.
  os?.addEventListener?.('change', () => {
    if (!stored()) {
      document.documentElement.removeAttribute('data-theme');
      render(btn);
    }
  });
}
