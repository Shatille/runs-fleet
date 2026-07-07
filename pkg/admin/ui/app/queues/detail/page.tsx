'use client';

import { Suspense, useEffect, useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { DetailSkeleton } from '@/components/skeleton';
import { QueueStatus } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { formatDuration } from '@/lib/format';

export default function QueueDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <QueueDetailView />
    </Suspense>
  );
}

function QueueDetailView() {
  const searchParams = useSearchParams();
  const name = searchParams.get('name');

  const [queue, setQueue] = useState<QueueStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!name) {
      setError('No queue name specified');
      setLoading(false);
      return;
    }
    let cancelled = false;
    async function fetchQueue() {
      try {
        setLoading(true);
        const res = await apiFetch(`/api/queues/${encodeURIComponent(name as string)}`);
        if (cancelled) return;
        if (res.status === 404) {
          setError('Queue not found');
          return;
        }
        if (!res.ok) {
          throw new Error(`Failed to fetch queue: ${res.statusText}`);
        }
        const data = await res.json();
        if (cancelled) return;
        setQueue(data);
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : 'Failed to load queue');
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    }
    void fetchQueue();
    return () => {
      cancelled = true;
    };
  }, [name]);

  if (loading) return <DetailSkeleton />;

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <a href="/admin/queues/" className="mt-2 inline-block text-blue-600 dark:text-blue-400 hover:underline">
          Back to Queues
        </a>
      </div>
    );
  }

  if (!queue) return null;

  return (
    <div>
      <div className="mb-6">
        <a href="/admin/queues/" className="text-blue-600 dark:text-blue-400 hover:underline text-sm">
          &larr; Back to Queues
        </a>
      </div>

      <div className="flex items-center gap-4 mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100 capitalize">{queue.name}</h1>
        {queue.dlq_messages > 0 && (
          <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-red-100 dark:bg-red-900/50 text-red-800 dark:text-red-300">
            {queue.dlq_messages} in DLQ
          </span>
        )}
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 mb-6">
        <StatCard label="Visible" value={queue.messages_visible} highlight={queue.messages_visible > 0} />
        <StatCard label="In Flight" value={queue.messages_in_flight} highlight={queue.messages_in_flight > 0} />
        <StatCard label="Delayed" value={queue.messages_delayed} />
        <StatCard label="In DLQ" value={queue.dlq_messages} highlight={queue.dlq_messages > 0} />
        <StatCardText label="Oldest Message" value={queue.oldest_message_age_seconds ? formatDuration(queue.oldest_message_age_seconds) : '-'} />
      </div>

      <div className="bg-white dark:bg-gray-800 shadow rounded-lg p-6">
        <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">Queue</h2>
        <dl className="space-y-3">
          <div className="flex justify-between gap-4">
            <dt className="text-sm font-medium text-gray-500 dark:text-gray-400">URL</dt>
            <dd className="text-sm font-mono text-gray-900 dark:text-gray-100 truncate" title={queue.url}>{queue.url}</dd>
          </div>
        </dl>
      </div>
    </div>
  );
}

function StatCard({ label, value, highlight }: { label: string; value: number; highlight?: boolean }) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 p-4">
      <div className="text-sm font-medium text-gray-500 dark:text-gray-400">{label}</div>
      <div className={`mt-1 text-2xl font-semibold ${highlight ? 'text-yellow-600 dark:text-yellow-400' : 'text-gray-900 dark:text-gray-100'}`}>
        {value}
      </div>
    </div>
  );
}

function StatCardText({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 p-4">
      <div className="text-sm font-medium text-gray-500 dark:text-gray-400">{label}</div>
      <div className="mt-1 text-2xl font-semibold text-gray-900 dark:text-gray-100">{value}</div>
    </div>
  );
}
