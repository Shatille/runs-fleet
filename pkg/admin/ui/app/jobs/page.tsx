'use client';

import { useEffect, useState, useCallback, useRef } from 'react';
import JobsTable from '@/components/jobs-table';
import JobStatsCard from '@/components/job-stats';
import { TableSkeleton, StatsCardSkeleton } from '@/components/skeleton';
import ConfirmDialog from '@/components/confirm-dialog';
import { useToast } from '@/components/toast';
import { Job, JobStats } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { useAutoRefresh } from '@/hooks/use-auto-refresh';

interface CleanupResult {
  cleaned: number;
  candidates: number;
  job_ids?: number[];
  message: string;
}

export default function JobsPage() {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [stats, setStats] = useState<JobStats | null>(null);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [statusFilter, setStatusFilter] = useState<string>('');
  const [poolFilter, setPoolFilter] = useState<string>('');
  const [offset, setOffset] = useState(0);
  const limit = 50;

  const [searchQuery, setSearchQuery] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const searchTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const [cleanupLoading, setCleanupLoading] = useState(false);
  const [cleanupResult, setCleanupResult] = useState<CleanupResult | null>(null);
  const [showCleanupConfirm, setShowCleanupConfirm] = useState(false);
  const [traceURL, setTraceURL] = useState<string>('');
  const { toast } = useToast();

  const fetchJobs = useCallback(async () => {
    try {
      setLoading(true);
      const params = new URLSearchParams();
      params.set('limit', String(limit));
      params.set('offset', String(offset));
      if (statusFilter) params.set('status', statusFilter);
      if (poolFilter) params.set('pool', poolFilter);

      const res = await apiFetch(`/api/jobs?${params}`);
      if (!res.ok) {
        throw new Error(`Failed to fetch jobs: ${res.statusText}`);
      }
      const data = await res.json();
      setJobs(data.jobs || []);
      setTotal(data.total || 0);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load jobs');
    } finally {
      setLoading(false);
    }
  }, [offset, statusFilter, poolFilter]);

  const fetchStats = useCallback(async () => {
    try {
      const res = await apiFetch('/api/jobs/stats');
      if (res.ok) {
        const data = await res.json();
        setStats(data);
      }
    } catch {
      // Stats fetch failure is non-critical
    }
  }, []);

  useEffect(() => {
    fetchJobs();
    fetchStats();
    apiFetch('/api/config/trace-url').then(async (res) => {
      if (res.ok) {
        const data = await res.json();
        if (data.trace_url) setTraceURL(data.trace_url);
      }
    }).catch(() => {});
  }, [fetchJobs, fetchStats]);

  useEffect(() => {
    return () => {
      if (searchTimerRef.current) clearTimeout(searchTimerRef.current);
    };
  }, []);

  const handleSearchChange = (value: string) => {
    setSearchQuery(value);
    if (searchTimerRef.current) clearTimeout(searchTimerRef.current);
    searchTimerRef.current = setTimeout(() => setDebouncedSearch(value), 300);
  };

  const filteredJobs = debouncedSearch
    ? jobs.filter((job) => {
        const q = debouncedSearch.toLowerCase();
        return (
          (job.repo && job.repo.toLowerCase().includes(q)) ||
          String(job.job_id).includes(q) ||
          (job.instance_id && job.instance_id.toLowerCase().includes(q))
        );
      })
    : jobs;

  const handleRefresh = useCallback(() => {
    fetchJobs();
    fetchStats();
  }, [fetchJobs, fetchStats]);

  const isSearchActive = searchQuery.length > 0 && searchQuery !== debouncedSearch;
  const autoRefreshCallback = useCallback(() => {
    if (!isSearchActive) handleRefresh();
  }, [handleRefresh, isSearchActive]);

  const { enabled: autoRefreshEnabled, toggle: toggleAutoRefresh, isRefreshing } = useAutoRefresh(
    autoRefreshCallback,
    15000,
    'runs-fleet-jobs-auto-refresh',
  );

  const handleCleanupOrphanedJobs = async (dryRun: boolean = false) => {
    try {
      setCleanupLoading(true);
      setCleanupResult(null);
      const params = new URLSearchParams();
      if (dryRun) params.set('dry_run', 'true');

      const res = await apiFetch(`/api/housekeeping/orphaned-jobs?${params}`, {
        method: 'POST',
      });
      if (!res.ok) {
        throw new Error(`Failed to cleanup orphaned jobs: ${res.statusText}`);
      }
      const data = await res.json();
      setCleanupResult(data);
      if (!dryRun && data.cleaned > 0) {
        toast('success', `Cleaned up ${data.cleaned} orphaned job(s)`);
        fetchJobs();
        fetchStats();
      } else if (!dryRun) {
        toast('info', 'No orphaned jobs found');
      }
    } catch (err) {
      toast('error', err instanceof Error ? err.message : 'Failed to cleanup orphaned jobs');
    } finally {
      setCleanupLoading(false);
    }
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
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Jobs</h1>
        <div className="flex items-center gap-2">
          <button
            onClick={toggleAutoRefresh}
            className={`flex items-center gap-1.5 px-3 py-2 rounded-md text-sm transition-colors ${
              autoRefreshEnabled
                ? 'bg-green-100 dark:bg-green-900/40 text-green-700 dark:text-green-400 hover:bg-green-200 dark:hover:bg-green-900/60'
                : 'bg-gray-100 dark:bg-gray-700 text-gray-500 dark:text-gray-400 hover:bg-gray-200 dark:hover:bg-gray-600'
            }`}
          >
            <span className={`inline-block h-2 w-2 rounded-full ${
              autoRefreshEnabled
                ? isRefreshing ? 'bg-green-400 animate-pulse' : 'bg-green-500'
                : 'bg-gray-400'
            }`} />
            Auto-refresh
          </button>
          <button
            onClick={handleRefresh}
            disabled={loading}
            className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-4 py-2 rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
          >
            {loading ? 'Loading...' : 'Refresh'}
          </button>
        </div>
      </div>

      {stats ? <JobStatsCard stats={stats} /> : loading && <StatsCardSkeleton count={7} />}

      <div className="mb-4 p-4 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">Orphaned Jobs Cleanup</h3>
            <p className="text-xs text-gray-500 dark:text-gray-400">Clean up jobs marked as running but whose instances no longer exist</p>
          </div>
          <div className="flex gap-2">
            <button
              onClick={() => handleCleanupOrphanedJobs(true)}
              disabled={cleanupLoading}
              className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-3 py-1.5 text-sm rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
            >
              {cleanupLoading ? 'Checking...' : 'Dry Run'}
            </button>
            <button
              onClick={() => setShowCleanupConfirm(true)}
              disabled={cleanupLoading}
              className="bg-red-600 text-white px-3 py-1.5 text-sm rounded-md hover:bg-red-700 transition-colors disabled:opacity-50"
            >
              {cleanupLoading ? 'Cleaning...' : 'Clean Up'}
            </button>
          </div>
        </div>
        {cleanupResult && (
          <div className={`mt-3 p-3 rounded-md text-sm ${cleanupResult.cleaned > 0 ? 'bg-green-50 dark:bg-green-900/30 text-green-800 dark:text-green-300' : 'bg-gray-50 dark:bg-gray-700 text-gray-700 dark:text-gray-300'}`}>
            <p>{cleanupResult.message}</p>
            {cleanupResult.job_ids && cleanupResult.job_ids.length > 0 && (
              <p className="mt-1 text-xs">Job IDs: {cleanupResult.job_ids.join(', ')}</p>
            )}
          </div>
        )}
      </div>

      <div className="mb-4">
        <input
          type="text"
          placeholder="Search by repo, job ID, or instance ID..."
          value={searchQuery}
          onChange={(e) => handleSearchChange(e.target.value)}
          className="w-full rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500 px-4 py-2"
        />
      </div>

      <div className="mb-4 flex gap-4">
        <select
          value={statusFilter}
          onChange={(e) => {
            setStatusFilter(e.target.value);
            setOffset(0);
          }}
          className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        >
          <option value="">All Statuses</option>
          <option value="pending">Pending</option>
          <option value="queued">Queued</option>
          <option value="running">Running</option>
          <option value="completed">Completed</option>
          <option value="failed">Failed</option>
          <option value="terminated">Terminated</option>
          <option value="requeued">Requeued</option>
          <option value="orphaned">Orphaned</option>
        </select>

        <input
          type="text"
          placeholder="Filter by pool..."
          value={poolFilter}
          onChange={(e) => {
            setPoolFilter(e.target.value);
            setOffset(0);
          }}
          className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        />
      </div>

      {loading && jobs.length === 0 ? (
        <TableSkeleton rows={8} cols={8} />
      ) : filteredJobs.length === 0 ? (
        <div className="text-center py-12 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
          <p className="text-gray-500 dark:text-gray-400">{debouncedSearch ? 'No jobs match your search.' : 'No jobs found.'}</p>
        </div>
      ) : (
        <>
          <JobsTable jobs={filteredJobs} traceURL={traceURL || undefined} />

          <div className="mt-4 flex justify-between items-center">
            <span className="text-sm text-gray-500 dark:text-gray-400">
              Showing {offset + 1}-{Math.min(offset + jobs.length, total)} of {total} jobs
            </span>
            <div className="flex gap-2">
              <button
                onClick={() => setOffset(Math.max(0, offset - limit))}
                disabled={offset === 0}
                className="px-3 py-1 rounded border border-gray-300 dark:border-gray-600 text-sm text-gray-700 dark:text-gray-300 disabled:opacity-50 disabled:cursor-not-allowed hover:bg-gray-50 dark:hover:bg-gray-700"
              >
                Previous
              </button>
              <button
                onClick={() => setOffset(offset + limit)}
                disabled={offset + limit >= total}
                className="px-3 py-1 rounded border border-gray-300 dark:border-gray-600 text-sm text-gray-700 dark:text-gray-300 disabled:opacity-50 disabled:cursor-not-allowed hover:bg-gray-50 dark:hover:bg-gray-700"
              >
                Next
              </button>
            </div>
          </div>
        </>
      )}

      <ConfirmDialog
        open={showCleanupConfirm}
        title="Clean Up Orphaned Jobs"
        message="This will mark orphaned jobs as failed. Run a dry run first to preview affected jobs. Continue?"
        confirmLabel="Clean Up"
        variant="danger"
        onConfirm={() => {
          setShowCleanupConfirm(false);
          handleCleanupOrphanedJobs(false);
        }}
        onCancel={() => setShowCleanupConfirm(false)}
      />
    </div>
  );
}
