// Shared UI pieces: message regions (aria-live), busy buttons, cooldowns,
// labelled fields, and a keyboard-friendly <dialog> confirm.

import { el, fill } from './dom.js';

let errorRegion = null;
let noticeRegion = null;
let seq = 0;

export const uid = (prefix = 'u') => `${prefix}${++seq}`;

/** Wire the page's two live regions: #errors (role=alert) and #notices (role=status). */
export function init() {
  errorRegion = document.getElementById('errors');
  noticeRegion = document.getElementById('notices');
}

/** "about 2 minutes" style wording for a number of seconds. */
export function waitText(seconds) {
  const s = Math.max(1, Math.ceil(Number(seconds) || 0));
  if (s < 60) return `${s} second${s === 1 ? '' : 's'}`;
  const m = Math.ceil(s / 60);
  if (m < 60) return `${m} minute${m === 1 ? '' : 's'}`;
  const h = Math.ceil(m / 60);
  return `${h} hour${h === 1 ? '' : 's'}`;
}

/** Plain-language text for any thrown error. */
export function errorText(err) {
  if (err?.code === 'rate_limited' || err?.status === 429) {
    const base = 'Too many attempts.';
    return err.retry_after ? `${base} Please try again in ${waitText(err.retry_after)}.` : `${base} Please wait a little and try again.`;
  }
  return err?.message || 'Something went wrong. Please try again.';
}

function message(region, tone, text, dismissible) {
  if (!region) return;
  const nodes = [el('p', {}, text)];
  if (dismissible) {
    nodes.push(el('button', { type: 'button', class: 'btn btn-sm', on: { click: () => region.replaceChildren() } }, 'Dismiss'));
  }
  fill(region, el('div', { class: `msg msg-${tone}` }, nodes));
}

export function showError(errOrText) {
  const text = typeof errOrText === 'string' ? errOrText : errorText(errOrText);
  message(errorRegion, 'error', text, true);
}

/** tone: ok | info | warn */
export function showNotice(text, tone = 'ok') {
  message(noticeRegion, tone, text, true);
}

export function clearMessages() {
  errorRegion?.replaceChildren();
  noticeRegion?.replaceChildren();
}

/**
 * Run an async action with `target` (a button, or a form/container whose
 * buttons should all lock) disabled. Errors land in the error region unless
 * `onError` handles them. Returns fn's result, or undefined on error.
 */
export async function busy(target, fn, { busyLabel, onError } = {}) {
  const buttons = target instanceof HTMLButtonElement ? [target] : [...target.querySelectorAll('button')];
  const locked = buttons.filter((b) => !b.disabled);
  const labelled = busyLabel ? (target instanceof HTMLButtonElement ? target : target.querySelector('button[type="submit"]')) : null;
  const original = labelled?.textContent;
  for (const b of locked) b.disabled = true;
  target.setAttribute('aria-busy', 'true');
  if (labelled) labelled.textContent = busyLabel;
  clearMessages();
  try {
    return await fn();
  } catch (err) {
    if (onError) onError(err);
    else showError(err);
    return undefined;
  } finally {
    for (const b of locked) b.disabled = false;
    target.removeAttribute('aria-busy');
    if (labelled) labelled.textContent = original;
  }
}

/**
 * Disable a button for `seconds`, showing a countdown in its label.
 * Returns a cancel function. label(secondsLeft) -> string.
 */
export function cooldown(button, seconds, label) {
  const original = button.textContent;
  let left = Math.max(1, Math.ceil(seconds));
  button.disabled = true;
  button.textContent = label(left);
  const timer = setInterval(() => {
    left -= 1;
    if (left <= 0) stop();
    else button.textContent = label(left);
  }, 1000);
  function stop() {
    clearInterval(timer);
    button.disabled = false;
    button.textContent = original;
  }
  return stop;
}

/** Labelled form control with optional hint and an error slot. */
export function field(label, control, { hint, optional = false } = {}) {
  if (!control.id) control.id = uid('f');
  const describedBy = [];
  const hintNode = hint ? el('p', { class: 'hint', id: `${control.id}-hint` }, hint) : null;
  if (hintNode) describedBy.push(hintNode.id);
  const errNode = el('p', { class: 'field-error', id: `${control.id}-err`, hidden: true });
  describedBy.push(errNode.id);
  control.setAttribute('aria-describedby', describedBy.join(' '));
  const labelNode = el('label', { for: control.id }, label, optional ? el('span', { class: 'optional' }, ' (optional)') : null);
  return el('div', { class: 'field' }, labelNode, control, hintNode, errNode);
}

/** Show (or clear, with text = '') an inline error under a field() control. */
export function fieldError(control, text) {
  const errNode = document.getElementById(`${control.id}-err`);
  if (text) control.setAttribute('aria-invalid', 'true');
  else control.removeAttribute('aria-invalid');
  if (errNode) {
    errNode.textContent = text || '';
    errNode.hidden = !text;
  }
}

/** Checkbox with a wrapping label, styled as a bordered consent row. */
export function checkRow(input, labelContent) {
  if (!input.id) input.id = uid('c');
  return el('div', { class: 'check' }, input, el('label', { for: input.id }, labelContent));
}

export const PIN_RE = /^\d{4,8}$/;

/** A PIN entry box: hidden digits, numeric keypad, no autofill. */
export function pinInput(attrs = {}) {
  return el('input', {
    type: 'password',
    class: 'pin',
    inputmode: 'numeric',
    pattern: '[0-9]*',
    minlength: 4,
    maxlength: 8,
    autocomplete: 'off',
    spellcheck: 'false',
    required: true,
    ...attrs,
  });
}

/** Validate a PIN input; shows the inline error. Returns the digits or null. */
export function checkPin(input, what = 'PIN') {
  const v = input.value.trim();
  if (!PIN_RE.test(v)) {
    fieldError(input, `Your ${what} must be 4 to 8 digits (numbers only).`);
    return null;
  }
  fieldError(input, '');
  return v;
}

/**
 * Confirm dialog on <dialog>. Resolves true on confirm, false on Cancel/Esc.
 * tone: 'primary' | 'danger'. Focus starts on Cancel for danger dialogs.
 */
export function confirm({ title, body = [], confirmLabel = 'Confirm', cancelLabel = 'Cancel', tone = 'primary' }) {
  return new Promise((resolve) => {
    const returnFocus = document.activeElement;
    const titleId = uid('dlg');
    const ok = el('button', { type: 'submit', class: `btn ${tone === 'danger' ? 'btn-danger-solid' : 'btn-primary'}` }, confirmLabel);
    const cancel = el('button', { type: 'button', class: 'btn' }, cancelLabel);
    const form = el('form', { method: 'dialog', class: 'dlg-form' },
      el('h2', { id: titleId }, title),
      [body].flat().map((p) => (p instanceof Node ? p : el('p', {}, p))),
      el('div', { class: 'dlg-actions' }, cancel, ok),
    );
    const dlg = el('dialog', { class: 'dlg', 'aria-labelledby': titleId }, form);

    let result = false;
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      result = true;
      dlg.close();
    });
    cancel.addEventListener('click', () => dlg.close());
    dlg.addEventListener('close', () => {
      dlg.remove();
      if (returnFocus instanceof HTMLElement && returnFocus.isConnected) returnFocus.focus();
      resolve(result);
    });

    document.body.append(dlg);
    dlg.showModal();
    (tone === 'danger' ? cancel : ok).focus();
  });
}
