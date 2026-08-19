'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { CurrentAMIsResponse, Instance, ReplaceStaleResult } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { useToast } from '@/components/toast';
import ConfirmDialog from '@/components/confirm-dialog';

interface AMICardProps {
  instances: Instance[];
  amiUnknown: boolean;
  onReplaced: () => void;
}

// Answers "has the new AMI rolled out?" on sight. Without it the question costs
// one click per instance, which is why nobody asked it.
export default function AMICard({ instances, amiUnknown, onReplaced }: AMICardProps) {
  const { toast } = useToast();
  const [data, setData] = useState<CurrentAMIsResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [replacing, setReplacing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [result, setResult] = useState<ReplaceStaleResult | null>(null);
  // ConfirmDialog stays open on confirm and its button carries no pending
  // state, so a key repeat can fire it twice before React re-renders.
  const inFlight = useRef(false);

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

  const replace = useCallback(async (dryRun: boolean) => {
    if (inFlight.current) return;
    inFlight.current = true;
    setConfirming(false);
    setReplacing(true);
    try {
      const res = await apiFetch(`/api/instances/replace-stale?dry_run=${dryRun}`, { method: 'POST' });
      const body = await res.json().catch(() => ({}));
      if (!res.ok) {
        throw new Error(body.details || body.error || `Failed to replace stale instances: ${res.statusText}`);
      }
      setResult(body as ReplaceStaleResult);
      if (!dryRun) {
        toast('success', body.message);
        onReplaced();
      }
    } catch (err) {
      toast('error', err instanceof Error ? err.message : 'Failed to replace stale instances');
    } finally {
      inFlight.current = false;
      setReplacing(false);
    }
  }, [toast, onReplaced]);

  const staleInstances = instances.filter((i) => i.ami_stale);
  const stale = staleInstances.length;
  // An upper bound: pool claims are not exposed to the client, so the server may
  // still report some of these busy.
  const replaceable = staleInstances.filter((i) => i.state === 'stopped' && !i.busy).length;
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
              {stale > 0 && ` — ${replaceable} of the rest can be replaced now.`}
              {stale > replaceable &&
                ' The others are running or busy: they pick up the new AMI when they cycle after their next job.'}
            </p>
          )}
        </div>
        {stale > 0 && !amiUnknown && (
          <div className="flex items-center gap-2">
            <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-amber-100 dark:bg-amber-900/50 text-amber-700 dark:text-amber-300">
              {stale} stale
            </span>
            {replaceable > 0 && (
              <>
                <button
                  onClick={() => replace(true)}
                  disabled={replacing}
                  className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-3 py-1.5 text-sm rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
                >
                  {replacing ? 'Checking...' : 'Dry Run'}
                </button>
                <button
                  onClick={() => setConfirming(true)}
                  disabled={replacing}
                  className="bg-red-600 text-white px-3 py-1.5 text-sm rounded-md hover:bg-red-700 transition-colors disabled:opacity-50"
                >
                  {replacing ? 'Replacing...' : 'Replace stale'}
                </button>
              </>
            )}
          </div>
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

      {result && (
        <div className="mt-3 p-3 rounded-md text-sm bg-gray-50 dark:bg-gray-700 text-gray-700 dark:text-gray-300">
          <p>{result.message}</p>
          {result.terminated && result.terminated.length > 0 && (
            <p className="mt-1 text-xs font-mono">Replacing: {result.terminated.join(', ')}</p>
          )}
          {result.busy && result.busy.length > 0 && (
            <p className="mt-1 text-xs">
              Left alone because a job is running on them, or one is already claimed:{' '}
              <span className="font-mono">{result.busy.join(', ')}</span>
            </p>
          )}
          {result.running && result.running.length > 0 && (
            <p className="mt-1 text-xs">
              Running, so they will pick up the new AMI when they next cycle:{' '}
              <span className="font-mono">{result.running.join(', ')}</span>
            </p>
          )}
          {result.skipped && result.skipped.length > 0 && (
            <p className="mt-1 text-xs">
              Left for a later run so the pool is not drained: <span className="font-mono">{result.skipped.join(', ')}</span>
            </p>
          )}
        </div>
      )}

      <ConfirmDialog
        open={confirming}
        title="Replace stale instances"
        message={[
          'Terminate up to 5 stopped instances that are not on the current AMI, so their pools relaunch them on it.',
          'Running instances are never touched here — they pick up the new AMI when they cycle after their next job.',
          'Instances already claimed for a job, or with a job running, are skipped.',
          'Each replacement is a fresh on-demand instance; this cannot be undone.',
        ]}
        confirmLabel="Replace"
        variant="danger"
        onConfirm={() => replace(false)}
        onCancel={() => setConfirming(false)}
      />

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
