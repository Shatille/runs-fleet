'use client';

import { useRef, useState } from 'react';
import { apiFetch } from '@/lib/api';
import { ActiveJobRef } from '@/lib/types';
import { useToast } from '@/components/toast';
import ConfirmDialog from '@/components/confirm-dialog';
import { isTerminableState } from '@/lib/instance-state';

interface TerminateInstanceButtonProps {
  instanceId: string;
  pool?: string;
  busy?: boolean;
  state: string;
  onTerminated: () => void;
  className?: string;
}

export default function TerminateInstanceButton({
  instanceId,
  pool,
  busy,
  state,
  onTerminated,
  className,
}: TerminateInstanceButtonProps) {
  const { toast } = useToast();
  const [step, setStep] = useState<'idle' | 'confirm' | 'force'>('idle');
  const [pending, setPending] = useState(false);
  const [blockingJob, setBlockingJob] = useState<ActiveJobRef | null>(null);
  // ConfirmDialog does not close on confirm and its button carries no pending
  // state, so a key repeat can fire it twice before React re-renders. A ref
  // rejects the second call in the same tick, which setPending cannot.
  const inFlight = useRef(false);

  if (!isTerminableState(state)) return null;

  async function terminate(force: boolean) {
    if (inFlight.current) return;
    inFlight.current = true;
    setStep('idle');
    setPending(true);
    try {
      const res = await apiFetch(
        `/api/instances/${encodeURIComponent(instanceId)}${force ? '?force=true' : ''}`,
        { method: 'DELETE' }
      );
      const data = await res.json().catch(() => ({}));

      // 409 means the instance is still serving a job. Re-confirm with the job
      // named rather than silently escalating to force.
      if (res.status === 409 && data.active_job) {
        setBlockingJob(data.active_job);
        setStep('force');
        return;
      }
      if (!res.ok) {
        throw new Error(data.details || data.error || `Failed to terminate ${instanceId}`);
      }

      toast('success', data.message || `Termination requested for ${instanceId}`);
      onTerminated();
    } catch (err) {
      toast('error', err instanceof Error ? err.message : `Failed to terminate ${instanceId}`);
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  const busyNote = busy ? ' It is currently marked busy.' : '';
  const poolNote = pool ? ` It belongs to pool "${pool}", which will launch a replacement.` : '';

  return (
    <>
      <button
        onClick={() => setStep('confirm')}
        disabled={pending}
        className={
          className ??
          'text-red-600 dark:text-red-400 hover:text-red-900 dark:hover:text-red-300 disabled:opacity-50 disabled:cursor-not-allowed'
        }
      >
        {pending ? 'Terminating...' : 'Terminate'}
      </button>

      <ConfirmDialog
        open={step === 'confirm'}
        title="Terminate Instance"
        message={`Terminate ${instanceId}?${busyNote}${poolNote} This cannot be undone.`}
        confirmLabel="Terminate"
        variant="danger"
        onConfirm={() => terminate(false)}
        onCancel={() => setStep('idle')}
      />

      <ConfirmDialog
        open={step === 'force'}
        title="Instance is running a job"
        message={
          blockingJob
            ? `${instanceId} is running job ${blockingJob.job_id} (run ${blockingJob.run_id}) in ${blockingJob.repo}. Terminating now fails that job on GitHub — it will not be re-run automatically. Terminate anyway?`
            : `${instanceId} is running a job. Terminate anyway?`
        }
        confirmLabel="Terminate anyway"
        variant="danger"
        onConfirm={() => terminate(true)}
        onCancel={() => {
          setStep('idle');
          setBlockingJob(null);
        }}
      />
    </>
  );
}
