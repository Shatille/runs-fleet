'use client';

import { useEffect, useState } from 'react';
import { useRouter, useParams } from 'next/navigation';
import PoolForm from '@/components/pool-form';
import { Pool, PoolFormData } from '@/lib/types';

export default function EditPoolPage() {
  const router = useRouter();
  const params = useParams();
  const poolName = params.name as string;

  const [pool, setPool] = useState<Pool | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    async function fetchPool() {
      try {
        const res = await fetch(`/api/pools/${poolName}`);
        if (!res.ok) {
          if (res.status === 404) {
            throw new Error('Pool not found');
          }
          throw new Error('Failed to fetch pool');
        }
        const data = await res.json();
        setPool(data);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to load pool');
      } finally {
        setLoading(false);
      }
    }

    fetchPool();
  }, [poolName]);

  async function handleSubmit(data: PoolFormData) {
    const res = await fetch(`/api/pools/${poolName}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data),
    });

    if (!res.ok) {
      const error = await res.json();
      throw new Error(error.details || error.error || 'Failed to update pool');
    }

    router.push('/admin/');
  }

  if (loading) {
    return (
      <div className="flex items-center justify-center h-64">
        <div className="text-gray-500">Loading pool...</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="bg-red-50 border border-red-200 rounded-md p-4">
        <p className="text-red-800">{error}</p>
        <a href="/admin/" className="mt-2 inline-block text-blue-600 hover:underline">
          Back to pools
        </a>
      </div>
    );
  }

  if (!pool) {
    return null;
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-2xl font-bold text-gray-900">Edit Pool: {pool.pool_name}</h1>
        {pool.ephemeral && (
          <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-yellow-100 text-yellow-800">
            Ephemeral
          </span>
        )}
      </div>
      <PoolForm pool={pool} onSubmit={handleSubmit} isEdit />
    </div>
  );
}
