'use client';

import { useEffect, useState, useCallback, useRef } from 'react';
import JobsTable from '@/components/jobs-table';
import JobStatsCard from '@/components/job-stats';
import HungJobsCard from '@/components/hung-jobs-card';
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
  truncated?: boolean;
  message: string;
  batches?: number;
}

interface RequeueResult {
  requeued: number;
  candidates: number;
  skipped_exhausted: number;
  job_ids?: number[];
  truncated?: boolean;
  message: string;
  batches?: number;
}

// Housekeeping sweeps scan the whole jobs table and can outlive the default
// read timeout, so they get their own.
const HOUSEKEEPING_TIMEOUT_MS = 60000;

// Bounds the drain so a server that never stops reporting more work cannot spin
// the browser forever. At the 100-item default batch this is 10k records.
const MAX_DRAIN_BATCHES = 100;

// How many consecutive batches may act on nothing before the drain gives up.
// One idle batch is normal -- its candidates were all still alive, or all
// refused -- but a run of them means the scan is stuck on rows it cannot act on.
// Three covers ~300 unactionable rows at the default batch size, chosen against
// the ~250 stranded records this was built to clear. A longer run stops with
// truncated still set, so the result says to run it again.
const MAX_IDLE_DRAIN_BATCHES = 3;

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
  const [requeueLoading, setRequeueLoading] = useState(false);
  const [requeueResult, setRequeueResult] = useState<RequeueResult | null>(null);
  const [showRequeueConfirm, setShowRequeueConfirm] = useState(false);
  // ConfirmDialog stays open on confirm, so a key repeat can fire a drain twice.
  const cleanupInFlight = useRef(false);
  const requeueInFlight = useRef(false);
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
    if (cleanupInFlight.current) return;
    cleanupInFlight.current = true;
    try {
      setCleanupLoading(true);
      setCleanupResult(null);
      const params = new URLSearchParams();
      if (dryRun) params.set('dry_run', 'true');

      let cleaned = 0;
      let candidates = 0;
      let jobIDs: number[] = [];
      let truncated = false;
      let batches = 0;
      let idleBatches = 0;

      // The server's truncated flag is the only thing that knows whether rows
      // remain, so it drives the loop. A batch can legitimately clean nothing --
      // every candidate it saw still had a live instance -- while real orphans sit
      // further down the table, so no-progress is tolerated rather than fatal. It
      // is still bounded: a scan that keeps returning the same unactionable rows
      // would otherwise loop until MAX_DRAIN_BATCHES.
      //
      // A dry run mutates nothing, so re-POSTing it would re-scan identical rows
      // forever. It reports one batch and says more remain.
      do {
        const res = await apiFetch(
          `/api/housekeeping/orphaned-jobs?${params}`,
          { method: 'POST' },
          HOUSEKEEPING_TIMEOUT_MS
        );
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          throw new Error(body.details || body.error || `Failed to cleanup orphaned jobs: ${res.statusText}`);
        }
        const data: CleanupResult = await res.json();
        batches++;
        cleaned += data.cleaned;
        candidates += data.candidates;
        if (data.job_ids) jobIDs = jobIDs.concat(data.job_ids);
        truncated = Boolean(data.truncated);

        idleBatches = data.cleaned === 0 ? idleBatches + 1 : 0;
        if (idleBatches >= MAX_IDLE_DRAIN_BATCHES) break;
      } while (!dryRun && truncated && batches < MAX_DRAIN_BATCHES);

      const message = dryRun
        ? `Dry run: would clean ${jobIDs.length} orphaned job(s)${truncated ? ', and more remain after this batch' : ''}`
        : `Cleaned ${cleaned} orphaned job(s)${batches > 1 ? ` in ${batches} batches` : ''}` +
          `${truncated ? '; more remain — run again' : ''}`;
      setCleanupResult({ cleaned, candidates, job_ids: jobIDs, truncated, batches, message });

      if (!dryRun && cleaned > 0) {
        toast('success', `Cleaned up ${cleaned} orphaned job(s)`);
        fetchJobs();
        fetchStats();
      } else if (!dryRun) {
        toast('info', 'No orphaned jobs found');
      }
    } catch (err) {
      toast('error', err instanceof Error ? err.message : 'Failed to cleanup orphaned jobs');
    } finally {
      setCleanupLoading(false);
      cleanupInFlight.current = false;
    }
  };

  const handleRequeueHungJobs = async (dryRun: boolean = false) => {
    if (requeueInFlight.current) return;
    requeueInFlight.current = true;
    try {
      setRequeueLoading(true);
      setRequeueResult(null);
      const params = new URLSearchParams();
      if (dryRun) params.set('dry_run', 'true');

      let requeued = 0;
      let candidates = 0;
      let skipped = 0;
      let jobIDs: number[] = [];
      let truncated = false;
      let batches = 0;
      let idleBatches = 0;
      let lastMessage = '';

      // As with cleanup, truncated drives the loop: a batch whose candidates were
      // all refused (retries exhausted, GitHub says the job is running) requeues
      // nothing while re-dispatchable records may still sit further down the
      // table. Bounded so a scan that keeps returning the same refused rows stops.
      do {
        const res = await apiFetch(
          `/api/housekeeping/requeue-hung-jobs?${params}`,
          { method: 'POST' },
          HOUSEKEEPING_TIMEOUT_MS
        );
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          throw new Error(body.details || body.error || `Failed to requeue hung jobs: ${res.statusText}`);
        }
        const data: RequeueResult = await res.json();
        batches++;
        requeued += data.requeued;
        candidates += data.candidates;
        skipped += data.skipped_exhausted;
        if (data.job_ids) jobIDs = jobIDs.concat(data.job_ids);
        truncated = Boolean(data.truncated);
        lastMessage = data.message;

        idleBatches = data.requeued === 0 ? idleBatches + 1 : 0;
        if (idleBatches >= MAX_IDLE_DRAIN_BATCHES) break;
      } while (!dryRun && truncated && batches < MAX_DRAIN_BATCHES);

      let message: string;
      if (candidates === 0) {
        // The server explains which records this action is even about.
        message = lastMessage;
      } else if (dryRun) {
        message = `Dry run: would requeue ${jobIDs.length} hung job(s)${truncated ? ', and more remain after this batch' : ''}`;
      } else {
        message =
          `Requeued ${requeued} hung job(s)${batches > 1 ? ` in ${batches} batches` : ''}` +
          `${truncated ? '; more remain — run again' : ''}`;
      }
      setRequeueResult({
        requeued,
        candidates,
        skipped_exhausted: skipped,
        job_ids: jobIDs,
        truncated,
        batches,
        message,
      });

      if (!dryRun && requeued > 0) {
        toast('success', `Requeued ${requeued} hung job(s)`);
        fetchJobs();
        fetchStats();
      } else if (!dryRun) {
        toast('info', 'No hung jobs to requeue');
      }
    } catch (err) {
      toast('error', err instanceof Error ? err.message : 'Failed to requeue hung jobs');
    } finally {
      setRequeueLoading(false);
      requeueInFlight.current = false;
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

      <HungJobsCard onActed={handleRefresh} />

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
            {cleanupResult.truncated && (
              <p className="mt-1 text-xs">
                The cap stopped this run before the end of the table. Run Clean Up again to continue.
              </p>
            )}
          </div>
        )}
      </div>

      <div className="mb-4 p-4 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">Requeue Unconfirmed Runners</h3>
            <p className="text-xs text-gray-500 dark:text-gray-400">
              Re-dispatch a fresh runner for jobs whose instance launched but whose runner never confirmed. The GitHub job
              is preserved; only the runner is re-driven. This acts on <span className="font-mono">launched</span> records
              only — a job listed above as done upstream needs Orphaned Jobs Cleanup instead.
            </p>
          </div>
          <div className="flex gap-2">
            <button
              onClick={() => handleRequeueHungJobs(true)}
              disabled={requeueLoading}
              className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-3 py-1.5 text-sm rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
            >
              {requeueLoading ? 'Checking...' : 'Dry Run'}
            </button>
            <button
              onClick={() => setShowRequeueConfirm(true)}
              disabled={requeueLoading}
              className="bg-blue-600 text-white px-3 py-1.5 text-sm rounded-md hover:bg-blue-700 transition-colors disabled:opacity-50"
            >
              {requeueLoading ? 'Requeuing...' : 'Requeue'}
            </button>
          </div>
        </div>
        {requeueResult && (
          <div className={`mt-3 p-3 rounded-md text-sm ${requeueResult.requeued > 0 ? 'bg-green-50 dark:bg-green-900/30 text-green-800 dark:text-green-300' : 'bg-gray-50 dark:bg-gray-700 text-gray-700 dark:text-gray-300'}`}>
            <p>{requeueResult.message}</p>
            {requeueResult.job_ids && requeueResult.job_ids.length > 0 && (
              <p className="mt-1 text-xs">Job IDs: {requeueResult.job_ids.join(', ')}</p>
            )}
            {requeueResult.skipped_exhausted > 0 && (
              <p className="mt-1 text-xs">Skipped (retries exhausted): {requeueResult.skipped_exhausted}</p>
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
          <JobsTable jobs={filteredJobs} traceURL={traceURL || undefined} onActed={handleRefresh} />

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

      <ConfirmDialog
        open={showRequeueConfirm}
        title="Requeue Unconfirmed Runners"
        message="This re-dispatches a fresh runner for launched jobs whose runner never confirmed, by re-enqueuing into the runs-fleet queue. The GitHub job is not cancelled or re-run. Run a dry run first to preview affected jobs. Continue?"
        confirmLabel="Requeue"
        variant="default"
        onConfirm={() => {
          setShowRequeueConfirm(false);
          handleRequeueHungJobs(false);
        }}
        onCancel={() => setShowRequeueConfirm(false)}
      />
    </div>
  );
}
