// Join (MEMBER-SERVICE-PLAN §4.1): an emailed code proves the address,
// then the invitation opens in the VettID app (a button on a phone, a QR
// code on a computer), then the member compares the SAS and approves on
// the phone. GET /api/connect/status drives every step after the code.

import { poll, pollMessages, post } from './api.js';
import { $, el, fill } from './dom.js';
import { qrModules, qrSvg } from './qr.js';
import * as ui from './ui.js';

ui.init();

const steps = ['step-email', 'step-code', 'step-invite', 'step-sas', 'step-end'].map($);
const emailForm = $('email-form');
const codeForm = $('code-form');
let email = '';
let stop = null;
let until = null; // the invitation's expiry, for the poll

function show(id) {
  for (const s of steps) s.hidden = s.id !== id;
  if (id !== 'step-email') $(id).focus();
}

function codeStep() {
  // After a reload the page no longer knows the address: ask for it again
  // with the code (the API never stores it).
  $('code-email-field').hidden = email !== '';
  $('code-email').required = email === '';
  $('code-to').textContent = email || 'your address';
  codeForm.elements.code.value = '';
  show('step-code');
}

function invite(url, expiresAt) {
  $('invite-link').href = url;
  const box = $('invite-qr');
  try {
    fill(box, qrSvg(qrModules(url), 'QR code of the invitation link'), el('span', { class: 'code-label' }, 'Scan with the VettID app'));
    box.hidden = false;
  } catch {
    box.hidden = true;
  }
  const exp = new Date(expiresAt);
  until = Number.isNaN(exp.getTime()) ? null : exp;
  $('invite-exp').textContent = Number.isNaN(exp.getTime())
    ? ''
    : `The invitation works until ${exp.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })}.`;
  show('step-invite');
}

const ENDINGS = {
  declined: ['The connection was declined', 'It was declined on the phone. If that wasn’t you, start again.'],
  expired: ['The invitation ended', 'It wasn’t completed in time. You can start again.'],
};

function end(status) {
  stop?.();
  const [title, text] = ENDINGS[status] ?? ENDINGS.expired;
  $('end-title').textContent = title;
  $('end-text').textContent = text;
  show('step-end');
}

/** Apply a status; true when the flow is over. */
function apply(st) {
  if (!st) {
    end('expired');
    return true;
  }
  const exp = new Date(st.expires_at);
  if (!Number.isNaN(exp.getTime())) until = exp;
  switch (st.status) {
    case 'code':
      return false;
    case 'waiting':
      if ($('step-invite').hidden) invite(st.url, st.expires_at);
      return false;
    case 'sas':
      if ($('sas').textContent !== st.sas) $('sas').textContent = st.sas;
      if ($('step-sas').hidden) show('step-sas');
      return false;
    case 'connected':
      location.replace('/home/');
      return true;
    default:
      end(st.status);
      return true;
  }
}

function watch() {
  stop?.();
  stop = poll('/api/connect/status', apply, { ...pollMessages(ui), until });
}

emailForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  const value = emailForm.elements.email.value.trim();
  if (!value) return;
  const res = await ui.busy(emailForm, () => post('/api/connect/start', { email: value }), { busyLabel: 'Sending…' });
  if (!res) return;
  email = value;
  codeStep();
});

codeForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  const address = email || $('code-email').value.trim();
  const code = codeForm.elements.code.value.trim();
  if (!address || !/^[0-9]{6}$/.test(code)) {
    ui.showError('Enter the 6-digit code from the email.');
    return;
  }
  const res = await ui.busy(codeForm, () => post('/api/connect/verify', { email: address, code }), { busyLabel: 'Checking…' });
  if (!res) return;
  invite(res.url, res.expires_at);
  watch();
});

// Back to the first step, with the address filled in when the page
// knows it, for a new code or a different address.
function restart() {
  stop?.();
  ui.clearMessages();
  emailForm.reset();
  emailForm.elements.email.value = email;
  email = '';
  show('step-email');
  emailForm.elements.email.focus();
}
$('restart').addEventListener('click', restart);
$('again').addEventListener('click', restart);

// Back on the page mid-flow? Pick it up where it was. If the API can't
// be reached, say so and keep asking rather than start over (the
// invitation may still be open on the phone).
stop = poll('/api/connect/status', (st) => {
  if (!st) show('step-email');
  else if (st.status === 'code') codeStep();
  else if (!apply(st)) watch();
  return true;
}, { ...pollMessages(ui), now: true });
