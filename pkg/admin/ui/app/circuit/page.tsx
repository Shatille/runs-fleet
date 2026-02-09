'use client';

import { useEffect, useState, useCallback } from 'react';
import { CircuitState } from '@/lib/types';
import { apiFetch } from '@/lib/api';

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

  useEffect(() => {
    fetchCircuits();
    const interval = setInterval(fetchCircuits, 15000);
    return () => clearInterval(interval);
  }, [fetchCircuits]);

  const openCircuits = circuits.filter((c) => c.state === 'open');
  const halfOpenCircuits = circuits.filter((c) => c.state === 'half-open');
  const closedCircuits = circuits.filter((c) => c.state === 'closed');

  if (error) {
    return (
      <div className="bg-red-50 border border-red-200 rounded-md p-4">
        <p className="text-red-800">{error}</p>
        <button
          onClick={fetchCircuits}
          className="mt-2 text-red-600 underline hover:no-underline"
        >
          Retry
        </button>
      </div>
    );
  }

  return (
    <div>
      <div className="flex justify-between items-center mb-6">
        <h1 className="text-2xl font-bold text-gray-900">Circuit Breaker Status</h1>
        <button
          onClick={fetchCircuits}
          disabled={loading}
          className="bg-gray-100 text-gray-700 px-4 py-2 rounded-md hover:bg-gray-200 transition-colors disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Refresh'}
        </button>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-4 mb-6">
        <SummaryCard label="Open" count={openCircuits.length} color="red" />
        <SummaryCard label="Half-Open" count={halfOpenCircuits.length} color="yellow" />
        <SummaryCard label="Closed" count={closedCircuits.length} color="green" />
      </div>

      {loading && circuits.length === 0 ? (
        <div className="flex items-center justify-center h-64">
          <div className="text-gray-500">Loading circuit states...</div>
        </div>
      ) : circuits.length === 0 ? (
        <div className="text-center py-12 bg-white rounded-lg border">
          <p className="text-gray-500">No circuit breaker states recorded.</p>
          <p className="text-sm text-gray-400 mt-2">
            Circuit breakers are created when spot interruptions occur.
          </p>
        </div>
      ) : (
        <div className="bg-white shadow rounded-lg overflow-hidden">
          <table className="min-w-full divide-y divide-gray-200">
            <thead className="bg-gray-50">
              <tr>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Instance Type
                </th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  State
                </th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Failure Count
                </th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Last Failure
                </th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Auto Reset
                </th>
              </tr>
            </thead>
            <tbody className="bg-white divide-y divide-gray-200">
              {circuits.map((circuit) => (
                <tr key={circuit.instance_type} className="hover:bg-gray-50">
                  <td className="px-6 py-4 whitespace-nowrap font-mono text-sm text-gray-900">
                    {circuit.instance_type}
                  </td>
                  <td className="px-6 py-4 whitespace-nowrap">
                    <CircuitStateBadge state={circuit.state} />
                  </td>
                  <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                    {circuit.failure_count}
                  </td>
                  <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                    {formatTime(circuit.last_failure)}
                  </td>
                  <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                    {formatTime(circuit.reset_at)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
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
    red: 'bg-red-50 text-red-700 border-red-200',
    yellow: 'bg-yellow-50 text-yellow-700 border-yellow-200',
    green: 'bg-green-50 text-green-700 border-green-200',
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
    open: 'bg-red-100 text-red-800',
    'half-open': 'bg-yellow-100 text-yellow-800',
    closed: 'bg-green-100 text-green-800',
  };

  const style = stateStyles[state.toLowerCase()] || 'bg-gray-100 text-gray-800';

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
