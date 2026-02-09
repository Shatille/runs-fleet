'use client';

import { useEffect, useState, useCallback } from 'react';
import { QueueStatus } from '@/lib/types';
import { apiFetch } from '@/lib/api';

export default function QueuesPage() {
  const [queues, setQueues] = useState<QueueStatus[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchQueues = useCallback(async () => {
    try {
      setLoading(true);
      const res = await apiFetch('/api/queues');
      if (!res.ok) {
        throw new Error(`Failed to fetch queues: ${res.statusText}`);
      }
      const data = await res.json();
      setQueues(data.queues || []);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load queues');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchQueues();
    const interval = setInterval(fetchQueues, 10000);
    return () => clearInterval(interval);
  }, [fetchQueues]);

  if (error) {
    return (
      <div className="bg-red-50 border border-red-200 rounded-md p-4">
        <p className="text-red-800">{error}</p>
        <button
          onClick={fetchQueues}
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
        <h1 className="text-2xl font-bold text-gray-900">Queue Status</h1>
        <button
          onClick={fetchQueues}
          disabled={loading}
          className="bg-gray-100 text-gray-700 px-4 py-2 rounded-md hover:bg-gray-200 transition-colors disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Refresh'}
        </button>
      </div>

      {loading && queues.length === 0 ? (
        <div className="flex items-center justify-center h-64">
          <div className="text-gray-500">Loading queues...</div>
        </div>
      ) : queues.length === 0 ? (
        <div className="text-center py-12 bg-white rounded-lg border">
          <p className="text-gray-500">No queues configured.</p>
        </div>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {queues.map((queue) => (
            <QueueCard key={queue.name} queue={queue} />
          ))}
        </div>
      )}
    </div>
  );
}

interface QueueCardProps {
  queue: QueueStatus;
}

function QueueCard({ queue }: QueueCardProps) {
  const totalMessages = queue.messages_visible + queue.messages_in_flight + queue.messages_delayed;
  const hasMessages = totalMessages > 0 || queue.dlq_messages > 0;

  return (
    <div className={`bg-white rounded-lg shadow p-6 ${hasMessages ? 'ring-2 ring-yellow-200' : ''}`}>
      <div className="flex items-center justify-between mb-4">
        <h3 className="text-lg font-semibold text-gray-900 capitalize">{queue.name}</h3>
        {queue.dlq_messages > 0 && (
          <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-red-100 text-red-800">
            {queue.dlq_messages} in DLQ
          </span>
        )}
      </div>

      <div className="space-y-3">
        <StatRow
          label="Visible"
          value={queue.messages_visible}
          description="Messages available for processing"
          highlight={queue.messages_visible > 0}
        />
        <StatRow
          label="In Flight"
          value={queue.messages_in_flight}
          description="Messages being processed"
          highlight={queue.messages_in_flight > 0}
        />
        <StatRow
          label="Delayed"
          value={queue.messages_delayed}
          description="Messages waiting for delay to expire"
        />
      </div>

      <div className="mt-4 pt-4 border-t">
        <div className="text-xs text-gray-400 truncate" title={queue.url}>
          {queue.url.replace(/https:\/\/sqs\.[^/]+\//, '')}
        </div>
      </div>
    </div>
  );
}

interface StatRowProps {
  label: string;
  value: number;
  description: string;
  highlight?: boolean;
}

function StatRow({ label, value, description, highlight }: StatRowProps) {
  return (
    <div className="flex justify-between items-center">
      <div>
        <span className={`text-sm font-medium ${highlight ? 'text-yellow-700' : 'text-gray-700'}`}>
          {label}
        </span>
        <span className="hidden sm:inline text-xs text-gray-400 ml-2">{description}</span>
      </div>
      <span className={`text-lg font-bold ${highlight ? 'text-yellow-600' : 'text-gray-900'}`}>
        {value}
      </span>
    </div>
  );
}
