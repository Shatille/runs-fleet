// Thrown when a request outlives its timeout. It is distinct from a failure
// response because the server may well have finished the work: a sweep that
// outruns the browser is still committing writes after the fetch is abandoned,
// and reporting that as "failed" sends operators to re-run work already done.
export class RequestTimeoutError extends Error {
  constructor(public readonly timeoutMs: number) {
    super(
      `The server did not answer within ${Math.round(timeoutMs / 1000)}s. It may still be working — ` +
        'refresh before retrying, or the same work may run twice.'
    );
    this.name = 'RequestTimeoutError';
  }
}

export async function apiFetch(
  url: string,
  options: RequestInit = {},
  timeoutMs = 10000
): Promise<Response> {
  const controller = new AbortController();
  let timedOut = false;
  const timeoutId = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, timeoutMs);

  try {
    const res = await fetch(url, {
      ...options,
      credentials: 'include',
      signal: controller.signal,
    });

    if (res.status === 401) {
      window.dispatchEvent(new CustomEvent('auth-required'));
    }

    return res;
  } catch (err) {
    if (timedOut) {
      throw new RequestTimeoutError(timeoutMs);
    }
    throw err;
  } finally {
    clearTimeout(timeoutId);
  }
}
