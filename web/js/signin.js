// Sign in (MEMBER-SERVICE-PLAN §4.2): POST /api/signin/start {email}
// shows the match code; GET /api/signin/status says pending, approved
// (the session is set) or ended. The page reads the same whether or not
// the address is a member's.

import { getOrNull, poll, post } from './api.js';
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

function wait(matchCode) {
  $('match').textContent = matchCode;
  show('step-wait');
  stop?.();
  let reported = false;
  stop = poll('/api/signin/status', (st) => {
    if (!st) {
      show('step-end');
      return true;
    }
    if (st.status === 'approved') {
      location.replace('/home/');
      return true;
    }
    if (st.status === 'ended') {
      show('step-end');
      return true;
    }
    return false;
  }, (err) => {
    if (!reported) ui.showError(err);
    reported = true;
  });
}

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  const email = form.elements.email.value.trim();
  if (!email) return;
  const res = await ui.busy(form, () => post('/api/signin/start', { email }), { busyLabel: 'Starting…' });
  if (res) wait(res.match_code);
});

function restart() {
  stop?.();
  ui.clearMessages();
  show('step-email');
  form.elements.email.focus();
}
$('cancel').addEventListener('click', restart);
$('again').addEventListener('click', restart);

// Back on the page with a sign-in still open? Pick it up.
getOrNull('/api/signin/status').then(
  (st) => {
    if (st?.status === 'pending') wait(st.match_code);
    else if (st?.status === 'approved') location.replace('/home/');
    else show('step-email');
  },
  () => show('step-email'),
);
