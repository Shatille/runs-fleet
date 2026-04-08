'use client';

import { useEffect, useState, useCallback } from 'react';
import { CostSummary } from '@/lib/types';
import { apiFetch } from '@/lib/api';

export default function CostPage() {
  const [summary, setSummary] = useState<CostSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchCostSummary = useCallback(async () => {
    try {
      setLoading(true);
      const res = await apiFetch('/api/cost/summary');
      if (!res.ok) {
        throw new Error(`Failed to fetch cost summary: ${res.statusText}`);
      }
      const data = await res.json();
      setSummary(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load cost data');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchCostSummary();
  }, [fetchCostSummary]);

  const handleRefresh = () => {
    setError(null);
    fetchCostSummary();
  };

  if (error) {
    return (
      <div className="bg-red-50 border border-red-200 rounded-md p-4">
        <p className="text-red-800">{error}</p>
        <button
          onClick={handleRefresh}
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
        <h1 className="text-2xl font-bold text-gray-900">Cost</h1>
        <button
          onClick={handleRefresh}
          disabled={loading}
          className="bg-gray-100 text-gray-700 px-4 py-2 rounded-md hover:bg-gray-200 transition-colors disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Refresh'}
        </button>
      </div>

      {loading && !summary ? (
        <div className="flex items-center justify-center h-64">
          <div className="text-gray-500">Loading cost data...</div>
        </div>
      ) : summary ? (
        <>
          <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
            <SummaryCard
              title="Total Cost"
              value={`$${summary.total_cost.toFixed(2)}`}
              subtitle="Current month estimate"
            />
            <SummaryCard
              title="Avg Cost / Job"
              value={summary.job_count > 0 ? `$${summary.avg_cost_per_job.toFixed(4)}` : '-'}
              subtitle={`${summary.job_count} jobs this month`}
            />
            <SummaryCard
              title="Spot Savings"
              value={`$${summary.spot_savings.toFixed(2)}`}
              subtitle={`${summary.spot_job_count} spot / ${summary.on_demand_count} on-demand`}
            />
            <SummaryCard
              title="Job Count"
              value={String(summary.job_count)}
              subtitle={`${formatPeriod(summary.period_start, summary.period_end)}`}
            />
          </div>

          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 mb-6">
            <div className="bg-white rounded-lg border p-4">
              <h3 className="text-sm font-medium text-gray-900 mb-3">Spot vs On-Demand</h3>
              <div className="space-y-2">
                <div className="flex justify-between text-sm">
                  <span className="text-gray-600">Spot</span>
                  <span className="font-medium">${summary.spot_cost.toFixed(2)}</span>
                </div>
                <div className="w-full bg-gray-200 rounded-full h-2">
                  <div
                    className="bg-green-500 h-2 rounded-full"
                    style={{ width: `${summary.total_cost > 0 ? (summary.spot_cost / summary.total_cost) * 100 : 0}%` }}
                  />
                </div>
                <div className="flex justify-between text-sm">
                  <span className="text-gray-600">On-Demand</span>
                  <span className="font-medium">${summary.on_demand_cost.toFixed(2)}</span>
                </div>
                <div className="w-full bg-gray-200 rounded-full h-2">
                  <div
                    className="bg-blue-500 h-2 rounded-full"
                    style={{ width: `${summary.total_cost > 0 ? (summary.on_demand_cost / summary.total_cost) * 100 : 0}%` }}
                  />
                </div>
              </div>
            </div>
          </div>

          {summary.family_breakdown.length > 0 && (
            <div className="bg-white rounded-lg border overflow-hidden mb-6">
              <div className="px-4 py-3 border-b">
                <h3 className="text-sm font-medium text-gray-900">Breakdown by Instance Family</h3>
              </div>
              <table className="min-w-full divide-y divide-gray-200">
                <thead className="bg-gray-50">
                  <tr>
                    <th className="px-4 py-2 text-left text-xs font-medium text-gray-500 uppercase">Family</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 uppercase">Jobs</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 uppercase">Hours</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 uppercase">Cost</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 uppercase">Spot %</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-200">
                  {summary.family_breakdown
                    .sort((a, b) => b.total_cost - a.total_cost)
                    .map((entry) => (
                      <tr key={entry.family}>
                        <td className="px-4 py-2 text-sm font-medium text-gray-900">{entry.family}</td>
                        <td className="px-4 py-2 text-sm text-gray-600 text-right">{entry.job_count}</td>
                        <td className="px-4 py-2 text-sm text-gray-600 text-right">{entry.total_hours.toFixed(1)}</td>
                        <td className="px-4 py-2 text-sm text-gray-600 text-right">${entry.total_cost.toFixed(2)}</td>
                        <td className="px-4 py-2 text-sm text-gray-600 text-right">{entry.spot_percent.toFixed(0)}%</td>
                      </tr>
                    ))}
                </tbody>
              </table>
            </div>
          )}

          <div className="bg-yellow-50 border border-yellow-200 rounded-md p-4 text-sm text-yellow-800">
            Estimates based on list pricing. Actual AWS costs may vary due to regional pricing, data transfer,
            and ancillary service charges. See CLAUDE.md for limitations.
          </div>
        </>
      ) : (
        <div className="text-center py-12 bg-white rounded-lg border">
          <p className="text-gray-500">No cost data available.</p>
        </div>
      )}
    </div>
  );
}

function SummaryCard({ title, value, subtitle }: { title: string; value: string; subtitle: string }) {
  return (
    <div className="bg-white rounded-lg border p-4">
      <dt className="text-sm font-medium text-gray-500">{title}</dt>
      <dd className="mt-1 text-2xl font-semibold text-gray-900">{value}</dd>
      <dd className="mt-1 text-xs text-gray-500">{subtitle}</dd>
    </div>
  );
}

function formatPeriod(start: string, end: string): string {
  try {
    const s = new Date(start);
    const e = new Date(end);
    return `${s.toLocaleDateString()} - ${e.toLocaleDateString()}`;
  } catch {
    return '';
  }
}
