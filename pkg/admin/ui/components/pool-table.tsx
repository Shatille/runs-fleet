import { Pool } from '@/lib/types';
import { formatRelativeTime } from '@/lib/format';
import { HelpTip } from '@/components/help-tip';

interface PoolTableProps {
  pools: Pool[];
  onDelete: (poolName: string) => void;
}

// HotCell shows the effective hot spec (override > recommendation), with a "*"
// when an operator override is in force. "-" when the pool resolves cold.
function HotCell({ pool }: { pool: Pool }) {
  const overridden = pool.override_linger_minutes != null || pool.override_max_hot != null;
  const linger = pool.override_linger_minutes ?? pool.auto_tune?.recommended_linger_minutes ?? 0;
  const maxHot = pool.override_max_hot ?? pool.auto_tune?.recommended_max_hot ?? 0;
  if (!linger || linger <= 0) {
    return <span className="text-gray-400 dark:text-gray-500">{overridden ? 'cold*' : '-'}</span>;
  }
  return (
    <span title={overridden ? 'operator override' : 'auto-tuned'}>
      {linger}m / {maxHot}
      {overridden && '*'}
    </span>
  );
}

function ReconcileCell({ pool }: { pool: Pool }) {
  if (!pool.last_reconcile_at) {
    return <span className="text-gray-400 dark:text-gray-500">-</span>;
  }
  const failed = !!pool.last_reconcile_result && pool.last_reconcile_result !== 'success';
  return (
    <span className="inline-flex items-center gap-1.5">
      <span
        className={`inline-block h-2 w-2 rounded-full ${failed ? 'bg-red-500' : 'bg-green-500'}`}
        title={pool.last_reconcile_result || 'success'}
      />
      <span>{formatRelativeTime(pool.last_reconcile_at)}</span>
    </span>
  );
}

export default function PoolTable({ pools, onDelete }: PoolTableProps) {
  return (
    <div className="bg-white dark:bg-gray-800 shadow rounded-lg overflow-hidden">
      <table className="min-w-full divide-y divide-gray-200 dark:divide-gray-700">
        <thead className="bg-gray-50 dark:bg-gray-700">
          <tr>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Pool Name
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Instance Type
              <HelpTip text="Instance type pinned for this pool. '-' means no pin: the fleet picks a type from the catalog per job." />
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Running
              <HelpTip text="current / desired. Left: instances running right now. Right: the configured warm floor (desired_running) the reconciler maintains while idle. Busy instances and hot-pool linger sit on top of that floor, so current above desired is normal under load — that is why a pool with desired 0 can show 4 running. Yellow just marks current differing from desired, not an error." />
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Stopped
              <HelpTip text="current / desired stopped instances. Stopped instances cost only EBS and can be started faster than a cold boot. Yellow marks current differing from desired." />
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Busy
              <HelpTip text="Instances in this pool currently executing a job. Always a subset of Running." />
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Arch
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Type
              <HelpTip text="Ephemeral pools are auto-created on first use, inherit their instance spec from the first job, and can be deleted here. Persistent pools are declared in configuration." />
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Hot
              <HelpTip text="linger / max hot. Left: minutes an idle instance stays running after finishing a job so the next job skips boot. Right: cap on how many instances may linger. '*' means an operator override, otherwise auto-tuned. '-' means no linger (fully cold)." />
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Last Reconcile
              <HelpTip text="When the reconciler last ran for this pool. Green = success, red = failure. Running and Stopped are snapshots written by that run, so they can lag real EC2 state by up to one reconcile interval (60s)." />
            </th>
            <th className="px-6 py-3 text-right text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider">
              Actions
            </th>
          </tr>
        </thead>
        <tbody className="bg-white dark:bg-gray-800 divide-y divide-gray-200 dark:divide-gray-700">
          {pools.map((pool) => (
            <tr key={pool.pool_name} className="hover:bg-gray-50 dark:hover:bg-gray-700">
              <td className="px-6 py-4 whitespace-nowrap">
                <span className="font-medium text-gray-900 dark:text-gray-100">{pool.pool_name}</span>
                {pool.schedules && pool.schedules.length > 0 && (
                  <span className="ml-2 inline-flex items-center px-1.5 py-0.5 rounded text-xs font-medium bg-blue-50 dark:bg-blue-900/50 text-blue-700 dark:text-blue-300">
                    {pool.schedules.length} sched
                  </span>
                )}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500 dark:text-gray-400">
                {pool.instance_type || '-'}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500 dark:text-gray-400">
                <span className={pool.current_running !== pool.desired_running ? 'text-yellow-600 dark:text-yellow-400' : ''}>
                  {pool.current_running ?? '-'}/{pool.desired_running}
                </span>
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500 dark:text-gray-400">
                <span className={pool.current_stopped !== pool.desired_stopped ? 'text-yellow-600 dark:text-yellow-400' : ''}>
                  {pool.current_stopped ?? '-'}/{pool.desired_stopped}
                </span>
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500 dark:text-gray-400">
                {pool.busy_instances > 0 ? (
                  <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-orange-100 dark:bg-orange-900/50 text-orange-800 dark:text-orange-300">
                    {pool.busy_instances}
                  </span>
                ) : (
                  <span className="text-gray-400 dark:text-gray-500">0</span>
                )}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500 dark:text-gray-400">
                {pool.arch || '-'}
              </td>
              <td className="px-6 py-4 whitespace-nowrap">
                {pool.ephemeral ? (
                  <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-yellow-100 dark:bg-yellow-900/50 text-yellow-800 dark:text-yellow-300">
                    Ephemeral
                  </span>
                ) : (
                  <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-green-100 dark:bg-green-900/50 text-green-800 dark:text-green-300">
                    Persistent
                  </span>
                )}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500 dark:text-gray-400">
                <HotCell pool={pool} />
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500 dark:text-gray-400">
                <ReconcileCell pool={pool} />
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-right text-sm font-medium">
                <a
                  href={`/admin/pools/edit/?name=${encodeURIComponent(pool.pool_name)}`}
                  className="text-blue-600 dark:text-blue-400 hover:text-blue-900 dark:hover:text-blue-300 mr-4"
                >
                  Edit
                </a>
                {pool.ephemeral && (
                  <button
                    onClick={() => onDelete(pool.pool_name)}
                    className="text-red-600 dark:text-red-400 hover:text-red-900 dark:hover:text-red-300"
                  >
                    Delete
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
