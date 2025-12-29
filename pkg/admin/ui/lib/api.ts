const AUTH_KEY = 'runs_fleet_admin_token';

export function getAuthToken(): string | null {
  if (typeof window === 'undefined') return null;
  return sessionStorage.getItem(AUTH_KEY);
}

export function setAuthToken(token: string): void {
  sessionStorage.setItem(AUTH_KEY, token);
}

export function clearAuthToken(): void {
  sessionStorage.removeItem(AUTH_KEY);
}

export async function apiFetch(
  url: string,
  options: RequestInit = {},
  timeoutMs = 10000
): Promise<Response> {
  const token = getAuthToken();
  const headers = new Headers(options.headers);

  if (token) {
    headers.set('Authorization', `Bearer ${token}`);
  }

  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), timeoutMs);

  try {
    const res = await fetch(url, {
      ...options,
      headers,
      signal: controller.signal,
    });

    if (res.status === 401) {
      clearAuthToken();
      window.dispatchEvent(new CustomEvent('auth-required'));
    }

    return res;
  } finally {
    clearTimeout(timeoutId);
  }
}
