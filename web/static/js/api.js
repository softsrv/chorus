// Minimal client for the Chorus JSON API.
//
// The access token lives only in memory. The refresh token is kept in
// sessionStorage so a page reload stays signed in for the life of the tab.
// That's acceptable for a test client under a strict CSP; native apps should
// keep the refresh token in the Keychain / Android Keystore instead.

const REFRESH_KEY = 'chorus.refresh_token';
const DEVICE_NAME = 'Web test client';

let accessToken = null;
let refreshInFlight = null;
let onLog = () => {};

export const session = { user: null };

export class ApiError extends Error {
  constructor(status, body) {
    super(body?.message || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code;
    this.fields = body?.fields || {};
  }
}

export function setLogger(fn) { onLog = fn; }

export function setTokens(t) {
  accessToken = t.access_token;
  session.user = t.user;
  try { sessionStorage.setItem(REFRESH_KEY, t.refresh_token); } catch { /* storage disabled */ }
}

export function clearTokens() {
  accessToken = null;
  session.user = null;
  try { sessionStorage.removeItem(REFRESH_KEY); } catch { /* storage disabled */ }
}

function storedRefreshToken() {
  try { return sessionStorage.getItem(REFRESH_KEY); } catch { return null; }
}

// refresh exchanges the stored refresh token for a new access token. Concurrent
// callers share one request. Resolves true on success.
export function refresh() {
  if (refreshInFlight) return refreshInFlight;
  const rt = storedRefreshToken();
  if (!rt) return Promise.resolve(false);
  refreshInFlight = (async () => {
    try {
      const t = await request('POST', '/auth/refresh', { refresh_token: rt, device_name: DEVICE_NAME }, { auth: false });
      setTokens(t);
      return true;
    } catch {
      clearTokens();
      return false;
    } finally {
      refreshInFlight = null;
    }
  })();
  return refreshInFlight;
}

// api calls an endpoint under /api/v1. On a token_expired 401 it refreshes
// once and retries. A 401 that can't be fixed by refreshing (revoked session,
// deleted account) clears the tokens and fires "chorus:signed-out".
export async function api(method, path, body) {
  try {
    return await request(method, path, body, { auth: true });
  } catch (err) {
    if (!(err instanceof ApiError) || err.status !== 401) throw err;
    if (err.code === 'token_expired' && await refresh()) {
      try {
        return await request(method, path, body, { auth: true });
      } catch (retryErr) {
        if (!(retryErr instanceof ApiError) || retryErr.status !== 401) throw retryErr;
      }
    }
    clearTokens();
    window.dispatchEvent(new Event('chorus:signed-out'));
    throw err;
  }
}

export { DEVICE_NAME };

export async function publicApi(method, path, body) {
  return request(method, path, body, { auth: false });
}

export async function authConfig() {
  return publicApi('GET', '/auth/config');
}

export async function googleRedeem(code, deviceName = DEVICE_NAME) {
  const t = await publicApi('POST', '/auth/google/redeem', { code, device_name: deviceName });
  setTokens(t);
  return t;
}

async function request(method, path, body, { auth }) {
  const headers = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (auth && accessToken) headers.Authorization = `Bearer ${accessToken}`;

  const started = performance.now();
  let res;
  try {
    res = await fetch(`/api/v1${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (err) {
    onLog({ method, path, status: 0, ms: 0, request: body, response: String(err) });
    throw new ApiError(0, { message: 'Network error — is the server running?' });
  }
  const data = res.status === 204 ? null : await res.json().catch(() => null);
  onLog({ method, path, status: res.status, ms: Math.round(performance.now() - started), request: body, response: data });

  if (!res.ok) throw new ApiError(res.status, data?.error);
  return data;
}
