'use client';

import { Suspense, useEffect, useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { DetailSkeleton } from '@/components/skeleton';
import { Job } from '@/lib/types';
import { apiFetch } from '@/lib/api';

export default function JobDetailPage() {
  return (
    <Suspense fallback={
      <div>
        <div className="h-4 w-20 animate-pulse rounded bg-gray-200 dark:bg-gray-700 mb-6" />
        <div className="h-8 w-32 animate-pulse rounded bg-gray-200 dark:bg-gray-700 mb-6" />
        <DetailSkeleton />
      </div>
    }>
      <JobDetail />
    </Suspense>
  );
}

function JobDetail() {
  const searchParams = useSearchParams();
  const id = searchParams.get('id');

  const [job, setJob] = useState<Job | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!id) {
      setError('No job ID specified');
      setLoading(false);
      return;
    }

    const jobId = id;
    async function fetchJob() {
      try {
        setLoading(true);
        const res = await apiFetch(`/api/jobs/${encodeURIComponent(jobId)}`);
        if (res.status === 404) {
          setError('Job not found');
          return;
        }
        if (!res.ok) {
          throw new Error(`Failed to fetch job: ${res.statusText}`);
        }
        const data = await res.json();
        setJob(data);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to load job');
      } finally {
        setLoading(false);
      }
    }

    fetchJob();
  }, [id]);

  if (loading) {
    return (
      <div>
        <div className="h-4 w-20 animate-pulse rounded bg-gray-200 dark:bg-gray-700 mb-6" />
        <div className="h-8 w-32 animate-pulse rounded bg-gray-200 dark:bg-gray-700 mb-6" />
        <DetailSkeleton />
      </div>
    );
  }

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <a
          href="/admin/jobs/"
          className="mt-2 inline-block text-blue-600 dark:text-blue-400 hover:underline"
        >
          Back to Jobs
        </a>
      </div>
    );
  }

  if (!job) return null;

  const githubRunUrl =
    job.repo && job.run_id
      ? `https://github.com/${job.repo}/actions/runs/${job.run_id}`
      : null;

  return (
    <div>
      <div className="mb-6">
        <a
          href="/admin/jobs/"
          className="text-blue-600 dark:text-blue-400 hover:underline text-sm"
        >
          &larr; Back to Jobs
        </a>
      </div>

      <div className="flex items-center gap-4 mb-6">
        <h1 className="text-2xl font-bold text-gray-900 dark:text-gray-100">
          Job {job.job_id}
        </h1>
        <StatusBadge status={job.status} exitCode={job.exit_code} />
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        <Section title="Identification">
          <Field label="Job ID" value={String(job.job_id)} mono />
          <Field label="Run ID">
            {githubRunUrl ? (
              <a
                href={githubRunUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="text-blue-600 dark:text-blue-400 hover:underline font-mono text-sm"
              >
                {job.run_id}
              </a>
            ) : (
              <span className="font-mono text-sm text-gray-900 dark:text-gray-100">
                {job.run_id || '-'}
              </span>
            )}
          </Field>
          <Field label="Repository" value={job.repo || '-'} />
          <Field label="Pool" value={job.pool || '-'} />
        </Section>

        <Section title="Instance">
          <Field label="Instance ID" value={job.instance_id || '-'} mono />
          <Field label="Instance Type" value={job.instance_type || '-'} mono />
          <Field label="Spot">
            {job.spot ? (
              <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-orange-100 dark:bg-orange-900/50 text-orange-700 dark:text-orange-300">
                Spot
              </span>
            ) : (
              <span className="text-sm text-gray-500 dark:text-gray-400">On-Demand</span>
            )}
          </Field>
          <Field label="Warm Pool Hit">
            {job.warm_pool_hit ? (
              <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-green-100 dark:bg-green-900/50 text-green-700 dark:text-green-300">
                Yes
              </span>
            ) : (
              <span className="text-sm text-gray-500 dark:text-gray-400">No</span>
            )}
          </Field>
          <Field
            label="Spot Request ID"
            value={job.spot_request_id || '-'}
            mono
          />
        </Section>

        <Section title="Execution">
          <Field label="Status" value={job.status} />
          <Field label="Exit Code" value={job.exit_code !== undefined ? String(job.exit_code) : '-'} mono />
          <Field label="Retry Count" value={String(job.retry_count)} />
          <Field label="Duration" value={formatDuration(job.duration_seconds)} />
        </Section>

        <Section title="Timestamps">
          <Field label="Created" value={formatTimestamp(job.created_at)} />
          <Field label="Started" value={formatTimestamp(job.started_at)} />
          <Field label="Completed" value={formatTimestamp(job.completed_at)} />
        </Section>
      </div>
    </div>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="bg-white dark:bg-gray-800 shadow rounded-lg p-6">
      <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{title}</h2>
      <dl className="space-y-3">{children}</dl>
    </div>
  );
}

function Field({
  label,
  value,
  mono,
  children,
}: {
  label: string;
  value?: string;
  mono?: boolean;
  children?: React.ReactNode;
}) {
  return (
    <div className="flex justify-between items-center">
      <dt className="text-sm font-medium text-gray-500 dark:text-gray-400">{label}</dt>
      <dd>
        {children || (
          <span
            className={`text-sm text-gray-900 dark:text-gray-100 ${mono ? 'font-mono' : ''}`}
          >
            {value}
          </span>
        )}
      </dd>
    </div>
  );
}

function StatusBadge({
  status,
  exitCode,
}: {
  status: string;
  exitCode?: number;
}) {
  const statusStyles: Record<string, string> = {
    pending: 'bg-gray-100 dark:bg-gray-700 text-gray-800 dark:text-gray-300',
    queued: 'bg-blue-100 dark:bg-blue-900/50 text-blue-800 dark:text-blue-300',
    running: 'bg-yellow-100 dark:bg-yellow-900/50 text-yellow-800 dark:text-yellow-300',
    completed: 'bg-green-100 dark:bg-green-900/50 text-green-800 dark:text-green-300',
    failed: 'bg-red-100 dark:bg-red-900/50 text-red-800 dark:text-red-300',
    terminated: 'bg-red-100 dark:bg-red-900/50 text-red-800 dark:text-red-300',
    requeued: 'bg-orange-100 dark:bg-orange-900/50 text-orange-800 dark:text-orange-300',
  };

  const style =
    statusStyles[status.toLowerCase()] || 'bg-gray-100 dark:bg-gray-700 text-gray-800 dark:text-gray-300';

  return (
    <span
      className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${style}`}
    >
      {status}
      {exitCode !== undefined && exitCode !== 0 && (
        <span className="ml-1 opacity-75">({exitCode})</span>
      )}
    </span>
  );
}

function formatDuration(seconds?: number): string {
  if (seconds == null) return '-';
  if (seconds < 60) return `${seconds}s`;
  const mins = Math.floor(seconds / 60);
  const secs = seconds % 60;
  if (mins < 60) return `${mins}m ${secs}s`;
  const hours = Math.floor(mins / 60);
  const remainingMins = mins % 60;
  return `${hours}h ${remainingMins}m`;
}

function formatTimestamp(isoString?: string): string {
  if (!isoString) return '-';
  const date = new Date(isoString);
  return date.toLocaleString();
}
