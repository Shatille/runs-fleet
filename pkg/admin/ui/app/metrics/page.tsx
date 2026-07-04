'use client';

import { useEffect, useState, useCallback } from 'react';
import { StatsCardSkeleton } from '@/components/skeleton';
import { MetricsSummary } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { useAutoRefresh } from '@/hooks/use-auto-refresh';
import { formatDuration } from '@/lib/format';

export default function MetricsPage() {
  const [metrics, setMetrics] = useState<MetricsSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchMetrics = useCallback(async () => {
    try {
      setLoading(true);
      const res = await apiFetch('/api/metrics/summary');
      if (!res.ok) {
        throw new Error(`Failed to fetch metrics: ${res.statusText}`);
      }
      setMetrics(await res.json());
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load metrics');
    } finally {
      setLoading(false);
    }
  }, []);

  const { enabled, toggle, isRefreshing } = useAutoRefresh(fetchMetrics, 15000, 'runs-fleet-metrics-auto-refresh');

  useEffect(() => {
    fetchMetrics();
  }, [fetchMetrics]);

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <button onClick={fetchMetrics} className="mt-2 text-red-600 dark:text-red-400 underline hover:no-underline">
          Retry
        </button>
      </div>
    );
  }

  return (
    <div>
      <div className="flex justify-between items-center mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Metrics</h1>
        <label className="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-400">
          <input type="checkbox" checked={enabled} onChange={toggle} className="rounded" />
          Auto-refresh{isRefreshing ? ' …' : ''}
        </label>
      </div>

      {loading && !metrics ? (
        <StatsCardSkeleton count={6} />
      ) : metrics ? (
        <>
          <h2 className="text-sm font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider mb-2">Last 24 hours</h2>
          <div className="grid grid-cols-2 md:grid-cols-4 gap-4 mb-6">
            <MetricCard label="Jobs" value={String(metrics.jobs_24h.total)} />
            <MetricCard label="Completed" value={String(metrics.jobs_24h.completed)} />
            <MetricCard label="Failed" value={String(metrics.jobs_24h.failed)} accent={metrics.jobs_24h.failed > 0 ? 'red' : undefined} />
            <MetricCard label="In Progress" value={String(metrics.jobs_24h.in_progress)} />
          </div>

          <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
            <MetricCard label="Warm Pool Hit Rate" value={`${(metrics.warm_pool_hit_rate * 100).toFixed(0)}%`} />
            <MetricCard label="Avg Startup" value={metrics.avg_startup_time_seconds > 0 ? formatDuration(Math.round(metrics.avg_startup_time_seconds)) : '-'} />
            <MetricCard
              label="Spot Interruptions"
              value={`${(metrics.spot_interruption_rate * 100).toFixed(1)}%`}
              subtitle={metrics.spot_interruption_rate_estimated ? 'estimate' : undefined}
            />
            <MetricCard label="Cost (MTD)" value={metrics.cost_mtd_usd != null ? `$${metrics.cost_mtd_usd.toFixed(2)}` : '—'} subtitle={metrics.cost_mtd_usd == null ? 'unavailable' : undefined} />
          </div>

          {metrics.spot_interruption_rate_estimated && (
            <div className="mt-6 bg-yellow-50 dark:bg-yellow-900/30 border border-yellow-200 dark:border-yellow-800 rounded-md p-4 text-sm text-yellow-800 dark:text-yellow-300">
              Spot interruption rate is a best-effort estimate derived from retried/requeued spot jobs — it
              over-counts by including bootstrap-failure and stale-claim retries. An exact figure requires
              stored interruption history.
            </div>
          )}
        </>
      ) : null}
    </div>
  );
}

function MetricCard({ label, value, subtitle, accent }: { label: string; value: string; subtitle?: string; accent?: 'red' }) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 p-4">
      <div className="text-sm font-medium text-gray-500 dark:text-gray-400">{label}</div>
      <div className={`mt-1 text-2xl font-semibold ${accent === 'red' ? 'text-red-600 dark:text-red-400' : 'text-gray-900 dark:text-gray-100'}`}>
        {value}
      </div>
      {subtitle && <div className="mt-1 text-xs text-gray-400 dark:text-gray-500">{subtitle}</div>}
    </div>
  );
}
