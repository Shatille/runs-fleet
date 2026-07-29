'use client';

import { useMemo, useState } from 'react';
import { HelpTip } from '@/components/help-tip';
import { CostRepositoryEntry } from '@/lib/types';

const PAGE_SIZES = [20, 50, 100] as const;
const ALL = 'all';

type PageSize = (typeof PAGE_SIZES)[number] | typeof ALL;

export function CostByRepositoryTable({
  repositories,
}: {
  repositories: CostRepositoryEntry[];
}) {
  const [query, setQuery] = useState('');
  const [pageSize, setPageSize] = useState<PageSize>(20);
  const [page, setPage] = useState(0);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return repositories;
    return repositories.filter((r) => r.repository.toLowerCase().includes(q));
  }, [repositories, query]);

  const perPage = pageSize === ALL ? filtered.length : pageSize;
  const pageCount = perPage > 0 ? Math.ceil(filtered.length / perPage) : 1;
  // Clamp rather than store a corrected page: shrinking the result set with a
  // filter must not leave us rendering an out-of-range slice for a frame.
  const currentPage = Math.min(page, Math.max(pageCount - 1, 0));
  const start = currentPage * perPage;
  const visible = pageSize === ALL ? filtered : filtered.slice(start, start + perPage);

  const shownTotal = useMemo(
    () => filtered.reduce((sum, r) => sum + r.total_cost, 0),
    [filtered]
  );

  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 overflow-hidden mb-6">
      <div className="px-4 py-3 border-b dark:border-gray-700">
        <div className="flex justify-between items-center gap-4 flex-wrap">
          <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">
            Breakdown by Repository
            <HelpTip text="Month-to-date cost attributed to the repository that requested each job, priced the same way as Total Cost. Jobs whose record carries no repository are grouped under &quot;unknown&quot;. Every repository is listed, so the rows sum to Total Cost when no filter is applied." />
          </h3>
          <span className="text-sm text-gray-500 dark:text-gray-400" role="status" aria-live="polite">
            {filtered.length} of {repositories.length}{' '}
            {repositories.length === 1 ? 'repository' : 'repositories'} · $
            {shownTotal.toFixed(2)}
          </span>
        </div>
        <div className="mt-3 flex gap-3 flex-wrap items-center">
          <input
            type="search"
            placeholder="Filter repositories..."
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setPage(0);
            }}
            aria-label="Filter repositories by name"
            className="flex-1 min-w-[12rem] rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500 px-3 py-1.5 text-sm"
          />
          <label className="text-sm text-gray-500 dark:text-gray-400 flex items-center gap-2">
            Rows
            <select
              value={String(pageSize)}
              onChange={(e) => {
                const v = e.target.value;
                setPageSize(v === ALL ? ALL : (Number(v) as PageSize));
                setPage(0);
              }}
              className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500 text-sm"
            >
              {PAGE_SIZES.map((size) => (
                <option key={size} value={size}>
                  {size}
                </option>
              ))}
              <option value={ALL}>All</option>
            </select>
          </label>
        </div>
      </div>

      {visible.length === 0 ? (
        <p className="px-4 py-6 text-sm text-center text-gray-500 dark:text-gray-400">
          No repositories match &quot;{query}&quot;.
        </p>
      ) : (
        <table className="min-w-full divide-y divide-gray-200 dark:divide-gray-700">
          <thead className="bg-gray-50 dark:bg-gray-700">
            <tr>
              <th className="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Repository</th>
              <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Jobs</th>
              <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Cost</th>
              <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Avg / Job</th>
              <th className="px-4 py-2 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase">Spot %</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-gray-200 dark:divide-gray-700">
            {visible.map((entry) => (
              <tr key={entry.repository} className="hover:bg-gray-50 dark:hover:bg-gray-700">
                <td className="px-4 py-2 text-sm font-medium text-gray-900 dark:text-gray-100">{entry.repository}</td>
                <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{entry.job_count}</td>
                <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">${entry.total_cost.toFixed(2)}</td>
                <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">${entry.avg_cost_per_job.toFixed(4)}</td>
                <td className="px-4 py-2 text-sm text-gray-600 dark:text-gray-400 text-right">{entry.spot_percent.toFixed(0)}%</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {visible.length > 0 && (
        <div className="px-4 py-3 border-t dark:border-gray-700 flex justify-between items-center">
          <span className="text-sm text-gray-500 dark:text-gray-400">
            {start + 1}-{start + visible.length} of {filtered.length}
          </span>
          {pageCount > 1 && (
            <div className="flex gap-2">
              <button
                onClick={() => setPage(currentPage - 1)}
                disabled={currentPage === 0}
                className="px-3 py-1 text-sm rounded-md bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
              >
                Previous
              </button>
              <span className="px-2 py-1 text-sm text-gray-500 dark:text-gray-400">
                Page {currentPage + 1} of {pageCount}
              </span>
              <button
                onClick={() => setPage(currentPage + 1)}
                disabled={currentPage >= pageCount - 1}
                className="px-3 py-1 text-sm rounded-md bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
              >
                Next
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
