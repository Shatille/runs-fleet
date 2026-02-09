'use client';

import { useEffect, useState, useCallback } from 'react';
import InstancesTable from '@/components/instances-table';
import { Instance } from '@/lib/types';
import { apiFetch } from '@/lib/api';

export default function InstancesPage() {
  const [instances, setInstances] = useState<Instance[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [poolFilter, setPoolFilter] = useState<string>('');
  const [stateFilter, setStateFilter] = useState<string>('');

  const fetchInstances = useCallback(async () => {
    try {
      setLoading(true);
      const params = new URLSearchParams();
      if (poolFilter) params.set('pool', poolFilter);
      if (stateFilter) params.set('state', stateFilter);

      const query = params.toString();
      const res = await apiFetch(`/api/instances${query ? '?' + query : ''}`);
      if (!res.ok) {
        throw new Error(`Failed to fetch instances: ${res.statusText}`);
      }
      const data = await res.json();
      setInstances(data.instances || []);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load instances');
    } finally {
      setLoading(false);
    }
  }, [poolFilter, stateFilter]);

  useEffect(() => {
    fetchInstances();
  }, [fetchInstances]);

  const handleRefresh = () => {
    fetchInstances();
  };

  const stats = {
    total: instances.length,
    running: instances.filter((i) => i.state === 'running').length,
    stopped: instances.filter((i) => i.state === 'stopped').length,
    busy: instances.filter((i) => i.busy).length,
    spot: instances.filter((i) => i.spot).length,
  };

  if (error) {
    return (
      <div className="bg-red-50 border border-red-200 rounded-md p-4">
        <p className="text-red-800">{error}</p>
        <button
          onClick={handleRefresh}
          className="mt-2 text-red-600 underline hover:no-underline"
        >
          Retry
        </button>
      </div>
    );
  }

  return (
    <div>
      <div className="flex justify-between items-center mb-6">
        <h1 className="text-2xl font-bold text-gray-900">Instances</h1>
        <button
          onClick={handleRefresh}
          disabled={loading}
          className="bg-gray-100 text-gray-700 px-4 py-2 rounded-md hover:bg-gray-200 transition-colors disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Refresh'}
        </button>
      </div>

      <div className="grid grid-cols-2 md:grid-cols-5 gap-4 mb-6">
        <StatCard label="Total" value={stats.total} />
        <StatCard label="Running" value={stats.running} color="green" />
        <StatCard label="Stopped" value={stats.stopped} color="gray" />
        <StatCard label="Busy" value={stats.busy} color="yellow" />
        <StatCard label="Spot" value={stats.spot} color="orange" />
      </div>

      <div className="mb-4 flex gap-4">
        <input
          type="text"
          placeholder="Filter by pool..."
          value={poolFilter}
          onChange={(e) => setPoolFilter(e.target.value)}
          className="rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        />

        <select
          value={stateFilter}
          onChange={(e) => setStateFilter(e.target.value)}
          className="rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
        >
          <option value="">All States</option>
          <option value="running">Running</option>
          <option value="stopped">Stopped</option>
          <option value="pending">Pending</option>
          <option value="stopping">Stopping</option>
        </select>
      </div>

      {loading && instances.length === 0 ? (
        <div className="flex items-center justify-center h-64">
          <div className="text-gray-500">Loading instances...</div>
        </div>
      ) : instances.length === 0 ? (
        <div className="text-center py-12 bg-white rounded-lg border">
          <p className="text-gray-500">No instances found.</p>
        </div>
      ) : (
        <InstancesTable instances={instances} />
      )}
    </div>
  );
}

interface StatCardProps {
  label: string;
  value: number;
  color?: 'green' | 'gray' | 'yellow' | 'orange';
}

function StatCard({ label, value, color }: StatCardProps) {
  const colorClasses = {
    green: 'bg-green-50 text-green-700',
    gray: 'bg-gray-50 text-gray-700',
    yellow: 'bg-yellow-50 text-yellow-700',
    orange: 'bg-orange-50 text-orange-700',
  };

  const bgClass = color ? colorClasses[color] : 'bg-gray-50 text-gray-700';

  return (
    <div className={`rounded-lg p-4 ${bgClass}`}>
      <div className="text-sm font-medium opacity-75">{label}</div>
      <div className="text-2xl font-bold">{value}</div>
    </div>
  );
}
