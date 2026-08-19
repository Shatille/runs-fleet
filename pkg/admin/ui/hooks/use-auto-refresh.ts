import { useEffect, useState, useRef } from 'react';

export function useAutoRefresh(callback: () => void, intervalMs: number) {
  const [isRefreshing, setIsRefreshing] = useState(false);
  const callbackRef = useRef(callback);

  // Held in a ref so a caller passing a fresh closure each render does not tear
  // down and restart the interval.
  callbackRef.current = callback;

  useEffect(() => {
    const tick = () => {
      if (document.hidden) return;
      setIsRefreshing(true);
      callbackRef.current();
      setTimeout(() => setIsRefreshing(false), 500);
    };

    const interval = setInterval(tick, intervalMs);

    const handleVisibility = () => {
      if (!document.hidden) {
        callbackRef.current();
      }
    };
    document.addEventListener('visibilitychange', handleVisibility);

    return () => {
      clearInterval(interval);
      document.removeEventListener('visibilitychange', handleVisibility);
    };
  }, [intervalMs]);

  return { isRefreshing };
}
