'use client';

import { useEffect, useState } from 'react';
import PoolTable from '@/components/pool-table';
import { Pool } from '@/lib/types';
import { apiFetch } from '@/lib/api';

export default function HomePage() {
  const [pools, setPools] = useState<Pool[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

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
    if (!confirm(`Delete pool "${poolName}"?`)) return;

    try {
      const res = await apiFetch(`/api/pools/${poolName}`, { method: 'DELETE' });
      if (!res.ok) {
        const data = await res.json();
        throw new Error(data.error || 'Failed to delete pool');
      }
      fetchPools();
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Failed to delete pool');
    }
  }

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-gray-500">Loading pools...</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="bg-red-50 border border-red-200 rounded-md p-4">
        <p className="text-red-800">{error}</p>
        <button
          onClick={fetchPools}
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
        <h1 className="text-2xl font-bold text-gray-900">Pools</h1>
        <a
          href="/admin/pools/new/"
          className="bg-blue-600 text-white px-4 py-2 rounded-md hover:bg-blue-700 transition-colors"
        >
          Create Pool
        </a>
      </div>

      {pools.length === 0 ? (
        <div className="text-center py-12 bg-white rounded-lg border">
          <p className="text-gray-500">No pools configured yet.</p>
          <a
            href="/admin/pools/new/"
            className="mt-2 inline-block text-blue-600 hover:underline"
          >
            Create your first pool
          </a>
        </div>
      ) : (
        <PoolTable pools={pools} onDelete={handleDelete} />
      )}
    </div>
  );
}
