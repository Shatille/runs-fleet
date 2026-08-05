'use client';

import { useEffect, useState, useCallback } from 'react';
import { CostSkeleton } from '@/components/skeleton';
import { HelpTip } from '@/components/help-tip';
import { CostByRepositoryTable } from '@/components/cost-by-repository-table';
import { CostSummary, CostDaily, CostByPool, CostByRepository } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { formatMinutes, formatPerMinute } from '@/lib/format';

export default function CostPage() {
  const [summary, setSummary] = useState<CostSummary | null>(null);
  const [daily, setDaily] = useState<CostDaily | null>(null);
  const [byPool, setByPool] = useState<CostByPool | null>(null);
  const [byRepo, setByRepo] = useState<CostByRepository | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchCost = useCallback(async () => {
    try {
      setLoading(true);
      const [summaryRes, dailyRes, byPoolRes, byRepoRes] = await Promise.all([
        apiFetch('/api/cost/summary'),
        apiFetch('/api/cost/daily'),
        apiFetch('/api/cost/by-pool'),
        apiFetch('/api/cost/by-repository'),
      ]);
      if (!summaryRes.ok) {
        throw new Error(`Failed to fetch cost summary: ${summaryRes.statusText}`);
      }
      setSummary(await summaryRes.json());
      if (dailyRes.ok) setDaily(await dailyRes.json());
      if (byPoolRes.ok) setByPool(await byPoolRes.json());
      if (byRepoRes.ok) setByRepo(await byRepoRes.json());
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load cost data');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchCost();
  }, [fetchCost]);

  const handleRefresh = () => {
    setError(null);
    fetchCost();
  };

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <button
          onClick={handleRefresh}
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
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Cost</h1>
        <button
          onClick={handleRefresh}
          disabled={loading}
          className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-4 py-2 rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Refresh'}
        </button>
      </div>

      {loading && !summary ? (
        <CostSkeleton />
      ) : summary ? (
        <>
          <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5 gap-4 mb-6">
            <SummaryCard
              title="Cost / Runner-Minute"
              value={formatPerMinute(summary.cost_per_minute)}
              subtitle={`${formatMinutes(summary.total_minutes)} runner-minutes`}
              help="Actual incurred unit price: Total Cost divided by the billable minutes it was computed from. This is the figure to put next to a hosted runner's per-minute rate, though it blends every runner shape — see the per-shape table below for a like-for-like comparison."
            />
            <SummaryCard
              title="Total Cost"
              value={`$${summary.total_cost.toFixed(2)}`}
              subtitle="Current month estimate"
              help="Estimate only, not billing. Each finished job is priced as its own duration times its instance type's hourly rate, then summed; Spot + On-Demand below add up to this. Because it prices job time rather than instance uptime, it excludes boot time, hot-pool linger, stopped-instance EBS, and any instance still running without a job record — so real EC2 spend is higher."
            />
            <SummaryCard
              title="Avg Cost / Job"
              value={summary.job_count > 0 ? `$${summary.avg_cost_per_job.toFixed(4)}` : '-'}
              subtitle={`${summary.job_count} jobs this month`}
              help="Total Cost divided by Job Count. Mixes architectures, instance sizes, and job durations, so it is only meaningful as a trend."
            />
            <SummaryCard
              title="Spot Savings"
              value={`$${summary.spot_savings.toFixed(2)}`}
              subtitle={`${summary.spot_job_count} spot / ${summary.on_demand_count} on-demand`}
              help="Counterfactual, not a component of Total Cost: for each spot job, its duration times (on-demand rate minus spot rate). Clamped at zero per job, so jobs whose fetched spot price met or exceeded on-demand contribute nothing rather than a negative. When no live spot price is available it falls back to assuming a flat 70% discount, which real prices rarely match — treat this as a rough floor."
            />
            <SummaryCard
              title="Job Count"
              value={String(summary.job_count)}
              subtitle={`${formatPeriod(summary.period_start, summary.period_end)}`}
              help="Finished jobs with a recorded duration in this period. Jobs still running, and instances that never registered a job, are not counted."
            />
          </div>

          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 mb-6">
            <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 p-4">
              <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100 mb-3">Spot vs On-Demand</h3>
              <div className="space-y-2">
                <div className="flex justify-between text-sm">
                  <span className="text-gray-600 dark:text-gray-400">Spot</span>
                  <span className="font-medium text-gray-900 dark:text-gray-100">${summary.spot_cost.toFixed(2)}</span>
                </div>
                <div className="w-full bg-gray-200 dark:bg-gray-700 rounded-full h-2">
                  <div
                    className="bg-green-500 h-2 rounded-full"
                    style={{ width: `${summary.total_cost > 0 ? (summary.spot_cost / summary.total_cost) * 100 : 0}%` }}
                  />
                </div>
                <div className="flex justify-between text-sm">
                  <span className="text-gray-600 dark:text-gray-400">On-Demand</span>
                  <span className="font-medium text-gray-900 dark:text-gray-100">${summary.on_demand_cost.toFixed(2)}</span>
                </div>
                <div className="w-full bg-gray-200 dark:bg-gray-700 rounded-full h-2">
                  <div
                    className="bg-blue-500 h-2 rounded-full"
                    style={{ width: `${summary.total_cost > 0 ? (summary.on_demand_cost / summary.total_cost) * 100 : 0}%` }}
                  />
                </div>
              </div>
            </div>
          </div>

          {daily && daily.days.length > 0 && <DailyChart daily={daily} />}

          {byPool && byPool.pools.length > 0 && (
            <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 overflow-hidden mb-6">
              <div className="px-4 py-3 border-b dark:border-gray-700">
                <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">Breakdown by Pool</h3>
              </div>
              <table className="min-w-full divide-y divide-gray-200 dark:divide-gray-700">
                <thead className="bg-gray-50 dark:bg-gray-700">
                  <tr>
                    <th className="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Pool</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Jobs</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Minutes</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Cost</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">$ / min</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Spot %</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-200 dark:divide-gray-700">
                  {byPool.pools.map((entry) => (
                    <tr key={entry.pool} className="hover:bg-gray-50 dark:hover:bg-gray-700">
                      <td className="px-4 py-2 text-sm font-medium text-gray-900 dark:text-gray-100">{entry.pool}</td>
                      <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{entry.job_count}</td>
                      <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{formatMinutes(entry.total_minutes)}</td>
                      <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">${entry.total_cost.toFixed(2)}</td>
                      <td className="px-4 py-2 text-sm font-medium text-gray-900 dark:text-gray-100 text-right">{formatPerMinute(entry.cost_per_minute)}</td>
                      <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{entry.spot_percent.toFixed(0)}%</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {byRepo && byRepo.repositories.length > 0 && (
            <CostByRepositoryTable repositories={byRepo.repositories} />
          )}

          {summary.family_breakdown.length > 0 && (
            <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 overflow-hidden mb-6">
              <div className="px-4 py-3 border-b dark:border-gray-700">
                <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">Breakdown by Instance Family</h3>
              </div>
              <table className="min-w-full divide-y divide-gray-200 dark:divide-gray-700">
                <thead className="bg-gray-50 dark:bg-gray-700">
                  <tr>
                    <th className="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Family</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Jobs</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Hours</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Cost</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">$ / min</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Spot %</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-200 dark:divide-gray-700">
                  {summary.family_breakdown
                    .sort((a, b) => b.total_cost - a.total_cost)
                    .map((entry) => (
                      <tr key={entry.family} className="hover:bg-gray-50 dark:hover:bg-gray-700">
                        <td className="px-4 py-2 text-sm font-medium text-gray-900 dark:text-gray-100">{entry.family}</td>
                        <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{entry.job_count}</td>
                        <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{entry.total_hours.toFixed(1)}</td>
                        <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">${entry.total_cost.toFixed(2)}</td>
                        <td className="px-4 py-2 text-sm font-medium text-gray-900 dark:text-gray-100 text-right">{formatPerMinute(entry.cost_per_minute)}</td>
                        <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{entry.spot_percent.toFixed(0)}%</td>
                      </tr>
                    ))}
                </tbody>
              </table>
            </div>
          )}

          {summary.runner_minute_breakdown.length > 0 && (
            <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 overflow-hidden mb-6">
              <div className="px-4 py-3 border-b dark:border-gray-700 flex justify-between items-center">
                <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">
                  Unit Cost by Runner Shape
                  <HelpTip text="What one runner-minute actually cost, per (arch, vCPU) shape — the cell to drop into a comparison matrix against a hosted runner's per-minute price for the same shape. $/min is this shape's incurred EC2 cost divided by its own runner-minutes, so it already reflects the spot/on-demand mix it actually ran on. Hosted $/min prices the identical minutes at the reference per-vCPU-minute rate below; it is a baseline, not runs-fleet spend. Shapes are built from job records with a recorded duration and a catalogued instance type, so uncatalogued types are absent and the Cost column can fall short of Total Cost." />
                </h3>
                <span className="text-sm font-semibold text-gray-900 dark:text-gray-100">
                  {formatPerMinute(summary.cost_per_minute)} / min blended
                </span>
              </div>
              <table className="min-w-full divide-y divide-gray-200 dark:divide-gray-700" aria-label="Unit cost per runner-minute by architecture and vCPU">
                <thead className="bg-gray-50 dark:bg-gray-700">
                  <tr>
                    <th className="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Arch</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">vCPU</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Runner-min</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Cost</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">$ / min</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Hosted $ / min</th>
                    <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">vs Hosted</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-200 dark:divide-gray-700">
                  {summary.runner_minute_breakdown.map((entry) => (
                    <tr key={`${entry.arch}-${entry.vcpu}`} className="hover:bg-gray-50 dark:hover:bg-gray-700">
                      <td className="px-4 py-2 text-sm font-medium text-gray-900 dark:text-gray-100">{entry.arch}</td>
                      <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{entry.vcpu}</td>
                      <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{formatMinutes(entry.runner_minutes)}</td>
                      <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">${entry.cost.toFixed(2)}</td>
                      <td className="px-4 py-2 text-sm font-medium text-gray-900 dark:text-gray-100 text-right">{formatPerMinute(entry.cost_per_minute)}</td>
                      <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{formatPerMinute(entry.baseline_cost_per_minute)}</td>
                      <td className="px-4 py-2 text-sm text-right">{formatSavingsMultiple(entry.cost_per_minute, entry.baseline_cost_per_minute)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <div className="px-4 py-2 border-t dark:border-gray-700 text-xs text-gray-500 dark:text-gray-400">
                hosted baseline rate: {formatRates(summary.runner_minute_rates)} per vCPU-minute
                {' '}(${summary.runner_minute_cost.toFixed(2)} for this month&apos;s usage)
              </div>
            </div>
          )}

          <div className="bg-yellow-50 dark:bg-yellow-900/30 border border-yellow-200 dark:border-yellow-800 rounded-md p-4 text-sm text-yellow-800 dark:text-yellow-300">
            runs-fleet cost uses live AWS prices — region-correct on-demand (AWS Pricing API) and current
            spot-market rates — falling back to list pricing if a lookup is unavailable. Every $/min column is
            incurred cost over the billable minutes it was priced from, so cost = minutes × $/min in each row; the
            Hosted $/min column is a fixed reference rate, not runs-fleet spend. Jobs whose record carries no
            duration are billed a 0.5h minimum, which inflates their minutes as well as their cost. Spot is priced
            at current market (not each job&apos;s run-time price), and costs exclude data transfer, EBS, and
            ancillary charges — as well as boot time, hot-pool linger, and idle instances, none of which appear in
            job records. Estimates only — see CLAUDE.md for limitations.
          </div>
        </>
      ) : (
        <div className="text-center py-12 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
          <p className="text-gray-500 dark:text-gray-400">No cost data available.</p>
        </div>
      )}
    </div>
  );
}

function DailyChart({ daily }: { daily: CostDaily }) {
  const maxCost = daily.days.reduce((m, d) => Math.max(m, d.total_cost), 0.0001);
  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 p-4 mb-6">
      <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100 mb-4">
        Daily Cost
        <HelpTip text="Each day's cost and its unit price — cost over that day's billable minutes. A day's $/min moves with the mix of shapes and the spot/on-demand split that ran, not with volume." />
      </h3>
      <div className="flex items-end gap-1 h-40" role="img" aria-label="Daily cost bar chart; see the accompanying data table for values">
        {daily.days.map((d) => (
          <div key={d.date} className="flex-1 flex flex-col justify-end h-full group relative">
            <div
              className="bg-blue-500 dark:bg-blue-600 rounded-t hover:bg-blue-600 dark:hover:bg-blue-500 transition-colors"
              style={{ height: `${(d.total_cost / maxCost) * 100}%` }}
            />
            <div className="pointer-events-none absolute bottom-full left-1/2 -translate-x-1/2 mb-1 hidden group-hover:block whitespace-nowrap rounded bg-gray-900 text-white text-xs px-2 py-1 z-10">
              {d.date}: ${d.total_cost.toFixed(2)} ({d.job_count} jobs,{' '}
              {formatPerMinute(d.cost_per_minute)}/min)
            </div>
          </div>
        ))}
      </div>
      <div className="flex justify-between mt-2 text-xs text-gray-500 dark:text-gray-400">
        <span>{daily.days[0]?.date}</span>
        <span>{daily.days[daily.days.length - 1]?.date}</span>
      </div>
      <table className="sr-only">
        <caption>Daily cost</caption>
        <thead>
          <tr><th>Date</th><th>Total cost (USD)</th><th>Runner-minutes</th><th>Cost per minute (USD)</th><th>Jobs</th></tr>
        </thead>
        <tbody>
          {daily.days.map((d) => (
            <tr key={d.date}>
              <td>{d.date}</td>
              <td>{d.total_cost.toFixed(2)}</td>
              <td>{formatMinutes(d.total_minutes)}</td>
              <td>{formatPerMinute(d.cost_per_minute)}</td>
              <td>{d.job_count}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function SummaryCard({
  title,
  value,
  subtitle,
  help,
}: {
  title: string;
  value: string;
  subtitle: string;
  help?: string;
}) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 p-4">
      <dt className="text-sm font-medium text-gray-500 dark:text-gray-400">
        {title}
        {help && <HelpTip text={help} />}
      </dt>
      <dd className="mt-1 text-2xl font-semibold text-gray-900 dark:text-gray-100">{value}</dd>
      <dd className="mt-1 text-xs text-gray-500 dark:text-gray-400">{subtitle}</dd>
    </div>
  );
}

function formatSavingsMultiple(incurred: number, baseline: number) {
  if (incurred <= 0 || baseline <= 0) {
    return <span className="text-gray-400 dark:text-gray-500">-</span>;
  }
  const ratio = baseline / incurred;
  const cheaper = ratio >= 1;
  return (
    <span className={cheaper ? 'text-green-600 dark:text-green-400' : 'text-red-600 dark:text-red-400'}>
      {cheaper ? `${ratio.toFixed(1)}x cheaper` : `${(1 / ratio).toFixed(1)}x pricier`}
    </span>
  );
}

function formatRates(rates: Record<string, number>): string {
  const entries = Object.entries(rates).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return '-';
  return entries.map(([arch, rate]) => `${arch} $${rate}`).join(' / ');
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
