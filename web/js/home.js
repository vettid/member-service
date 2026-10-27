// Member home: who is signed in, the roster (its size for every member,
// names for admins: plan §6), sign out.

import { get, getOrNull, post } from './api.js';
import { $, date, el, fill } from './dom.js';
import * as ui from './ui.js';

ui.init();

async function load() {
  const me = await getOrNull('/api/me');
  if (!me) {
    location.replace('/signin/');
    return;
  }
  $('hello').textContent = me.name ? `Welcome, ${me.name}` : 'Welcome';
  fill($('joined'), 'Member since ', date(me.joined), me.admin ? ' · admin' : '');

  const r = await get('/api/members');
  $('member-count').textContent = `${r.count} ${r.count === 1 ? 'member' : 'members'}`;
  if (Array.isArray(r.members)) {
    const list = $('roster');
    fill(list, r.members.map((m) =>
      el('li', {}, el('span', {}, m.name || 'Name not shared yet'), el('span', { class: 'muted' }, 'joined ', date(m.joined)))));
    list.hidden = false;
  }
}

load().catch((err) => ui.showError(err));

$('signout').addEventListener('click', async () => {
  const done = await ui.busy($('signout'), () => post('/api/signout'), { busyLabel: 'Signing out…' });
  if (done !== undefined) location.replace('/?signedout=1');
});
