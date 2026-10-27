// Sign in (MEMBER-SERVICE-PLAN §4.2): POST /api/signin/start {email}
// shows the match code; GET /api/signin/status says pending, approved
// (the session is set) or ended. The page reads the same whether or not
// the address is a member's.

import { poll, pollMessages, post } from './api.js';
import { $ } from './dom.js';
import * as ui from './ui.js';

ui.init();

const steps = ['step-email', 'step-wait', 'step-end'].map($);
const form = $('email-form');
let stop = null;

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
    if (!st || st.status === 'ended') {
      show('step-end');
      return true;
    }
    return false;
  }, { ...pollMessages(ui), until: Number.isNaN(until.getTime()) ? null : until });
}

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  const email = form.elements.email.value.trim();
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
  else if (st?.status === 'approved') location.replace('/home/');
  else show('step-email');
  return true;
}, { ...pollMessages(ui), now: true });
