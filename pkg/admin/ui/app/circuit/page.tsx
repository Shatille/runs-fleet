'use client';

import { useEffect, useState, useCallback } from 'react';
import { StatsCardSkeleton, TableSkeleton } from '@/components/skeleton';
import { CircuitState } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { useAutoRefresh } from '@/hooks/use-auto-refresh';

export default function CircuitPage() {
  const [circuits, setCircuits] = useState<CircuitState[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchCircuits = useCallback(async () => {
    try {
      setLoading(true);
      const res = await apiFetch('/api/circuit');
      if (!res.ok) {
        throw new Error(`Failed to fetch circuit states: ${res.statusText}`);
      }
      const data = await res.json();
      setCircuits(data.circuits || []);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load circuit states');
    } finally {
      setLoading(false);
    }
  }, []);

  const { isRefreshing } = useAutoRefresh(fetchCircuits, 15000, 'runs-fleet-circuit-auto-refresh', true);

  useEffect(() => {
    fetchCircuits();
  }, [fetchCircuits]);

  const openCircuits = circuits.filter((c) => c.state === 'open');
  const halfOpenCircuits = circuits.filter((c) => c.state === 'half-open');
  const closedCircuits = circuits.filter((c) => c.state === 'closed');

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <button
          onClick={fetchCircuits}
          className="mt-2 text-red-600 dark:text-red-400 underline hover:no-underline"
        >
          Retry
        </button>
      </div>
    );
  }

  return (
    <div>
      <div className="flex justify-between items-center mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Circuit Breaker Status</h1>
        <button
          onClick={fetchCircuits}
          disabled={loading}
          className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-4 py-2 rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Refresh'}
        </button>
      </div>

      {loading && circuits.length === 0 ? (
        <>
          <StatsCardSkeleton count={3} />
          <TableSkeleton rows={4} cols={5} />
        </>
      ) : (
        <>
          <div className="grid grid-cols-1 md:grid-cols-3 gap-4 mb-6">
            <SummaryCard label="Open" count={openCircuits.length} color="red" />
            <SummaryCard label="Half-Open" count={halfOpenCircuits.length} color="yellow" />
            <SummaryCard label="Closed" count={closedCircuits.length} color="green" />
          </div>

          {circuits.length === 0 ? (
            <div className="text-center py-12 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
              <p className="text-gray-500 dark:text-gray-400">No circuit breaker states recorded.</p>
              <p className="text-sm text-gray-400 dark:text-gray-500 mt-2">
                Circuit breakers are created when spot interruptions occur.
              </p>
            </div>
          ) : (
            <div className="bg-white dark:bg-gray-800 shadow rounded-lg overflow-hidden">
              <table className="min-w-full divide-y divide-gray-200 dark:divide-gray-700">
                <thead className="bg-gray-50 dark:bg-gray-700">
                  <tr>
                    <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
                      Instance Type
                    </th>
                    <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
                      State
                    </th>
                    <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
                      Failure Count
                    </th>
                    <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
                      Last Failure
                    </th>
                    <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
                      Auto Reset
                    </th>
                  </tr>
                </thead>
                <tbody className="bg-white dark:bg-gray-800 divide-y divide-gray-200 dark:divide-gray-700">
                  {circuits.map((circuit) => (
                    <tr key={circuit.instance_type} className="hover:bg-gray-50 dark:hover:bg-gray-700">
                      <td className="px-6 py-4 whitespace-nowrap font-mono text-sm text-gray-900 dark:text-gray-100">
                        {circuit.instance_type}
                      </td>
                      <td className="px-6 py-4 whitespace-nowrap">
                        <CircuitStateBadge state={circuit.state} />
                      </td>
                      <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500 dark:text-gray-400">
                        {circuit.failure_count}
                      </td>
                      <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500 dark:text-gray-400">
                        {formatTime(circuit.last_failure)}
                      </td>
                      <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500 dark:text-gray-400">
                        {formatTime(circuit.reset_at)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
}

interface SummaryCardProps {
  label: string;
  count: number;
  color: 'red' | 'yellow' | 'green';
}

function SummaryCard({ label, count, color }: SummaryCardProps) {
  const colorClasses = {
    red: 'bg-red-50 dark:bg-red-900/30 text-red-700 dark:text-red-400 border-red-200 dark:border-red-800',
    yellow: 'bg-yellow-50 dark:bg-yellow-900/30 text-yellow-700 dark:text-yellow-400 border-yellow-200 dark:border-yellow-800',
    green: 'bg-green-50 dark:bg-green-900/30 text-green-700 dark:text-green-400 border-green-200 dark:border-green-800',
  };

  return (
    <div className={`rounded-lg p-4 border ${colorClasses[color]}`}>
      <div className="text-sm font-medium opacity-75">{label}</div>
      <div className="text-3xl font-bold">{count}</div>
    </div>
  );
}

interface CircuitStateBadgeProps {
  state: string;
}

function CircuitStateBadge({ state }: CircuitStateBadgeProps) {
  const stateStyles: Record<string, string> = {
    open: 'bg-red-100 dark:bg-red-900/50 text-red-800 dark:text-red-300',
    'half-open': 'bg-yellow-100 dark:bg-yellow-900/50 text-yellow-800 dark:text-yellow-300',
    closed: 'bg-green-100 dark:bg-green-900/50 text-green-800 dark:text-green-300',
  };

  const style = stateStyles[state.toLowerCase()] || 'bg-gray-100 dark:bg-gray-700 text-gray-800 dark:text-gray-300';

  return (
    <span className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${style}`}>
      {state}
    </span>
  );
}

function formatTime(isoString?: string): string {
  if (!isoString) return '-';
  const date = new Date(isoString);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  const diffMins = Math.floor(diffMs / 60000);

  if (diffMins < 0) {
    const futureMins = Math.abs(diffMins);
    if (futureMins < 60) return `in ${futureMins}m`;
    const futureHours = Math.floor(futureMins / 60);
    return `in ${futureHours}h`;
  }

  if (diffMins < 1) return 'just now';
  if (diffMins < 60) return `${diffMins}m ago`;

  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;

  return date.toLocaleDateString();
}
