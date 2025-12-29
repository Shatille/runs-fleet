'use client';

import { useState } from 'react';
import { Pool, PoolFormData } from '@/lib/types';

interface PoolFormProps {
  pool?: Pool;
  onSubmit: (data: PoolFormData) => Promise<void>;
  isEdit?: boolean;
}

export default function PoolForm({ pool, onSubmit, isEdit = false }: PoolFormProps) {
  const [formData, setFormData] = useState<PoolFormData>({
    pool_name: pool?.pool_name || '',
    instance_type: pool?.instance_type || '',
    desired_running: pool?.desired_running || 0,
    desired_stopped: pool?.desired_stopped || 0,
    idle_timeout_minutes: pool?.idle_timeout_minutes || 60,
    arch: pool?.arch || '',
    cpu_min: pool?.cpu_min || 0,
    cpu_max: pool?.cpu_max || 0,
    ram_min: pool?.ram_min || 0,
    ram_max: pool?.ram_max || 0,
    families: pool?.families || [],
  });

  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);

    try {
      await onSubmit(formData);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to save pool');
    } finally {
      setSubmitting(false);
    }
  }

  function handleChange(e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) {
    const { name, value, type } = e.target;
    setFormData((prev) => ({
      ...prev,
      [name]: type === 'number' ? (value === '' ? 0 : Number(value)) : value,
    }));
  }

  function handleFamiliesChange(e: React.ChangeEvent<HTMLInputElement>) {
    const value = e.target.value;
    setFormData((prev) => ({
      ...prev,
      families: value ? value.split(',').map((f) => f.trim()) : [],
    }));
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-6 bg-white shadow rounded-lg p-6">
      {error && (
        <div className="bg-red-50 border border-red-200 rounded-md p-4">
          <p className="text-red-800">{error}</p>
        </div>
      )}

      <div className="grid grid-cols-1 md:grid-cols-2 gap-6">
        <div>
          <label htmlFor="pool_name" className="block text-sm font-medium text-gray-700">
            Pool Name
          </label>
          <input
            type="text"
            id="pool_name"
            name="pool_name"
            value={formData.pool_name}
            onChange={handleChange}
            disabled={isEdit}
            required
            pattern="[a-zA-Z0-9][a-zA-Z0-9_-]*"
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500 disabled:bg-gray-100"
            placeholder="my-pool"
          />
          <p className="mt-1 text-sm text-gray-500">
            Alphanumeric with hyphens/underscores, starting with alphanumeric
          </p>
        </div>

        <div>
          <label htmlFor="instance_type" className="block text-sm font-medium text-gray-700">
            Instance Type
          </label>
          <input
            type="text"
            id="instance_type"
            name="instance_type"
            value={formData.instance_type}
            onChange={handleChange}
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
            placeholder="c7g.xlarge"
          />
        </div>

        <div>
          <label htmlFor="desired_running" className="block text-sm font-medium text-gray-700">
            Desired Running
          </label>
          <input
            type="number"
            id="desired_running"
            name="desired_running"
            value={formData.desired_running}
            onChange={handleChange}
            min="0"
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
          />
          <p className="mt-1 text-sm text-gray-500">Number of warm running instances</p>
        </div>

        <div>
          <label htmlFor="desired_stopped" className="block text-sm font-medium text-gray-700">
            Desired Stopped
          </label>
          <input
            type="number"
            id="desired_stopped"
            name="desired_stopped"
            value={formData.desired_stopped}
            onChange={handleChange}
            min="0"
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
          />
          <p className="mt-1 text-sm text-gray-500">Number of stopped instances ready to start</p>
        </div>

        <div>
          <label htmlFor="idle_timeout_minutes" className="block text-sm font-medium text-gray-700">
            Idle Timeout (minutes)
          </label>
          <input
            type="number"
            id="idle_timeout_minutes"
            name="idle_timeout_minutes"
            value={formData.idle_timeout_minutes}
            onChange={handleChange}
            min="0"
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
          />
        </div>

        <div>
          <label htmlFor="arch" className="block text-sm font-medium text-gray-700">
            Architecture
          </label>
          <select
            id="arch"
            name="arch"
            value={formData.arch}
            onChange={handleChange}
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
          >
            <option value="">Any</option>
            <option value="arm64">arm64</option>
            <option value="amd64">amd64</option>
          </select>
        </div>

        <div>
          <label htmlFor="cpu_min" className="block text-sm font-medium text-gray-700">
            CPU Min
          </label>
          <input
            type="number"
            id="cpu_min"
            name="cpu_min"
            value={formData.cpu_min || ''}
            onChange={handleChange}
            min="0"
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
            placeholder="0"
          />
        </div>

        <div>
          <label htmlFor="cpu_max" className="block text-sm font-medium text-gray-700">
            CPU Max
          </label>
          <input
            type="number"
            id="cpu_max"
            name="cpu_max"
            value={formData.cpu_max || ''}
            onChange={handleChange}
            min="0"
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
            placeholder="0"
          />
        </div>

        <div>
          <label htmlFor="ram_min" className="block text-sm font-medium text-gray-700">
            RAM Min (GB)
          </label>
          <input
            type="number"
            id="ram_min"
            name="ram_min"
            value={formData.ram_min || ''}
            onChange={handleChange}
            min="0"
            step="0.5"
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
            placeholder="0"
          />
        </div>

        <div>
          <label htmlFor="ram_max" className="block text-sm font-medium text-gray-700">
            RAM Max (GB)
          </label>
          <input
            type="number"
            id="ram_max"
            name="ram_max"
            value={formData.ram_max || ''}
            onChange={handleChange}
            min="0"
            step="0.5"
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
            placeholder="0"
          />
        </div>

        <div className="md:col-span-2">
          <label htmlFor="families" className="block text-sm font-medium text-gray-700">
            Instance Families
          </label>
          <input
            type="text"
            id="families"
            name="families"
            value={formData.families.join(', ')}
            onChange={handleFamiliesChange}
            className="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-blue-500 focus:ring-blue-500"
            placeholder="c7g, m7g, r7g"
          />
          <p className="mt-1 text-sm text-gray-500">Comma-separated list of instance families</p>
        </div>
      </div>

      <div className="flex justify-end space-x-4">
        <a
          href="/admin/"
          className="px-4 py-2 border border-gray-300 rounded-md text-gray-700 hover:bg-gray-50"
        >
          Cancel
        </a>
        <button
          type="submit"
          disabled={submitting}
          className="px-4 py-2 bg-blue-600 text-white rounded-md hover:bg-blue-700 disabled:opacity-50"
        >
          {submitting ? 'Saving...' : isEdit ? 'Update Pool' : 'Create Pool'}
        </button>
      </div>
    </form>
  );
}
