import Link from 'next/link';
import { Job } from '@/lib/types';

interface JobsTableProps {
  jobs: Job[];
  traceURL?: string;
}

export default function JobsTable({ jobs, traceURL }: JobsTableProps) {
  return (
    <div className="bg-white shadow rounded-lg overflow-hidden">
      <table className="min-w-full divide-y divide-gray-200">
        <thead className="bg-gray-50">
          <tr>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Job ID
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Repo
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Status
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Instance
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Pool
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Type
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Duration
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Created
            </th>
            {traceURL && (
              <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                Trace
              </th>
            )}
          </tr>
        </thead>
        <tbody className="bg-white divide-y divide-gray-200">
          {jobs.map((job) => (
            <tr key={job.job_id} className="hover:bg-gray-50 cursor-pointer" onClick={() => window.location.href = `/admin/jobs/${job.job_id}/`}>
              <td className="px-4 py-3 whitespace-nowrap">
                <span className="font-mono text-sm text-gray-900">{job.job_id}</span>
                {job.run_id && (
                  <span className="ml-2 text-xs text-gray-400">#{job.run_id}</span>
                )}
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500 max-w-xs truncate">
                {job.repo || '-'}
              </td>
              <td className="px-4 py-3 whitespace-nowrap">
                <StatusBadge status={job.status} exitCode={job.exit_code} />
              </td>
              <td className="px-4 py-3 whitespace-nowrap">
                <div className="text-sm">
                  <span className="font-mono text-gray-700">
                    {job.instance_id ? job.instance_id.slice(-12) : '-'}
                  </span>
                  <div className="flex gap-1 mt-0.5">
                    {job.spot && (
                      <span className="inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium bg-orange-100 text-orange-700">
                        Spot
                      </span>
                    )}
                    {job.warm_pool_hit && (
                      <span className="inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium bg-green-100 text-green-700">
                        Warm
                      </span>
                    )}
                  </div>
                </div>
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500">
                {job.pool || '-'}
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500 font-mono">
                {job.instance_type || '-'}
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500">
                {formatDuration(job.duration_seconds)}
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500">
                {formatTime(job.created_at)}
              </td>
              {traceURL && (
                <td className="px-4 py-3 whitespace-nowrap text-sm">
                  {job.trace_id ? (
                    <a
                      href={`${traceURL}${job.trace_id}`}
                      target="_blank"
                      rel="noopener noreferrer"
                      onClick={(e) => e.stopPropagation()}
                      className="text-blue-600 hover:text-blue-800"
                      title={job.trace_id}
                    >
                      <svg xmlns="http://www.w3.org/2000/svg" className="h-4 w-4 inline" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                        <path strokeLinecap="round" strokeLinejoin="round" d="M13.828 10.172a4 4 0 00-5.656 0l-4 4a4 4 0 105.656 5.656l1.102-1.101m-.758-4.899a4 4 0 005.656 0l4-4a4 4 0 00-5.656-5.656l-1.1 1.1" />
                      </svg>
                    </a>
                  ) : (
                    <span className="text-gray-300">-</span>
                  )}
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

interface StatusBadgeProps {
  status: string;
  exitCode?: number;
}

function StatusBadge({ status, exitCode }: StatusBadgeProps) {
  const statusStyles: Record<string, string> = {
    pending: 'bg-gray-100 text-gray-800',
    queued: 'bg-blue-100 text-blue-800',
    running: 'bg-yellow-100 text-yellow-800',
    completed: 'bg-green-100 text-green-800',
    failed: 'bg-red-100 text-red-800',
    terminated: 'bg-red-100 text-red-800',
    requeued: 'bg-orange-100 text-orange-800',
  };

  const style = statusStyles[status.toLowerCase()] || 'bg-gray-100 text-gray-800';

  return (
    <span className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${style}`}>
      {status}
      {exitCode !== undefined && exitCode !== 0 && (
        <span className="ml-1 opacity-75">({exitCode})</span>
      )}
    </span>
  );
}

function formatDuration(seconds?: number): string {
  if (!seconds) return '-';
  if (seconds < 60) return `${seconds}s`;
  const mins = Math.floor(seconds / 60);
  const secs = seconds % 60;
  if (mins < 60) return `${mins}m ${secs}s`;
  const hours = Math.floor(mins / 60);
  const remainingMins = mins % 60;
  return `${hours}h ${remainingMins}m`;
}

function formatTime(isoString?: string): string {
  if (!isoString) return '-';
  const date = new Date(isoString);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  const diffMins = Math.floor(diffMs / 60000);

  if (diffMins < 1) return 'just now';
  if (diffMins < 60) return `${diffMins}m ago`;

  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;

  return date.toLocaleDateString();
}
