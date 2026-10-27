// Sign in (MEMBER-SERVICE-PLAN §4.2): POST /api/signin/start {email}
// shows the match code; GET /api/signin/status says pending, approved
// (the session is set), confirm or ended. The page reads the same whether
// or not the address is a member's. confirm: approved with a changed
// credential key, so the member proves the address with an emailed code
// (POST /api/signin/confirm, then /api/signin/verify) before a session.

import { poll, pollMessages, post } from './api.js';
import { $ } from './dom.js';
import * as ui from './ui.js';

ui.init();

const steps = ['step-email', 'step-wait', 'step-confirm', 'step-end'].map($);
const form = $('email-form');
const confirmEmailForm = $('confirm-email-form');
const confirmCodeForm = $('confirm-code-form');
let stop = null;
let email = ''; // the address this page started with, kept in memory only

function show(id) {
  for (const s of steps) s.hidden = s.id !== id;
  const shown = $(id);
  if (id !== 'step-email') shown.focus();
}

function wait(matchCode, expiresAt) {
  $('match').textContent = matchCode;
  show('step-wait');
  stop?.();
  const until = new Date(expiresAt);
  stop = poll('/api/signin/status', (st) => {
    if (st?.status === 'approved') {
      location.replace('/home/');
      return true;
    }
    if (st?.status === 'confirm') {
      confirm(st.code_sent);
      return true;
    }
    if (!st || st.status === 'ended') {
      show('step-end');
      return true;
    }
    return false;
  }, { ...pollMessages(ui), until: Number.isNaN(until.getTime()) ? null : until });
}

/** The changed-key step: send the code (when the page knows the address), then take it. */
function confirm(codeSent) {
  show('step-confirm');
  if (codeSent) {
    codeStep();
  } else if (email) {
    sendConfirm(confirmCodeForm);
  } else {
    confirmEmailForm.hidden = false;
    confirmCodeForm.hidden = true;
    $('confirm-email').focus();
  }
}

function codeStep() {
  confirmEmailForm.hidden = true;
  confirmCodeForm.hidden = false;
  $('confirm-to').textContent = email || 'your address';
  $('confirm-code').value = '';
  $('confirm-code').focus();
}

async function sendConfirm(target) {
  const res = await ui.busy(target, () => post('/api/signin/confirm', { email }), { busyLabel: 'Sending…' });
  if (res) codeStep();
  else confirmEmailForm.hidden = false; // e.g. wrong_email: let the address be corrected
}

confirmEmailForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  email = $('confirm-email').value.trim();
  if (email) await sendConfirm(confirmEmailForm);
});

$('confirm-resend').addEventListener('click', async () => {
  if (!email) {
    confirmCodeForm.hidden = true;
    confirmEmailForm.hidden = false;
    return;
  }
  await sendConfirm($('confirm-resend'));
});

confirmCodeForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  const code = $('confirm-code').value.trim();
  if (!email) {
    confirmCodeForm.hidden = true;
    confirmEmailForm.hidden = false;
    return;
  }
  if (!/^[0-9]{6}$/.test(code)) {
    ui.showError('Enter the 6-digit code from the email.');
    return;
  }
  const res = await ui.busy(confirmCodeForm, () => post('/api/signin/verify', { email, code }), { busyLabel: 'Checking…' });
  if (res?.status === 'approved') location.replace('/home/');
  else if (res) show('step-end');
});

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  email = form.elements.email.value.trim();
  if (!email) return;
  const res = await ui.busy(form, () => post('/api/signin/start', { email }), { busyLabel: 'Starting…' });
  if (res) wait(res.match_code, res.expires_at);
});

function restart() {
  stop?.();
  ui.clearMessages();
  show('step-email');
  form.elements.email.focus();
}
$('cancel').addEventListener('click', restart);
$('again').addEventListener('click', restart);

// Back on the page with a sign-in still open? Pick it up. If the API
// can't be reached, say so and keep asking rather than start over.
stop = poll('/api/signin/status', (st) => {
  if (st?.status === 'pending') wait(st.match_code, st.expires_at);
  else if (st?.status === 'confirm') confirm(st.code_sent);
  else if (st?.status === 'approved') location.replace('/home/');
  else show('step-email');
  return true;
}, { ...pollMessages(ui), now: true });
