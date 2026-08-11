'use client';

import { useRef, useState } from 'react';
import { apiFetch } from '@/lib/api';
import { useToast } from '@/components/toast';
import ConfirmDialog from '@/components/confirm-dialog';

// Statuses a job can be stuck in. Anything else has settled and there is nothing
// left to re-drive. running and claiming are included because the hang that
// strands a job for hours leaves a record reading running; the server refuses
// those unless GitHub confirms the job is still queued.
const REQUEUEABLE = ['launched', 'running', 'claiming'];
const RECONCILABLE = ['launched', 'running', 'claiming'];

type Action = 'requeue' | 'reconcile';

// A runner normally registers within this window, so a job younger than it may
// simply still be booting rather than hung. The fleet-wide sweep enforces an age
// floor for exactly this reason; a per-row action trusts the operator instead, so
// the age has to be in front of them at the moment they confirm.
const CONFIRM_WINDOW_MS = 5 * 60 * 1000;

interface JobActionsProps {
  jobId: number;
  status: string;
  instanceId?: string;
  createdAt?: string;
  onActed: () => void;
}

export default function JobActions({ jobId, status, instanceId, createdAt, onActed }: JobActionsProps) {
  const { toast } = useToast();
  const [confirming, setConfirming] = useState<Action | null>(null);
  const [pending, setPending] = useState<Action | null>(null);
  const [exhausted, setExhausted] = useState<string | null>(null);
  // ConfirmDialog stays open on confirm and its button carries no pending state,
  // so a key repeat can fire it twice before React re-renders. A ref rejects the
  // second call in the same tick, which setPending cannot.
  const inFlight = useRef(false);

  const state = status?.toLowerCase() || '';
  const canRequeue = REQUEUEABLE.includes(state);
  const canReconcile = RECONCILABLE.includes(state);
  if (!canRequeue && !canReconcile) return null;

  async function run(action: Action, force = false) {
    if (inFlight.current) return;
    inFlight.current = true;
    setConfirming(null);
    setPending(action);
    try {
      const query = force ? '?force=true' : '';
      const res = await apiFetch(`/api/jobs/${encodeURIComponent(jobId)}/${action}${query}`, { method: 'POST' });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        // The retry cap is the one refusal an operator can act on, so offer the
        // override instead of leaving them to guess it exists. Only requeue has
        // a cap; confirming a force here must never stand in for a reconcile.
        if (action === 'requeue' && data.outcome === 'exhausted' && !force) {
          setExhausted(data.details || `Job ${jobId} has spent its requeue retries.`);
          return;
        }
        throw new Error(data.details || data.error || `Failed to ${action} job ${jobId}`);
      }
      toast('success', data.message || `Job ${jobId} ${action}d`);
      onActed();
    } catch (err) {
      toast('error', err instanceof Error ? err.message : `Failed to ${action} job ${jobId}`);
    } finally {
      inFlight.current = false;
      setPending(null);
    }
  }

  const instanceNote = instanceId
    ? ` Its instance ${instanceId} is terminated first if it is still alive.`
    : '';

  // A launched record has no confirmed runner; anything past it might be doing
  // real work, and the server will refuse unless GitHub says otherwise.
  const gateNote = state === 'launched'
    ? ''
    : ' This is refused unless GitHub confirms the job is still queued.';

  // A negative age means the browser clock trails the server's; reporting "1s ago"
  // off that would be a fabricated warning, so an unusable age says nothing at all.
  const ageMs = createdAt ? Date.now() - new Date(createdAt).getTime() : NaN;
  const ageNote = Number.isNaN(ageMs) || ageMs < 0
    ? ''
    : ageMs < CONFIRM_WINDOW_MS
      ? ` It was launched ${Math.max(1, Math.round(ageMs / 1000))}s ago — a runner normally registers within 5 minutes, so it may still be booting.`
      : ` It was launched ${Math.round(ageMs / 60000)} minutes ago.`;

  return (
    <span className="flex gap-3 justify-end">
      {canRequeue && (
        <button
          onClick={() => setConfirming('requeue')}
          disabled={pending !== null}
          className="text-blue-600 dark:text-blue-400 hover:text-blue-900 dark:hover:text-blue-300 disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {pending === 'requeue' ? 'Requeuing...' : 'Requeue'}
        </button>
      )}
      {canReconcile && (
        <button
          onClick={() => setConfirming('reconcile')}
          disabled={pending !== null}
          className="text-gray-600 dark:text-gray-400 hover:text-gray-900 dark:hover:text-gray-200 disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {pending === 'reconcile' ? 'Reconciling...' : 'Reconcile'}
        </button>
      )}

      <ConfirmDialog
        open={confirming === 'requeue'}
        title="Requeue Job"
        message={`Re-dispatch a fresh runner for job ${jobId}?${ageNote}${instanceNote}${gateNote} The GitHub job stays queued — it is not cancelled or re-run.`}
        confirmLabel="Requeue"
        onConfirm={() => run('requeue')}
        onCancel={() => setConfirming(null)}
      />

      <ConfirmDialog
        open={exhausted !== null}
        title="Requeue anyway?"
        message={`${exhausted ?? ''} Forcing ignores that cap and re-dispatches job ${jobId} once more. It cannot re-dispatch a job GitHub is actually running — that check still applies.`}
        confirmLabel="Force requeue"
        onConfirm={() => { setExhausted(null); void run('requeue', true); }}
        onCancel={() => setExhausted(null)}
      />

      <ConfirmDialog
        open={confirming === 'reconcile'}
        title="Reconcile Job"
        message={`Mark job ${jobId} orphaned if its instance is gone? This only retires a stale record; it is refused while the instance is still running.`}
        confirmLabel="Reconcile"
        onConfirm={() => run('reconcile')}
        onCancel={() => setConfirming(null)}
      />
    </span>
  );
}
