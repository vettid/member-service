// Classic (non-module) script loaded synchronously in <head>, before the
// stylesheet, so an explicit Light/Dark choice applies before first paint.
// No data-theme attribute = follow the OS (prefers-color-scheme).
(function () {
  try {
    var t = window.localStorage.getItem('vettid-theme');
    if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
  } catch (e) {
    /* storage blocked — System default it is */
  }
})();
