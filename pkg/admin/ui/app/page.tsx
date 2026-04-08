'use client';

import { useEffect, useState } from 'react';
import PoolTable from '@/components/pool-table';
import { TableSkeleton } from '@/components/skeleton';
import ConfirmDialog from '@/components/confirm-dialog';
import { useToast } from '@/components/toast';
import { Pool } from '@/lib/types';
import { apiFetch } from '@/lib/api';

export default function HomePage() {
  const [pools, setPools] = useState<Pool[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const { toast } = useToast();

  useEffect(() => {
    fetchPools();
  }, []);

  async function fetchPools() {
    try {
      setLoading(true);
      const res = await apiFetch('/api/pools');
      if (!res.ok) {
        throw new Error(`Failed to fetch pools: ${res.statusText}`);
      }
      const data = await res.json();
      setPools(data.pools || []);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load pools');
    } finally {
      setLoading(false);
    }
  }

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

  if (loading) {
    return (
      <div>
        <div className="flex justify-between items-center mb-6">
          <div className="h-8 w-32 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
          <div className="h-10 w-28 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
        </div>
        <TableSkeleton rows={4} cols={10} />
      </div>
    );
  }

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <button
          onClick={fetchPools}
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
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Pools</h1>
        <a
          href="/admin/pools/new/"
          className="bg-blue-600 text-white px-4 py-2 rounded-md hover:bg-blue-700 transition-colors"
        >
          Create Pool
        </a>
      </div>

      {pools.length === 0 ? (
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
