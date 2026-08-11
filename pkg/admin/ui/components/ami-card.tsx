'use client';

import { useCallback, useEffect, useState } from 'react';
import { CurrentAMIsResponse, Instance } from '@/lib/types';
import { apiFetch } from '@/lib/api';

interface AMICardProps {
  instances: Instance[];
  amiUnknown: boolean;
}

// Answers "has the new AMI rolled out?" on sight. Without it the question costs
// one click per instance, which is why nobody asked it.
export default function AMICard({ instances, amiUnknown }: AMICardProps) {
  const [data, setData] = useState<CurrentAMIsResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await apiFetch('/api/instances/amis');
      const body = await res.json().catch(() => ({}));
      if (!res.ok) {
        throw new Error(body.details || body.error || `Failed to read launch templates: ${res.statusText}`);
      }
      setError(null);
      setData(body as CurrentAMIsResponse);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to read launch templates');
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const stale = instances.filter((i) => i.ami_stale).length;
  const total = instances.length;

  return (
    <div className="mb-4 p-4 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">Runner AMI</h3>
          {amiUnknown || error ? (
            <p className="text-xs text-gray-500 dark:text-gray-400">
              The current AMI could not be read, so no instance is marked stale.
              {error ? ` ${error}` : ''}
            </p>
          ) : (
            <p className="text-xs text-gray-500 dark:text-gray-400">
              {total - stale} of {total} on the current AMI
              {stale > 0 && ' — the rest are stopped or stuck, and will not pick it up on their own.'}
            </p>
          )}
        </div>
        {stale > 0 && !amiUnknown && (
          <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-amber-100 dark:bg-amber-900/50 text-amber-700 dark:text-amber-300">
            {stale} stale
          </span>
        )}
      </div>

      {data && data.amis.length > 0 && (
        <dl className="mt-3 grid grid-cols-1 sm:grid-cols-2 gap-3">
          {data.amis.map((ami) => (
            <div key={ami.architecture} className="rounded-md bg-gray-50 dark:bg-gray-700/50 p-3">
              <dt className="text-xs font-medium text-gray-500 dark:text-gray-400">{ami.architecture}</dt>
              <dd className="font-mono text-sm text-gray-900 dark:text-gray-100">{ami.image_id}</dd>
              <dd className="text-xs text-gray-500 dark:text-gray-400">
                {ami.launch_template} v{ami.version}
                {ami.version_created && ` · ${formatWhen(ami.version_created)}`}
              </dd>
            </div>
          ))}
        </dl>
      )}

      {data && data.unresolved && data.unresolved.length > 0 && (
        <p className="mt-2 text-xs text-amber-700 dark:text-amber-400">
          Could not read the template for: {data.unresolved.join(', ')}. Instances of{' '}
          {data.unresolved.length === 1 ? 'that architecture are' : 'those architectures are'} not marked stale.
        </p>
      )}
    </div>
  );
}

function formatWhen(iso: string): string {
  const date = new Date(iso);
  const mins = Math.floor((Date.now() - date.getTime()) / 60000);
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}
