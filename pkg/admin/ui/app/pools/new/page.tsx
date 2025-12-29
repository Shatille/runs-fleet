'use client';

import { useRouter } from 'next/navigation';
import PoolForm from '@/components/pool-form';
import { PoolFormData } from '@/lib/types';

export default function NewPoolPage() {
  const router = useRouter();

  async function handleSubmit(data: PoolFormData) {
    const res = await fetch('/api/pools', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(data),
    });

    if (!res.ok) {
      const error = await res.json();
      throw new Error(error.details || error.error || 'Failed to create pool');
    }

    router.push('/admin/');
  }

  return (
    <div>
      <h1 className="text-2xl font-bold text-gray-900 mb-6">Create Pool</h1>
      <PoolForm onSubmit={handleSubmit} />
    </div>
  );
}
