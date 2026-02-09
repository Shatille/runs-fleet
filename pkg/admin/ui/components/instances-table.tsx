import { Instance } from '@/lib/types';

interface InstancesTableProps {
  instances: Instance[];
}

export default function InstancesTable({ instances }: InstancesTableProps) {
  return (
    <div className="bg-white shadow rounded-lg overflow-hidden">
      <table className="min-w-full divide-y divide-gray-200">
        <thead className="bg-gray-50">
          <tr>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Instance ID
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Type
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Pool
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              State
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Status
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Private IP
            </th>
            <th className="px-4 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Launch Time
            </th>
          </tr>
        </thead>
        <tbody className="bg-white divide-y divide-gray-200">
          {instances.map((inst) => (
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
