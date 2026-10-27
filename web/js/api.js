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
  invalid_code: 'That code didn’t work. Check the newest email and try again, or ask for a new code.',
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
 * poll(path, onStatus) asks every `every` ms until onStatus returns true
 * (done). Network errors and 5xx keep polling; the page shows them once.
 * Returns a stop function.
 */
export function poll(path, onStatus, onError, every = 2000) {
  let stopped = false;
  let timer = null;
  async function tick() {
    if (stopped) return;
    try {
      const st = await getOrNull(path);
      if (stopped) return;
      if (onStatus(st)) {
        stopped = true;
        return;
      }
    } catch (err) {
      if (stopped) return;
      onError?.(err);
    }
    timer = setTimeout(tick, every);
  }
  timer = setTimeout(tick, every);
  return () => {
    stopped = true;
    clearTimeout(timer);
  };
}
