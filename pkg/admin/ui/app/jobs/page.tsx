'use client';

import { useEffect, useState, useCallback, useRef } from 'react';
import JobsTable from '@/components/jobs-table';
import JobStatsCard from '@/components/job-stats';
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
  const [traceURL, setTraceURL] = useState<string>('');

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
        fetchJobs();
        fetchStats();
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to cleanup orphaned jobs');
    } finally {
      setCleanupLoading(false);
    }
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
        <h1 className="text-2xl font-bold text-gray-900">Jobs</h1>
        <div className="flex items-center gap-2">
          <button
            onClick={toggleAutoRefresh}
            className={`flex items-center gap-1.5 px-3 py-2 rounded-md text-sm transition-colors ${
              autoRefreshEnabled
                ? 'bg-green-100 text-green-700 hover:bg-green-200'
                : 'bg-gray-100 text-gray-500 hover:bg-gray-200'
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
            className="bg-gray-100 text-gray-700 px-4 py-2 rounded-md hover:bg-gray-200 transition-colors disabled:opacity-50"
          >
            {loading ? 'Loading...' : 'Refresh'}
          </button>
        </div>
      </div>

      {stats && <JobStatsCard stats={stats} />}

      <div className="mb-4 p-4 bg-white rounded-lg border">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-medium text-gray-900">Orphaned Jobs Cleanup</h3>
            <p className="text-xs text-gray-500">Clean up jobs marked as running but whose instances no longer exist</p>
          </div>
          <div className="flex gap-2">
            <button
              onClick={() => handleCleanupOrphanedJobs(true)}
              disabled={cleanupLoading}
              className="bg-gray-100 text-gray-700 px-3 py-1.5 text-sm rounded-md hover:bg-gray-200 transition-colors disabled:opacity-50"
            >
              {cleanupLoading ? 'Checking...' : 'Dry Run'}
            </button>
            <button
              onClick={() => handleCleanupOrphanedJobs(false)}
              disabled={cleanupLoading}
              className="bg-red-600 text-white px-3 py-1.5 text-sm rounded-md hover:bg-red-700 transition-colors disabled:opacity-50"
            >
              {cleanupLoading ? 'Cleaning...' : 'Clean Up'}
            </button>
          </div>
        </div>
        {cleanupResult && (
          <div className={`mt-3 p-3 rounded-md text-sm ${cleanupResult.cleaned > 0 ? 'bg-green-50 text-green-800' : 'bg-gray-50 text-gray-700'}`}>
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
          className="w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500 px-4 py-2"
        />
      </div>

      <div className="mb-4 flex gap-4">
        <select
          value={statusFilter}
          onChange={(e) => {
            setStatusFilter(e.target.value);
            setOffset(0);
          }}
          className="rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
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
          className="rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        />
      </div>

      {loading && jobs.length === 0 ? (
        <div className="flex items-center justify-center h-64">
          <div className="text-gray-500">Loading jobs...</div>
        </div>
      ) : filteredJobs.length === 0 ? (
        <div className="text-center py-12 bg-white rounded-lg border">
          <p className="text-gray-500">{debouncedSearch ? 'No jobs match your search.' : 'No jobs found.'}</p>
        </div>
      ) : (
        <>
          <JobsTable jobs={filteredJobs} traceURL={traceURL || undefined} />

          <div className="mt-4 flex justify-between items-center">
            <span className="text-sm text-gray-500">
              Showing {offset + 1}-{Math.min(offset + jobs.length, total)} of {total} jobs
            </span>
            <div className="flex gap-2">
              <button
                onClick={() => setOffset(Math.max(0, offset - limit))}
                disabled={offset === 0}
                className="px-3 py-1 rounded border border-gray-300 text-sm disabled:opacity-50 disabled:cursor-not-allowed hover:bg-gray-50"
              >
                Previous
              </button>
              <button
                onClick={() => setOffset(offset + limit)}
                disabled={offset + limit >= total}
                className="px-3 py-1 rounded border border-gray-300 text-sm disabled:opacity-50 disabled:cursor-not-allowed hover:bg-gray-50"
              >
                Next
              </button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
