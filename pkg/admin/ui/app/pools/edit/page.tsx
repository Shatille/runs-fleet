'use client';

import { Suspense, useEffect, useState } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import PoolForm from '@/components/pool-form';
import { FormSkeleton } from '@/components/skeleton';
import { Pool, PoolFormData } from '@/lib/types';
import { apiFetch } from '@/lib/api';

function EditPoolContent() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const poolName = searchParams.get('name');

  const [pool, setPool] = useState<Pool | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!poolName) {
      setError('Pool name is required');
      setLoading(false);
      return;
    }

    async function fetchPool() {
      try {
        const res = await apiFetch(`/api/pools/${poolName}`);
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
    if (!poolName) return;

    const res = await apiFetch(`/api/pools/${poolName}`, {
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
      <div>
        <div className="h-8 w-48 animate-pulse rounded bg-gray-200 dark:bg-gray-700 mb-6" />
        <FormSkeleton />
      </div>
    );
  }

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <a href="/admin/" className="mt-2 inline-block text-blue-600 dark:text-blue-400 hover:underline">
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
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">Edit Pool: {pool.pool_name}</h1>
        {pool.ephemeral && (
          <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-yellow-100 dark:bg-yellow-900/50 text-yellow-800 dark:text-yellow-300">
            Ephemeral
          </span>
        )}
      </div>
      <PoolForm pool={pool} onSubmit={handleSubmit} isEdit />
    </div>
  );
}

export default function EditPoolPage() {
  return (
    <Suspense fallback={
      <div>
        <div className="h-8 w-48 animate-pulse rounded bg-gray-200 dark:bg-gray-700 mb-6" />
        <FormSkeleton />
      </div>
    }>
      <EditPoolContent />
    </Suspense>
  );
}
