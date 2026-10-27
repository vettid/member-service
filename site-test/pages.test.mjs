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
  await c.tick(); // connected
  assert.equal(page.replaced, '/home/');
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
  clock();
  api({ 'GET /api/signin/status': [[404, { error: 'no_flow' }]], 'POST /api/signin/start': [[429, { error: 'rate_limited' }]] });
  await run('signin.js');
  page.get('email').value = 'ada@example.org';
  await page.get('email-form').dispatch('submit');
  assert.ok(visible(page, 'step-email'));
  assert.match(page.get('errors').textContent, /Too many attempts/);
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
    'GET /api/members': [[200, { count: 2, members: [{ name: 'Bob', joined: '2026-10-26T12:00:00Z' }, { name: '', joined: '2026-10-27T12:00:00Z' }] }]],
    'POST /api/signout': [[204]],
  });
  await run('home.js');
  assert.match(page.get('joined').textContent, /admin/);
  const roster = page.get('roster');
  assert.ok(!roster.hidden);
  assert.equal(roster.children.length, 2);
  assert.match(roster.children[0].textContent, /^Bob/);
  assert.match(roster.children[1].textContent, /Name not shared yet/);

  await page.get('signout').dispatch('click');
  assert.equal(calls.at(-1).key, 'POST /api/signout');
  assert.equal(page.replaced, '/?signedout=1');
});

test('home: signed out goes to sign-in', async () => {
  const page = load(web + 'home/index.html');
  clock();
  api({ 'GET /api/me': [[401, { error: 'signed_out' }]] });
  await run('home.js');
  assert.equal(page.replaced, '/signin/');
});

test('landing: a member goes home', async () => {
  const page = load(web + 'index.html');
  clock();
  api({ 'GET /api/me': [[200, { connection_id: 'c1', name: 'Ada', joined: '2026-10-27T12:00:00Z' }]] });
  await run('index.js');
  assert.equal(page.replaced, '/home/');
});
