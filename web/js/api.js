// Fetch wrapper for the member API (README "Member API"). Same-origin;
// the session and flow cookies are HttpOnly, so this code never sees
// them. A POST is JSON, which the API requires.

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

const MESSAGES = {
  bad_email: 'That doesn’t look like an email address.',
  invalid_code: 'That code didn’t work. Check the newest email and try again, or get a new code.',
  rate_limited: 'Too many attempts. Please wait a while and try again.',
  unavailable: 'The VettID service is busy right now. Please try again in a minute.',
  signed_out: 'You are not signed in.',
  network: 'We could not reach the member service. Check your connection and try again.',
};

function message(status, code) {
  if (MESSAGES[code]) return MESSAGES[code];
  if (status === 429) return MESSAGES.rate_limited;
  if (status >= 500) return 'Something went wrong on our side. Please try again in a few minutes.';
  return 'Something went wrong. Please try again.';
}

async function send(method, path, body) {
  const headers = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res;
  try {
    res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
      cache: 'no-store',
    });
  } catch {
    throw new ApiError(0, 'network', MESSAGES.network);
  }
  const text = await res.text().catch(() => '');
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (res.ok) return data ?? {};
  const code = typeof data?.error === 'string' ? data.error : `http_${res.status}`;
  throw new ApiError(res.status, code, message(res.status, code));
}

export const get = (path) => send('GET', path);
export const post = (path, body) => send('POST', path, body);

/** GET that answers null for a 404 or 401 (no flow, signed out). */
export async function getOrNull(path) {
  try {
    return await get(path);
  } catch (err) {
    if (err instanceof ApiError && (err.status === 404 || err.status === 401)) return null;
    throw err;
  }
}

/**
 * poll(path, onStatus, opts) asks for path until onStatus(status) returns
 * true (done); status is null for a 404 or 401. Every `every` ms while
 * the API answers; after an error it backs off (up to 30 s) and calls
 * onError, once per outage, and onRecover when an answer comes back. Past
 * `until` (a Date) and a minute's grace it stops and calls onGiveUp.
 * `now` asks at once instead of after the first interval. Returns stop().
 */
export function poll(path, onStatus, { every = 2000, now = false, until = null, onError, onRecover, onGiveUp } = {}) {
  let stopped = false;
  let timer = null;
  let failing = false;
  let wait = every;
  async function tick() {
    timer = null;
    if (stopped) return;
    if (until && Date.now() > until.getTime() + 60_000) {
      stopped = true;
      onGiveUp?.();
      return;
    }
    try {
      const st = await getOrNull(path);
      if (stopped) return;
      if (failing) {
        failing = false;
        onRecover?.();
      }
      wait = every;
      if (onStatus(st)) {
        stopped = true;
        return;
      }
    } catch (err) {
      if (stopped) return;
      if (!failing) onError?.(err);
      failing = true;
      wait = Math.min(wait * 2, 30_000);
    }
    if (!stopped) timer = setTimeout(tick, wait);
  }
  if (now) tick();
  else timer = setTimeout(tick, every);
  return () => {
    stopped = true;
    if (timer !== null) clearTimeout(timer);
  };
}

/** Errors shown while polling, cleared when the API answers again. */
export function pollMessages(ui) {
  return {
    onError: (err) => ui.showError(err),
    onRecover: () => ui.clearMessages(),
    onGiveUp: () => ui.showError('We still can’t reach the member service. Reload the page to try again.'),
  };
}
