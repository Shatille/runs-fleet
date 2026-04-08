import { useEffect, useState, useCallback, useRef } from 'react';

export function useAutoRefresh(
  callback: () => void,
  intervalMs: number,
  storageKey: string,
  defaultEnabled: boolean = false,
) {
  const [enabled, setEnabled] = useState(() => {
    if (typeof window === 'undefined') return defaultEnabled;
    const stored = localStorage.getItem(storageKey);
    return stored !== null ? stored === 'true' : defaultEnabled;
  });
  const [isRefreshing, setIsRefreshing] = useState(false);
  const callbackRef = useRef(callback);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  callbackRef.current = callback;

  const toggle = useCallback(() => {
    setEnabled((prev) => {
      const next = !prev;
      localStorage.setItem(storageKey, String(next));
      return next;
    });
  }, [storageKey]);

  useEffect(() => {
    if (!enabled) {
      if (intervalRef.current) {
        clearInterval(intervalRef.current);
        intervalRef.current = null;
      }
      return;
    }

    const tick = () => {
      if (document.hidden) return;
      setIsRefreshing(true);
      callbackRef.current();
      setTimeout(() => setIsRefreshing(false), 500);
    };

    intervalRef.current = setInterval(tick, intervalMs);

    const handleVisibility = () => {
      if (!document.hidden && enabled) {
        callbackRef.current();
      }
    };
    document.addEventListener('visibilitychange', handleVisibility);

    return () => {
      if (intervalRef.current) clearInterval(intervalRef.current);
      document.removeEventListener('visibilitychange', handleVisibility);
    };
  }, [enabled, intervalMs]);

  return { enabled, toggle, isRefreshing };
}
