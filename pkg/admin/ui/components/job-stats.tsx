import { JobStats } from '@/lib/types';

interface JobStatsProps {
  stats: JobStats;
}

export default function JobStatsCard({ stats }: JobStatsProps) {
  return (
    <div className="grid grid-cols-2 md:grid-cols-4 lg:grid-cols-7 gap-4 mb-6">
      <StatCard label="Total" value={stats.total} />
      <StatCard label="Running" value={stats.running} color="blue" />
      <StatCard label="Completed" value={stats.completed} color="green" />
      <StatCard label="Failed" value={stats.failed} color="red" />
      <StatCard label="Requeued" value={stats.requeued} color="yellow" />
      <StatCard label="Warm Pool Hits" value={stats.warm_pool_hit} color="purple" />
      <StatCard
        label="Hit Rate"
        value={`${(stats.hit_rate * 100).toFixed(1)}%`}
        color="indigo"
      />
    </div>
  );
}

interface StatCardProps {
  label: string;
  value: number | string;
  color?: 'blue' | 'green' | 'red' | 'yellow' | 'purple' | 'indigo';
}

function StatCard({ label, value, color }: StatCardProps) {
  const colorClasses = {
    blue: 'bg-blue-50 dark:bg-blue-900/30 text-blue-700 dark:text-blue-400',
    green: 'bg-green-50 dark:bg-green-900/30 text-green-700 dark:text-green-400',
    red: 'bg-red-50 dark:bg-red-900/30 text-red-700 dark:text-red-400',
    yellow: 'bg-yellow-50 dark:bg-yellow-900/30 text-yellow-700 dark:text-yellow-400',
    purple: 'bg-purple-50 dark:bg-purple-900/30 text-purple-700 dark:text-purple-400',
    indigo: 'bg-indigo-50 dark:bg-indigo-900/30 text-indigo-700 dark:text-indigo-400',
  };

  const bgClass = color ? colorClasses[color] : 'bg-gray-50 dark:bg-gray-800 text-gray-700 dark:text-gray-300';

  return (
    <div className={`rounded-lg p-4 ${bgClass}`}>
      <div className="text-sm font-medium opacity-75">{label}</div>
      <div className="text-2xl font-bold">{value}</div>
    </div>
  );
}
