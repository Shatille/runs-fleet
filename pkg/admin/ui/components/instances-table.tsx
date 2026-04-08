'use client';

import { useMemo } from 'react';
import { Instance } from '@/lib/types';
import { useSortable } from '@/hooks/use-sortable';

type InstanceSortField = 'instance_id' | 'state' | 'pool' | 'instance_type' | 'launch_time';

const INSTANCE_COMPARATORS: Record<InstanceSortField, (a: Instance, b: Instance) => number> = {
  instance_id: (a, b) => a.instance_id.localeCompare(b.instance_id),
  state: (a, b) => a.state.localeCompare(b.state),
  pool: (a, b) => (a.pool || '').localeCompare(b.pool || ''),
  instance_type: (a, b) => a.instance_type.localeCompare(b.instance_type),
  launch_time: (a, b) => {
    const da = a.launch_time ? new Date(a.launch_time).getTime() : 0;
    const db = b.launch_time ? new Date(b.launch_time).getTime() : 0;
    return da - db;
  },
};

interface InstancesTableProps {
  instances: Instance[];
}

export default function InstancesTable({ instances }: InstancesTableProps) {
  const comparators = useMemo(() => INSTANCE_COMPARATORS, []);
  const { sortedData, requestSort, getSortIndicator } = useSortable<Instance, InstanceSortField>(
    instances,
    'launch_time',
    'desc',
    comparators,
  );

  const thBase = 'px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider';
  const thSortable = `${thBase} cursor-pointer select-none hover:text-gray-700`;

  return (
    <div className="bg-white shadow rounded-lg overflow-hidden">
      <table className="min-w-full divide-y divide-gray-200">
        <thead className="bg-gray-50">
          <tr>
            <th className={thSortable} onClick={() => requestSort('instance_id')}>
              Instance ID{getSortIndicator('instance_id')}
            </th>
            <th className={thSortable} onClick={() => requestSort('instance_type')}>
              Type{getSortIndicator('instance_type')}
            </th>
            <th className={thSortable} onClick={() => requestSort('pool')}>
              Pool{getSortIndicator('pool')}
            </th>
            <th className={thSortable} onClick={() => requestSort('state')}>
              State{getSortIndicator('state')}
            </th>
            <th className={thBase}>
              Status
            </th>
            <th className={thBase}>
              Private IP
            </th>
            <th className={thSortable} onClick={() => requestSort('launch_time')}>
              Launch Time{getSortIndicator('launch_time')}
            </th>
          </tr>
        </thead>
        <tbody className="bg-white divide-y divide-gray-200">
          {sortedData.map((inst) => (
            <tr key={inst.instance_id} className="hover:bg-gray-50">
              <td className="px-4 py-3 whitespace-nowrap">
                <span className="font-mono text-sm text-gray-900">{inst.instance_id}</span>
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500 font-mono">
                {inst.instance_type}
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500">
                {inst.pool || '-'}
              </td>
              <td className="px-4 py-3 whitespace-nowrap">
                <StateBadge state={inst.state} />
              </td>
              <td className="px-4 py-3 whitespace-nowrap">
                <div className="flex gap-1">
                  {inst.spot && (
                    <span className="inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium bg-orange-100 text-orange-700">
                      Spot
                    </span>
                  )}
                  {inst.busy ? (
                    <span className="inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium bg-yellow-100 text-yellow-700">
                      Busy
                    </span>
                  ) : inst.state === 'running' ? (
                    <span className="inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium bg-green-100 text-green-700">
                      Idle
                    </span>
                  ) : null}
                </div>
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500 font-mono">
                {inst.private_ip || '-'}
              </td>
              <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500">
                {formatTime(inst.launch_time)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

interface StateBadgeProps {
  state: string;
}

function StateBadge({ state }: StateBadgeProps) {
  const stateStyles: Record<string, string> = {
    pending: 'bg-blue-100 text-blue-800',
    running: 'bg-green-100 text-green-800',
    stopping: 'bg-yellow-100 text-yellow-800',
    stopped: 'bg-gray-100 text-gray-800',
    'shutting-down': 'bg-red-100 text-red-800',
    terminated: 'bg-red-100 text-red-800',
  };

  const style = stateStyles[state.toLowerCase()] || 'bg-gray-100 text-gray-800';

  return (
    <span className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${style}`}>
      {state}
    </span>
  );
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

  const diffDays = Math.floor(diffHours / 24);
  if (diffDays < 7) return `${diffDays}d ago`;

  return date.toLocaleDateString();
}
