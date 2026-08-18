'use client';

import { useCallback, useState } from 'react';
import { JobLogsResponse } from '@/lib/types';
import { apiFetch } from '@/lib/api';

interface JobLogsCardProps {
  jobId: number;
}

// Loads on click, not on mount: each fetch is an audited read of logs that may
// carry secret material, so opening a job page must not record an access.
export default function JobLogsCard({ jobId }: JobLogsCardProps) {
  const [data, setData] = useState<JobLogsResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const res = await apiFetch(`/api/jobs/${jobId}/logs`);
      const body = await res.json().catch(() => ({}));
      if (res.status === 503) {
        setUnavailable(body.details || 'This deployment does not keep runner logs.');
        setError(null);
        setData(null);
        return;
      }
      if (!res.ok) {
        throw new Error(body.details || body.error || `Failed to load runner logs: ${res.statusText}`);
      }
      setUnavailable(null);
      setError(null);
      setData(body as JobLogsResponse);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load runner logs');
    } finally {
      setLoading(false);
    }
  }, [jobId]);

  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700 p-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="font-semibold text-gray-900 dark:text-gray-100">Runner logs</h2>
          <p className="text-sm text-gray-500 dark:text-gray-400">
            The instance&apos;s own logs, kept after GitHub expires the job&apos;s.
          </p>
        </div>
        <button
          onClick={() => void load()}
          disabled={loading}
          className="px-3 py-1.5 text-sm rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50"
        >
          {loading ? 'Loading…' : data ? 'Refresh links' : 'Load runner logs'}
        </button>
      </div>

      {unavailable && (
        <p className="mt-3 text-sm text-gray-500 dark:text-gray-400">{unavailable}</p>
      )}

      {error && (
        <p className="mt-3 text-sm text-red-600 dark:text-red-400">{error}</p>
      )}

      {data && data.logs.length === 0 && (
        <p className="mt-3 text-sm text-gray-500 dark:text-gray-400">
          No runner logs stored for this job. They may have passed their retention window, or the
          agent could not upload them — check the job&apos;s log-upload outcome.
        </p>
      )}

      {data && data.logs.length > 0 && (
        <>
          <ul className="mt-3 divide-y dark:divide-gray-700">
            {data.logs.map((log) => (
              <li key={log.name} className="py-2 flex items-center justify-between gap-4">
                <div className="min-w-0">
                  <a
                    href={log.url}
                    className="text-sm text-blue-600 dark:text-blue-400 hover:underline break-all"
                  >
                    {log.name}
                  </a>
                  <p className="text-xs text-gray-500 dark:text-gray-400">
                    {formatBytes(log.size)} · {new Date(log.last_modified).toLocaleString()}
                  </p>
                </div>
              </li>
            ))}
          </ul>
          <p className="mt-3 text-xs text-gray-500 dark:text-gray-400">
            Links expire in {Math.round(data.expires_in_seconds / 60)} minutes. These logs are not
            secret-masked — do not paste them into issues or pull requests.
          </p>
        </>
      )}
    </div>
  );
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
