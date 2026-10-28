// The site's pages against a scripted member API (fakedom.mjs): each
// step of connect, sign-in and home, as the README's routes answer.

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { api, clock, load, run } from './fakedom.mjs';

const web = new URL('../web/', import.meta.url).pathname;
const visible = (page, id) => !page.get(id).hidden;

test('connect: email, code, invitation, SAS, connected', async () => {
  const page = load(web + 'connect/index.html');
  const c = clock();
  const calls = api({
    'GET /api/connect/status': [
      [404, { error: 'no_flow' }],
      [200, { status: 'waiting', url: 'https://vettid.org/i/abc', expires_at: '2026-10-27T12:10:00Z' }],
      [200, { status: 'sas', sas: '4821', expires_at: '2026-10-27T12:10:00Z' }],
      [200, { status: 'connected' }],
    ],
    'POST /api/connect/start': [[202, { expires_at: '2026-10-27T12:10:00Z' }]],
    'POST /api/connect/verify': [[400, { error: 'invalid_code' }], [200, { url: 'https://vettid.org/i/abc', expires_at: '2026-10-27T12:10:00Z' }]],
  });
  await run('connect.js');
  assert.ok(visible(page, 'step-email'));

  page.get('email').value = ' ada@example.org ';
  await page.get('email-form').dispatch('submit');
  assert.deepEqual(calls.at(-1).body, { email: 'ada@example.org' });
  assert.equal(calls.at(-1).headers['Content-Type'], 'application/json');
  assert.ok(visible(page, 'step-code'));
  assert.equal(page.get('code-to').textContent, 'ada@example.org');
  assert.ok(page.get('code-email-field').hidden, 'the address is known: not asked again');

  page.get('code').value = '12345';
  await page.get('code-form').dispatch('submit');
  assert.equal(calls.at(-1).key, 'POST /api/connect/start', 'a short code is not sent');

  page.get('code').value = '123456';
  await page.get('code-form').dispatch('submit');
  assert.match(page.get('errors').textContent, /didn’t work/);
  assert.ok(visible(page, 'step-code'));

  await page.get('code-form').dispatch('submit');
  assert.deepEqual(calls.at(-1).body, { email: 'ada@example.org', code: '123456' });
  assert.ok(visible(page, 'step-invite'));
  assert.equal(page.get('invite-link').href, 'https://vettid.org/i/abc');
  assert.equal(page.get('invite-qr').children[0].tagName, 'SVG');

  await c.tick(); // waiting
  assert.ok(visible(page, 'step-invite'));
  await c.tick(); // sas
  assert.ok(visible(page, 'step-sas'));
  assert.equal(page.get('sas').textContent, '4821');
  assert.equal(c.pending(), 1, 'one poll');
  await c.tick(); // connected
  assert.equal(page.replaced, '/home/');
  assert.equal(c.pending(), 0, 'the poll stopped');
});

test('connect: a resume that cannot reach the API says so, retries, and picks up at the SAS', async () => {
  const page = load(web + 'connect/index.html');
  const c = clock();
  api({
    'GET /api/connect/status': [
      [0],
      [503, { error: 'unavailable' }],
      [200, { status: 'sas', sas: '4821', expires_at: '2026-10-27T12:10:00Z' }],
      [200, { status: 'declined' }],
    ],
  });
  await run('connect.js');
  assert.ok(page.get('step-email').hidden, 'not sent back to step 1');
  assert.match(page.get('errors').textContent, /could not reach/);
  await c.tick(); // 503: the same outage, reported once
  assert.match(page.get('errors').textContent, /could not reach/);
  await c.tick(); // sas
  assert.ok(visible(page, 'step-sas'));
  assert.equal(page.get('errors').textContent, '', 'cleared once the API answers');
  assert.equal(c.pending(), 1);
  await c.tick(); // declined, by poll
  assert.ok(visible(page, 'step-end'));
  assert.match(page.get('end-title').textContent, /declined/);
  assert.equal(c.pending(), 0);
});

test('connect: polling gives up after the invitation, and a restart stops it', async () => {
  let page = load(web + 'connect/index.html');
  let c = clock();
  api({ 'GET /api/connect/status': [[200, { status: 'waiting', url: 'https://vettid.org/i/abc', expires_at: '2026-10-27T12:10:00Z' }], [0]] });
  await run('connect.js');
  assert.ok(visible(page, 'step-invite'));
  await c.tick();
  c.advance(12 * 60_000);
  await c.tick();
  assert.match(page.get('errors').textContent, /Reload the page/);
  assert.equal(c.pending(), 0);

  page = load(web + 'connect/index.html');
  c = clock();
  api({
    'GET /api/connect/status': [[404, { error: 'no_flow' }], [200, { status: 'code' }]],
    'POST /api/connect/start': [[202, { expires_at: '2026-10-27T12:10:00Z' }]],
  });
  await run('connect.js');
  page.get('email').value = 'ada@example.org';
  await page.get('email-form').dispatch('submit');
  await page.get('restart').dispatch('click');
  assert.ok(visible(page, 'step-email'));
  assert.equal(page.get('email').value, 'ada@example.org', 'the address is kept for a new code');
  assert.equal(c.pending(), 0);
});

test('connect: a reload at the code step asks for the address again', async () => {
  const page = load(web + 'connect/index.html');
  clock();
  const calls = api({
    'GET /api/connect/status': [[200, { status: 'code', expires_at: '2026-10-27T12:10:00Z' }]],
    'POST /api/connect/verify': [[200, { url: 'https://vettid.org/i/abc', expires_at: '2026-10-27T12:10:00Z' }]],
  });
  await run('connect.js');
  assert.ok(visible(page, 'step-code'));
  assert.ok(!page.get('code-email-field').hidden);
  page.get('code-email').value = 'ada@example.org';
  page.get('code').value = '000123';
  await page.get('code-form').dispatch('submit');
  assert.deepEqual(calls.at(-1).body, { email: 'ada@example.org', code: '000123' });
});

test('connect: declined and expired end the flow', async () => {
  for (const [status, title] of [['declined', /declined/], ['expired', /ended/]]) {
    const page = load(web + 'connect/index.html');
    clock();
    api({ 'GET /api/connect/status': [[200, { status }]] });
    await run('connect.js');
    assert.ok(visible(page, 'step-end'));
    assert.match(page.get('end-title').textContent, title);
  }
});

test('sign-in: match code, then approved', async () => {
  const page = load(web + 'signin/index.html');
  const c = clock();
  const calls = api({
    'GET /api/signin/status': [[404, { error: 'no_flow' }], [200, { status: 'pending', match_code: '42' }], [200, { status: 'approved' }]],
    'POST /api/signin/start': [[202, { match_code: '42', expires_at: '2026-10-27T12:06:00Z' }]],
  });
  await run('signin.js');
  assert.ok(visible(page, 'step-email'));
  page.get('email').value = 'ada@example.org';
  await page.get('email-form').dispatch('submit');
  assert.deepEqual(calls.at(-1).body, { email: 'ada@example.org' });
  assert.ok(visible(page, 'step-wait'));
  assert.equal(page.get('match').textContent, '42');
  await c.tick();
  assert.equal(page.replaced, null);
  await c.tick();
  assert.equal(page.replaced, '/home/');
});

test('sign-in: ended reads the same however it ended; a reload resumes', async () => {
  let page = load(web + 'signin/index.html');
  const c = clock();
  api({ 'GET /api/signin/status': [[200, { status: 'pending', match_code: '17' }], [200, { status: 'ended' }]] });
  await run('signin.js');
  assert.ok(visible(page, 'step-wait'));
  assert.equal(page.get('match').textContent, '17');
  await c.tick();
  assert.ok(visible(page, 'step-end'));

  page = load(web + 'signin/index.html');
  const c2 = clock();
  api({ 'GET /api/signin/status': [[0], [200, { status: 'pending', match_code: '17', expires_at: '2026-10-27T12:06:00Z' }]] });
  await run('signin.js');
  assert.ok(page.get('step-email').hidden);
  assert.match(page.get('errors').textContent, /could not reach/);
  await c2.tick();
  assert.ok(visible(page, 'step-wait'));
  assert.equal(page.get('errors').textContent, '');
  await page.get('cancel').dispatch('click');
  assert.equal(c2.pending(), 0, 'starting over stops the poll');

  page = load(web + 'signin/index.html');
  clock();
  api({ 'GET /api/signin/status': [[404, { error: 'no_flow' }]], 'POST /api/signin/start': [[429, { error: 'rate_limited' }]] });
  await run('signin.js');
  page.get('email').value = 'ada@example.org';
  await page.get('email-form').dispatch('submit');
  assert.ok(visible(page, 'step-email'));
  assert.match(page.get('errors').textContent, /Too many attempts/);
});

test('sign-in: a changed key confirms with an emailed code', async () => {
  let page = load(web + 'signin/index.html');
  const c = clock();
  const calls = api({
    'GET /api/signin/status': [[404, { error: 'no_flow' }], [200, { status: 'confirm', code_sent: false, expires_at: '2026-10-27T12:10:00Z' }]],
    'POST /api/signin/start': [[202, { match_code: '42', expires_at: '2026-10-27T12:06:00Z' }]],
    'POST /api/signin/confirm': [[202, { expires_at: '2026-10-27T12:10:00Z' }]],
    'POST /api/signin/verify': [[400, { error: 'invalid_code' }], [200, { status: 'approved' }]],
  });
  await run('signin.js');
  page.get('email').value = 'ada@example.org';
  await page.get('email-form').dispatch('submit');
  await c.tick();
  assert.ok(visible(page, 'step-confirm'));
  assert.deepEqual(calls.find((x) => x.key === 'POST /api/signin/confirm').body, { email: 'ada@example.org' }, 'the code is sent at once');
  assert.ok(visible(page, 'confirm-code-form'));
  assert.equal(page.get('confirm-to').textContent, 'ada@example.org');
  assert.equal(page.replaced, null, 'no session yet');
  page.get('confirm-code').value = '123456';
  await page.get('confirm-code-form').dispatch('submit');
  assert.match(page.get('errors').textContent, /didn’t work/);
  await page.get('confirm-code-form').dispatch('submit');
  assert.deepEqual(calls.at(-1).body, { email: 'ada@example.org', code: '123456' });
  assert.equal(page.replaced, '/home/');
  assert.equal(c.pending(), 0);

  // After a reload the page asks for the address first.
  page = load(web + 'signin/index.html');
  clock();
  const calls2 = api({
    'GET /api/signin/status': [[200, { status: 'confirm', code_sent: false, expires_at: '2026-10-27T12:10:00Z' }]],
    'POST /api/signin/confirm': [[400, { error: 'wrong_email' }], [202, { expires_at: '2026-10-27T12:10:00Z' }]],
  });
  await run('signin.js');
  assert.ok(visible(page, 'step-confirm'));
  assert.ok(visible(page, 'confirm-email-form'));
  assert.ok(page.get('confirm-code-form').hidden);
  page.get('confirm-email').value = 'bob@example.org';
  await page.get('confirm-email-form').dispatch('submit');
  assert.match(page.get('errors').textContent, /isn’t the address/);
  assert.ok(visible(page, 'confirm-email-form'));
  page.get('confirm-email').value = 'ada@example.org';
  await page.get('confirm-email-form').dispatch('submit');
  assert.ok(visible(page, 'confirm-code-form'));
  assert.equal(calls2.at(-1).body.email, 'ada@example.org');
});

test('sign-in: a reload after the code was sent asks for the address beside it, and a confirmation that runs out ends', async () => {
  const page = load(web + 'signin/index.html');
  const c = clock();
  const calls = api({
    'GET /api/signin/status': [
      [200, { status: 'confirm', code_sent: true, expires_at: '2026-10-27T12:10:00Z' }],
      [200, { status: 'confirm', code_sent: true, expires_at: '2026-10-27T12:10:00Z' }],
      [404, { error: 'no_flow' }],
    ],
    'POST /api/signin/verify': [[400, { error: 'invalid_code' }]],
  });
  await run('signin.js');
  assert.ok(visible(page, 'confirm-code-form'));
  assert.ok(visible(page, 'confirm-code-email-field'));
  assert.ok(!calls.some((x) => x.key === 'POST /api/signin/confirm'), 'no new code that would void the one sent');
  page.get('confirm-code-email').value = 'ada@example.org';
  page.get('confirm-code').value = '654321';
  await page.get('confirm-code-form').dispatch('submit');
  assert.deepEqual(calls.at(-1).body, { email: 'ada@example.org', code: '654321' });
  await c.tick(); // still confirm
  assert.ok(visible(page, 'step-confirm'));
  await c.tick(); // gone
  assert.ok(visible(page, 'step-end'));
  assert.equal(c.pending(), 0);
});

test('home: a member sees the count, an admin the names', async () => {
  let page = load(web + 'home/index.html');
  clock();
  api({
    'GET /api/me': [[200, { connection_id: 'c1', name: 'Ada', joined: '2026-10-27T12:00:00Z', admin: false }]],
    'GET /api/members': [[200, { count: 2 }]],
  });
  await run('home.js');
  assert.equal(page.get('hello').textContent, 'Welcome, Ada');
  assert.equal(page.get('member-count').textContent, '2 members');
  assert.ok(page.get('roster').hidden);

  page = load(web + 'home/index.html');
  clock();
  const calls = api({
    'GET /api/me': [[200, { connection_id: 'c1', name: 'Ada', joined: '2026-10-27T12:00:00Z', admin: true }]],
    'GET /api/members': [[200, { count: 2, members: [{ name: 'Bob', joined: '2026-10-26T12:00:00Z' }, { name: '', joined: '2026-10-27T12:00:00Z' }, { name: 'Cy', joined: '2026-10-27T13:00:00Z', suspended: true }] }]],
    'POST /api/signout': [[204]],
  });
  await run('home.js');
  assert.match(page.get('joined').textContent, /admin/);
  const roster = page.get('roster');
  assert.ok(!roster.hidden);
  assert.equal(roster.children.length, 3);
  assert.match(roster.children[0].textContent, /^Bob/);
  assert.doesNotMatch(roster.children[0].textContent, /suspended/);
  assert.match(roster.children[1].textContent, /Name not shared yet/);
  assert.match(roster.children[2].textContent, /^Cy.*suspended, must connect again/);

  await page.get('signout').dispatch('click');
  assert.equal(calls.at(-1).key, 'POST /api/signout');
  assert.equal(page.replaced, '/?signedout=1');
});

test('home: signed out goes to sign-in, also when the session ends between calls', async () => {
  let page = load(web + 'home/index.html');
  clock();
  api({ 'GET /api/me': [[401, { error: 'signed_out' }]] });
  await run('home.js');
  assert.equal(page.replaced, '/signin/');

  page = load(web + 'home/index.html');
  clock();
  api({ 'GET /api/me': [[200, { connection_id: 'c1', name: 'Ada', joined: '2026-10-27T12:00:00Z' }]], 'GET /api/members': [[401, { error: 'signed_out' }]] });
  await run('home.js');
  assert.equal(page.replaced, '/signin/');
  assert.equal(page.get('errors').textContent, '');
});

test('landing: a member goes home', async () => {
  const page = load(web + 'index.html');
  clock();
  api({ 'GET /api/me': [[200, { connection_id: 'c1', name: 'Ada', joined: '2026-10-27T12:00:00Z' }]] });
  await run('index.js');
  assert.equal(page.replaced, '/home/');
});
