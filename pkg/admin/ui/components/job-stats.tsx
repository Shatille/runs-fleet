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
    blue: 'bg-blue-50 text-blue-700',
    green: 'bg-green-50 text-green-700',
    red: 'bg-red-50 text-red-700',
    yellow: 'bg-yellow-50 text-yellow-700',
    purple: 'bg-purple-50 text-purple-700',
    indigo: 'bg-indigo-50 text-indigo-700',
  };

  const bgClass = color ? colorClasses[color] : 'bg-gray-50 text-gray-700';

  return (
    <div className={`rounded-lg p-4 ${bgClass}`}>
      <div className="text-sm font-medium opacity-75">{label}</div>
      <div className="text-2xl font-bold">{value}</div>
    </div>
  );
}
