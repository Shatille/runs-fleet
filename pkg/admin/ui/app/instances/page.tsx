'use client';

import { useEffect, useState, useCallback, useMemo, useRef } from 'react';
import InstancesTable from '@/components/instances-table';
import AMICard from '@/components/ami-card';
import { StatsCardSkeleton, TableSkeleton } from '@/components/skeleton';
import ConfirmDialog from '@/components/confirm-dialog';
import { useToast } from '@/components/toast';
import { Instance, OrphanedInstancesResult } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { useAutoRefresh } from '@/hooks/use-auto-refresh';

interface BulkTerminateResult {
  terminated: string[];
  busy: string[];
  failed: string[];
}

export default function InstancesPage() {
  const [instances, setInstances] = useState<Instance[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [poolFilter, setPoolFilter] = useState<string>('');
  const [stateFilter, setStateFilter] = useState<string>('');
  const [staleOnly, setStaleOnly] = useState(false);
  const [amiUnknown, setAmiUnknown] = useState(false);

  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [bulkPending, setBulkPending] = useState(false);
  const [showBulkConfirm, setShowBulkConfirm] = useState(false);
  const [bulkResult, setBulkResult] = useState<BulkTerminateResult | null>(null);

  const [reapLoading, setReapLoading] = useState(false);
  const [reapResult, setReapResult] = useState<OrphanedInstancesResult | null>(null);
  const [showReapConfirm, setShowReapConfirm] = useState(false);
  const { toast } = useToast();

  // ConfirmDialog does not close on confirm and its button carries no pending
  // state, so a key repeat can fire it twice before React re-renders. Refs reject
  // the second call in the same tick, which the loading flags cannot.
  const bulkInFlight = useRef(false);
  const reapInFlight = useRef(false);

  const fetchInstances = useCallback(async () => {
    try {
      setLoading(true);
      const params = new URLSearchParams();
      if (poolFilter) params.set('pool', poolFilter);
      if (stateFilter) params.set('state', stateFilter);

      const query = params.toString();
      const res = await apiFetch(`/api/instances${query ? '?' + query : ''}`);
      if (!res.ok) {
        throw new Error(`Failed to fetch instances: ${res.statusText}`);
      }
      const data = await res.json();
      setInstances(data.instances || []);
      setAmiUnknown(Boolean(data.ami_current_unknown));
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load instances');
    } finally {
      setLoading(false);
    }
  }, [poolFilter, stateFilter]);

  useEffect(() => {
    fetchInstances();
  }, [fetchInstances]);

  // Drop selections for instances the latest fetch no longer lists, so a stale id
  // cannot ride along into the next bulk action.
  useEffect(() => {
    setSelected((prev) => {
      if (prev.size === 0) return prev;
      const live = new Set(instances.map((i) => i.instance_id));
      const next = new Set([...prev].filter((id) => live.has(id)));
      return next.size === prev.size ? prev : next;
    });
  }, [instances]);

  const selectedIds = useMemo(() => [...selected], [selected]);

  // Filtered client-side: staleness is derived from the reference AMI the server
  // already resolved, so a round trip would buy nothing.
  const visibleInstances = useMemo(
    () => (staleOnly ? instances.filter((i) => i.ami_stale) : instances),
    [instances, staleOnly],
  );

  // Terminations run one at a time through the same endpoint the per-row button
  // uses, so each keeps its active-job 409 guard. A busy instance is reported, not
  // force-killed: forcing stays a deliberate per-row act.
  const terminateSelected = useCallback(async () => {
    if (bulkInFlight.current) return;
    bulkInFlight.current = true;
    setShowBulkConfirm(false);
    setBulkPending(true);
    const result: BulkTerminateResult = { terminated: [], busy: [], failed: [] };
    try {
      for (const id of selectedIds) {
        try {
          const res = await apiFetch(`/api/instances/${encodeURIComponent(id)}`, { method: 'DELETE' });
          if (res.ok) result.terminated.push(id);
          else if (res.status === 409) result.busy.push(id);
          else result.failed.push(id);
        } catch {
          result.failed.push(id);
        }
      }
      setBulkResult(result);
      setSelected(new Set(result.busy.concat(result.failed)));
      if (result.terminated.length > 0) {
        toast('success', `Termination requested for ${result.terminated.length} instance(s)`);
      }
      if (result.busy.length > 0) {
        toast('info', `${result.busy.length} instance(s) still serving a job were left alone`);
      }
      if (result.failed.length > 0) {
        toast('error', `${result.failed.length} instance(s) could not be terminated`);
      }
      fetchInstances();
    } finally {
      bulkInFlight.current = false;
      setBulkPending(false);
    }
  }, [selectedIds, toast, fetchInstances]);

  const reapOrphanedInstances = useCallback(
    async (dryRun: boolean) => {
      if (reapInFlight.current) return;
      reapInFlight.current = true;
      try {
        setReapLoading(true);
        setReapResult(null);
        const params = new URLSearchParams();
        if (dryRun) params.set('dry_run', 'true');

        const res = await apiFetch(`/api/housekeeping/orphaned-instances?${params}`, { method: 'POST' });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          throw new Error(data.details || data.error || 'Failed to sweep orphaned instances');
        }
        setReapResult(data);
        if (!dryRun) {
          if (data.terminated > 0) {
            toast('success', `Terminated ${data.terminated} orphaned instance(s)`);
            fetchInstances();
          } else {
            toast('info', 'No orphaned instances found');
          }
        }
      } catch (err) {
        toast('error', err instanceof Error ? err.message : 'Failed to sweep orphaned instances');
      } finally {
        reapInFlight.current = false;
        setReapLoading(false);
      }
    },
    [toast, fetchInstances],
  );

  const handleRefresh = useCallback(() => {
    fetchInstances();
  }, [fetchInstances]);

  const { enabled: autoRefreshEnabled, toggle: toggleAutoRefresh, isRefreshing } = useAutoRefresh(
    handleRefresh,
    15000,
    'runs-fleet-instances-auto-refresh',
  );

  const stats = {
    total: instances.length,
    running: instances.filter((i) => i.state === 'running').length,
    stopped: instances.filter((i) => i.state === 'stopped').length,
    busy: instances.filter((i) => i.busy).length,
    spot: instances.filter((i) => i.spot).length,
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
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Instances</h1>
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

      {loading && instances.length === 0 ? (
        <StatsCardSkeleton count={5} />
      ) : (
        <div className="grid grid-cols-2 md:grid-cols-5 gap-4 mb-6">
          <StatCard label="Total" value={stats.total} />
          <StatCard label="Running" value={stats.running} color="green" />
          <StatCard label="Stopped" value={stats.stopped} color="gray" />
          <StatCard label="Busy" value={stats.busy} color="yellow" />
          <StatCard label="Spot" value={stats.spot} color="orange" />
        </div>
      )}

      <div className="mb-4 p-4 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
        <div className="flex items-center justify-between gap-4 flex-wrap">
          <div>
            <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">Orphaned Instances</h3>
            <p className="text-xs text-gray-500 dark:text-gray-400">
              Terminate instances nothing owns any more: over-runtime runners, untagged zombies, cold
              starts that never claimed a job, instances that finished but failed to self-terminate,
              and stopped instances outside every pool. Runs the same sweep as scheduled housekeeping.
            </p>
          </div>
          <div className="flex gap-2">
            <button
              onClick={() => reapOrphanedInstances(true)}
              disabled={reapLoading}
              className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-3 py-1.5 text-sm rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
            >
              {reapLoading ? 'Checking...' : 'Dry Run'}
            </button>
            <button
              onClick={() => setShowReapConfirm(true)}
              disabled={reapLoading}
              className="bg-red-600 text-white px-3 py-1.5 text-sm rounded-md hover:bg-red-700 transition-colors disabled:opacity-50"
            >
              {reapLoading ? 'Reaping...' : 'Reap'}
            </button>
          </div>
        </div>
        {reapResult && (
          <div
            className={`mt-3 p-3 rounded-md text-sm ${
              reapResult.candidates > 0
                ? 'bg-green-50 dark:bg-green-900/30 text-green-800 dark:text-green-300'
                : 'bg-gray-50 dark:bg-gray-700 text-gray-700 dark:text-gray-300'
            }`}
          >
            <p>{reapResult.message}</p>
            {reapResult.instance_ids && reapResult.instance_ids.length > 0 && (
              <p className="mt-1 text-xs font-mono">{reapResult.instance_ids.join(', ')}</p>
            )}
          </div>
        )}
      </div>

      {selected.size > 0 && (
        <div className="mb-4 p-3 bg-blue-50 dark:bg-blue-900/30 border border-blue-200 dark:border-blue-800 rounded-lg flex items-center justify-between gap-4 flex-wrap">
          <span className="text-sm text-blue-900 dark:text-blue-200">
            {selected.size} instance{selected.size === 1 ? '' : 's'} selected
          </span>
          <div className="flex gap-2">
            <button
              onClick={() => setSelected(new Set())}
              disabled={bulkPending}
              className="px-3 py-1.5 text-sm rounded-md bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-300 border border-gray-300 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-700 disabled:opacity-50"
            >
              Clear
            </button>
            <button
              onClick={() => setShowBulkConfirm(true)}
              disabled={bulkPending}
              className="px-3 py-1.5 text-sm rounded-md bg-red-600 text-white hover:bg-red-700 transition-colors disabled:opacity-50"
            >
              {bulkPending ? 'Terminating...' : `Terminate ${selected.size} selected`}
            </button>
          </div>
        </div>
      )}

      {bulkResult && (
        <div className="mb-4 p-3 rounded-lg text-sm bg-gray-50 dark:bg-gray-800 border dark:border-gray-700 text-gray-700 dark:text-gray-300">
          <p>
            Terminated {bulkResult.terminated.length}, left {bulkResult.busy.length} still serving a
            job, failed on {bulkResult.failed.length}.
          </p>
          {bulkResult.busy.length > 0 && (
            <p className="mt-1 text-xs font-mono">
              Still busy: {bulkResult.busy.join(', ')} — terminate these one at a time to see the job
              each would kill.
            </p>
          )}
          {bulkResult.failed.length > 0 && (
            <p className="mt-1 text-xs font-mono">Failed: {bulkResult.failed.join(', ')}</p>
          )}
        </div>
      )}

      <AMICard instances={instances} amiUnknown={amiUnknown} onReplaced={fetchInstances} />

      <div className="mb-4 flex gap-4 items-center">
        <input
          type="text"
          placeholder="Filter by pool..."
          value={poolFilter}
          onChange={(e) => setPoolFilter(e.target.value)}
          className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        />

        <label className="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
          <input
            type="checkbox"
            checked={staleOnly}
            disabled={amiUnknown}
            onChange={(e) => setStaleOnly(e.target.checked)}
            className="rounded border-gray-300 dark:border-gray-600 text-blue-600 focus:ring-blue-500 disabled:opacity-40"
          />
          Stale AMI only
        </label>

        <select
          value={stateFilter}
          onChange={(e) => setStateFilter(e.target.value)}
          className="rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-100 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        >
          <option value="">All States</option>
          <option value="running">Running</option>
          <option value="stopped">Stopped</option>
          <option value="pending">Pending</option>
          <option value="stopping">Stopping</option>
        </select>
      </div>

      {loading && instances.length === 0 ? (
        <TableSkeleton rows={5} cols={8} />
      ) : instances.length === 0 ? (
        <div className="text-center py-12 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
          <p className="text-gray-500 dark:text-gray-400">No instances found.</p>
        </div>
      ) : (
        <InstancesTable
          instances={visibleInstances}
          onTerminated={fetchInstances}
          selected={selected}
          onSelectionChange={setSelected}
        />
      )}

      <ConfirmDialog
        open={showBulkConfirm}
        title="Terminate Selected Instances"
        message={`Terminate ${selected.size} instance${selected.size === 1 ? '' : 's'}? Any that are still serving a job are skipped and listed instead. This cannot be undone.`}
        confirmLabel="Terminate"
        variant="danger"
        onConfirm={terminateSelected}
        onCancel={() => setShowBulkConfirm(false)}
      />

      <ConfirmDialog
        open={showReapConfirm}
        title="Reap Orphaned Instances"
        message="Terminate every instance this sweep considers orphaned. Run a dry run first to see the list. Continue?"
        confirmLabel="Reap"
        variant="danger"
        onConfirm={() => {
          setShowReapConfirm(false);
          reapOrphanedInstances(false);
        }}
        onCancel={() => setShowReapConfirm(false)}
      />
    </div>
  );
}

interface StatCardProps {
  label: string;
  value: number;
  color?: 'green' | 'gray' | 'yellow' | 'orange';
}

function StatCard({ label, value, color }: StatCardProps) {
  const colorClasses = {
    green: 'bg-green-50 dark:bg-green-900/30 text-green-700 dark:text-green-400',
    gray: 'bg-gray-50 dark:bg-gray-800 text-gray-700 dark:text-gray-300',
    yellow: 'bg-yellow-50 dark:bg-yellow-900/30 text-yellow-700 dark:text-yellow-400',
    orange: 'bg-orange-50 dark:bg-orange-900/30 text-orange-700 dark:text-orange-400',
  };

  const bgClass = color ? colorClasses[color] : 'bg-gray-50 dark:bg-gray-800 text-gray-700 dark:text-gray-300';

  return (
    <div className={`rounded-lg p-4 ${bgClass}`}>
      <div className="text-sm font-medium opacity-75">{label}</div>
      <div className="text-2xl font-bold">{value}</div>
    </div>
  );
}
