'use client';

import { useEffect, useState, useCallback } from 'react';
import AuditLogsTable from '@/components/audit-logs-table';
import { TableSkeleton } from '@/components/skeleton';
import { AuditEntry } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { useAutoRefresh } from '@/hooks/use-auto-refresh';

export default function AuditPage() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notConfigured, setNotConfigured] = useState(false);

  const [userFilter, setUserFilter] = useState('');
  const [actionFilter, setActionFilter] = useState('');
  const [sinceFilter, setSinceFilter] = useState('');
  const [untilFilter, setUntilFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const limit = 50;

  const fetchAuditLogs = useCallback(async () => {
    try {
      setLoading(true);
      setNotConfigured(false);
      const params = new URLSearchParams();
      params.set('limit', String(limit));
      params.set('offset', String(offset));
      if (userFilter) params.set('user', userFilter);
      if (actionFilter) params.set('action', actionFilter);
      if (sinceFilter) params.set('since', new Date(sinceFilter).toISOString());
      if (untilFilter) params.set('until', new Date(untilFilter).toISOString());

      const res = await apiFetch(`/api/audit-logs?${params}`);
      if (res.status === 503) {
        setNotConfigured(true);
        setEntries([]);
        return;
      }
      if (!res.ok) {
        throw new Error(`Failed to fetch audit logs: ${res.statusText}`);
      }
      const data = await res.json();
      setEntries(data.entries || []);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load audit logs');
    } finally {
      setLoading(false);
    }
  }, [offset, userFilter, actionFilter, sinceFilter, untilFilter]);

  useEffect(() => {
    fetchAuditLogs();
  }, [fetchAuditLogs]);

  const { enabled: autoRefreshEnabled, toggle: toggleAutoRefresh, isRefreshing } = useAutoRefresh(
    fetchAuditLogs,
    15000,
    'runs-fleet-audit-auto-refresh',
  );

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <button
          onClick={fetchAuditLogs}
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
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Audit Log</h1>
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
            onClick={fetchAuditLogs}
            disabled={loading}
            className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-4 py-2 rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
          >
            {loading ? 'Loading...' : 'Refresh'}
          </button>
        </div>
      </div>

      {notConfigured ? (
        <div className="text-center py-12 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
          <p className="text-gray-500 dark:text-gray-400">
            Audit log persistence is not configured (RUNS_FLEET_AUDIT_TABLE unset).
          </p>
        </div>
      ) : (
        <>
          <div className="mb-4 flex flex-wrap gap-4">
            <input
              type="text"
              placeholder="Filter by user..."
              value={userFilter}
              onChange={(e) => {
                setUserFilter(e.target.value);
                setOffset(0);
              }}
              className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500"
            />
            <input
              type="text"
              placeholder="Filter by action..."
              value={actionFilter}
              onChange={(e) => {
                setActionFilter(e.target.value);
                setOffset(0);
              }}
              className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500"
            />
            <label className="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-400">
              Since
              <input
                type="datetime-local"
                value={sinceFilter}
                onChange={(e) => {
                  setSinceFilter(e.target.value);
                  setOffset(0);
                }}
                className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500"
              />
            </label>
            <label className="flex items-center gap-2 text-sm text-gray-600 dark:text-gray-400">
              Until
              <input
                type="datetime-local"
                value={untilFilter}
                onChange={(e) => {
                  setUntilFilter(e.target.value);
                  setOffset(0);
                }}
                className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500"
              />
            </label>
          </div>

          {loading && entries.length === 0 ? (
            <TableSkeleton rows={8} cols={6} />
          ) : entries.length === 0 ? (
            <div className="text-center py-12 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
              <p className="text-gray-500 dark:text-gray-400">No audit entries found.</p>
            </div>
          ) : (
            <>
              <AuditLogsTable entries={entries} />

              <div className="mt-4 flex justify-between items-center">
                <span className="text-sm text-gray-500 dark:text-gray-400">
                  Showing {offset + 1}-{offset + entries.length}
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
                    disabled={entries.length < limit}
                    className="px-3 py-1 rounded border border-gray-300 dark:border-gray-600 text-sm text-gray-700 dark:text-gray-300 disabled:opacity-50 disabled:cursor-not-allowed hover:bg-gray-50 dark:hover:bg-gray-700"
                  >
                    Next
                  </button>
                </div>
              </div>
            </>
          )}
        </>
      )}
    </div>
  );
}
