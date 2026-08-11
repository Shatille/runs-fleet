'use client';

import { useCallback, useEffect, useState } from 'react';
import { HungClassification, HungJob, HungJobsResponse } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { formatDuration } from '@/lib/format';
import { useToast } from '@/components/toast';
import JobActions from '@/components/job-actions';

const CLASSIFICATION_STYLE: Record<HungClassification, string> = {
  hung: 'bg-red-100 dark:bg-red-900/50 text-red-800 dark:text-red-300',
  running: 'bg-green-100 dark:bg-green-900/50 text-green-800 dark:text-green-300',
  completed_upstream: 'bg-blue-100 dark:bg-blue-900/50 text-blue-800 dark:text-blue-300',
  unknown: 'bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300',
};

const CLASSIFICATION_LABEL: Record<HungClassification, string> = {
  hung: 'hung',
  running: 'running',
  completed_upstream: 'done upstream',
  unknown: 'unknown',
};

const CLASSIFICATION_HELP: Record<HungClassification, string> = {
  hung: 'GitHub still has this job queued, so nothing is running it — the runner provisioned for it never took it.',
  running: 'GitHub reports the job in progress. It is a long build, not a hang.',
  completed_upstream: 'The job finished at GitHub; only our record is behind.',
  unknown: 'GitHub could not be asked about this job.',
};

interface HungJobsCardProps {
  onActed: () => void;
}

// Age alone cannot tell a hang from a long build, so this card asks GitHub and
// shows the verdict. It is the only view in the console that can distinguish
// them — every other view reads our own record, which says "running" either way.
export default function HungJobsCard({ onActed }: HungJobsCardProps) {
  const { toast } = useToast();
  const [data, setData] = useState<HungJobsResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [staleMinutes, setStaleMinutes] = useState(15);

  const load = useCallback(async (minutes: number) => {
    setLoading(true);
    try {
      const res = await apiFetch(`/api/jobs/hung?stale_minutes=${minutes}`);
      const body = await res.json().catch(() => ({}));
      if (!res.ok) {
        throw new Error(body.details || body.error || `Failed to load hung jobs: ${res.statusText}`);
      }
      setData(body as HungJobsResponse);
    } catch (err) {
      toast('error', err instanceof Error ? err.message : 'Failed to load hung jobs');
    } finally {
      setLoading(false);
    }
  }, [toast]);

  useEffect(() => {
    void load(staleMinutes);
  }, [load, staleMinutes]);

  const hungCount = data?.jobs.filter((j) => j.classification === 'hung').length ?? 0;

  return (
    <div className="mb-4 p-4 bg-white dark:bg-gray-800 rounded-lg border dark:border-gray-700">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-sm font-medium text-gray-900 dark:text-gray-100">
            Hung Jobs
            {hungCount > 0 && (
              <span className="ml-2 inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-red-100 dark:bg-red-900/50 text-red-800 dark:text-red-300">
                {hungCount}
              </span>
            )}
          </h3>
          <p className="text-xs text-gray-500 dark:text-gray-400">
            Open records older than the window, each checked against GitHub. Only GitHub can tell a hang from a long build.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <label className="text-xs text-gray-500 dark:text-gray-400" htmlFor="hung-window">
            Older than
          </label>
          <select
            id="hung-window"
            value={staleMinutes}
            onChange={(e) => setStaleMinutes(Number(e.target.value))}
            className="text-sm rounded-md border-gray-300 dark:border-gray-600 dark:bg-gray-700 dark:text-gray-200 px-2 py-1"
          >
            <option value={15}>15m</option>
            <option value={30}>30m</option>
            <option value={60}>1h</option>
            <option value={360}>6h</option>
          </select>
          <button
            onClick={() => load(staleMinutes)}
            disabled={loading}
            className="bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 px-3 py-1.5 text-sm rounded-md hover:bg-gray-200 dark:hover:bg-gray-600 transition-colors disabled:opacity-50"
          >
            {loading ? 'Checking...' : 'Refresh'}
          </button>
        </div>
      </div>

      {data && !data.github_available && (
        <p className="mt-3 p-3 rounded-md text-sm bg-yellow-50 dark:bg-yellow-900/30 text-yellow-800 dark:text-yellow-300">
          No GitHub client is configured, so these are age-based suspicions only — none of them has been verified.
        </p>
      )}

      {data && data.truncated && (
        <p className="mt-3 text-xs text-gray-500 dark:text-gray-400">
          Showing the {data.checked} oldest of {data.candidates} candidates.
        </p>
      )}

      {data && data.jobs.length === 0 && !loading && (
        <p className="mt-3 p-3 rounded-md text-sm bg-gray-50 dark:bg-gray-700 text-gray-700 dark:text-gray-300">
          No job has been open longer than {data.stale_minutes} minutes.
        </p>
      )}

      {data && data.jobs.length > 0 && (
        <div className="mt-3 overflow-x-auto">
          <table className="min-w-full text-sm">
            <thead>
              <tr className="text-left text-xs uppercase tracking-wider text-gray-500 dark:text-gray-400">
                <th className="py-2 pr-4">Job</th>
                <th className="py-2 pr-4">Repo</th>
                <th className="py-2 pr-4">Open for</th>
                <th className="py-2 pr-4">Our status</th>
                <th className="py-2 pr-4">GitHub</th>
                <th className="py-2 pr-4">Verdict</th>
                <th className="py-2 text-right">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-200 dark:divide-gray-700">
              {data.jobs.map((job) => (
                <HungRow key={job.job_id} job={job} onActed={onActed} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function HungRow({ job, onActed }: { job: HungJob; onActed: () => void }) {
  return (
    <tr>
      <td className="py-2 pr-4 font-mono text-gray-900 dark:text-gray-100">
        <a href={`/admin/jobs/detail/?id=${encodeURIComponent(job.job_id)}`} className="hover:underline">
          {job.job_id}
        </a>
      </td>
      <td className="py-2 pr-4 text-gray-500 dark:text-gray-400">{job.repo || '-'}</td>
      <td className="py-2 pr-4 text-gray-700 dark:text-gray-300">{formatDuration(job.elapsed_seconds)}</td>
      <td className="py-2 pr-4 text-gray-500 dark:text-gray-400">{job.status}</td>
      <td className="py-2 pr-4 text-gray-500 dark:text-gray-400">
        {job.github_status || '-'}
        {job.github_conclusion && <span className="ml-1 opacity-75">({job.github_conclusion})</span>}
        {job.runner_name && (
          <div className="text-xs font-mono opacity-75" title="The runner GitHub actually gave this job to">
            {job.runner_name}
          </div>
        )}
      </td>
      <td className="py-2 pr-4">
        <span
          className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${CLASSIFICATION_STYLE[job.classification]}`}
          title={job.detail || CLASSIFICATION_HELP[job.classification]}
        >
          {CLASSIFICATION_LABEL[job.classification]}
        </span>
      </td>
      <td className="py-2 text-right">
        <JobActions
          jobId={job.job_id}
          status={job.status}
          instanceId={job.instance_id}
          createdAt={job.created_at}
          onActed={onActed}
        />
      </td>
    </tr>
  );
}
