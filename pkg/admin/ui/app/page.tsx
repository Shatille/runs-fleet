'use client';

import { useCallback, useEffect, useState } from 'react';
import PoolTable from '@/components/pool-table';
import { TableSkeleton } from '@/components/skeleton';
import ConfirmDialog from '@/components/confirm-dialog';
import { useToast } from '@/components/toast';
import { Pool } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { useAutoRefresh } from '@/hooks/use-auto-refresh';

export default function HomePage() {
  const [pools, setPools] = useState<Pool[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const { toast } = useToast();

  const fetchPools = useCallback(async () => {
    try {
      setLoading(true);
      const res = await apiFetch('/api/pools');
      if (!res.ok) {
        throw new Error(`Failed to fetch pools: ${res.statusText}`);
      }
      const data = await res.json();
      setPools(data.pools || []);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load pools');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchPools();
  }, [fetchPools]);

  useAutoRefresh(fetchPools, 15000);

  async function handleDelete(poolName: string) {
    setDeleteTarget(poolName);
  }

  async function confirmDelete() {
    if (!deleteTarget) return;
    const poolName = deleteTarget;
    setDeleteTarget(null);

    try {
      const res = await apiFetch(`/api/pools/${poolName}`, { method: 'DELETE' });
      if (!res.ok) {
        const data = await res.json();
        throw new Error(data.error || 'Failed to delete pool');
      }
      toast('success', `Pool "${poolName}" deleted`);
      fetchPools();
    } catch (err) {
      toast('error', err instanceof Error ? err.message : 'Failed to delete pool');
    }
  }

  return (
    <div>
      <div className="flex justify-between items-center mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Pools</h1>
        <div className="flex items-center gap-2">
          <button
            onClick={fetchPools}
            disabled={loading}
            className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-4 py-2 rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
          >
            {loading ? 'Loading...' : 'Refresh'}
          </button>
          <a
            href="/admin/pools/new/"
            className="bg-blue-600 text-white px-4 py-2 rounded-md hover:bg-blue-700 transition-colors"
          >
            Create Pool
          </a>
        </div>
      </div>

      {error && (
        <div className="mb-4 bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
          <p className="text-red-800 dark:text-red-300">{error}</p>
          <button
            onClick={fetchPools}
            className="mt-2 text-red-600 dark:text-red-400 underline hover:no-underline"
          >
            Retry
          </button>
        </div>
      )}

      {loading && pools.length === 0 ? (
        <TableSkeleton rows={4} cols={10} />
      ) : error && pools.length === 0 ? null : pools.length === 0 ? (
        <div className="text-center py-12 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
          <p className="text-gray-500 dark:text-gray-400">No pools configured yet.</p>
          <a
            href="/admin/pools/new/"
            className="mt-2 inline-block text-blue-600 dark:text-blue-400 hover:underline"
          >
            Create your first pool
          </a>
        </div>
      ) : (
        <PoolTable pools={pools} onDelete={handleDelete} />
      )}

      <ConfirmDialog
        open={!!deleteTarget}
        title="Delete Pool"
        message={`Are you sure you want to delete pool "${deleteTarget}"? This action cannot be undone.`}
        confirmLabel="Delete"
        variant="danger"
        onConfirm={confirmDelete}
        onCancel={() => setDeleteTarget(null)}
      />
    </div>
  );
}
