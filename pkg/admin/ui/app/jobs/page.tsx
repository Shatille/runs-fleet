'use client';

import { useEffect, useState, useCallback } from 'react';
import JobsTable from '@/components/jobs-table';
import JobStatsCard from '@/components/job-stats';
import { Job, JobStats } from '@/lib/types';
import { apiFetch } from '@/lib/api';

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
  }, [fetchJobs, fetchStats]);

  const handleRefresh = () => {
    fetchJobs();
    fetchStats();
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
        <button
          onClick={handleRefresh}
          disabled={loading}
          className="bg-gray-100 text-gray-700 px-4 py-2 rounded-md hover:bg-gray-200 transition-colors disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Refresh'}
        </button>
      </div>

      {stats && <JobStatsCard stats={stats} />}

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
      ) : jobs.length === 0 ? (
        <div className="text-center py-12 bg-white rounded-lg border">
          <p className="text-gray-500">No jobs found.</p>
        </div>
      ) : (
        <>
          <JobsTable jobs={jobs} />

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
