export async function apiFetch(
  url: string,
  options: RequestInit = {},
  timeoutMs = 10000
): Promise<Response> {
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), timeoutMs);

  try {
    const res = await fetch(url, {
      ...options,
      signal: controller.signal,
    });

    if (res.status === 401) {
      window.dispatchEvent(new CustomEvent('auth-required'));
    }

    return res;
  } finally {
    clearTimeout(timeoutId);
  }
}
