'use client';

import { Suspense, useEffect, useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { DetailSkeleton } from '@/components/skeleton';
import { InstanceDetail } from '@/lib/types';
import { apiFetch } from '@/lib/api';
import { formatTimestamp } from '@/lib/format';

export default function InstanceDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <InstanceDetailView />
    </Suspense>
  );
}

function InstanceDetailView() {
  const searchParams = useSearchParams();
  const id = searchParams.get('id');

  const [instance, setInstance] = useState<InstanceDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!id) {
      setError('No instance ID specified');
      setLoading(false);
      return;
    }
    async function fetchInstance() {
      try {
        setLoading(true);
        const res = await apiFetch(`/api/instances/${encodeURIComponent(id as string)}`);
        if (res.status === 404) {
          setError('Instance not found');
          return;
        }
        if (!res.ok) {
          throw new Error(`Failed to fetch instance: ${res.statusText}`);
        }
        setInstance(await res.json());
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to load instance');
      } finally {
        setLoading(false);
      }
    }
    fetchInstance();
  }, [id]);

  if (loading) return <DetailSkeleton />;

  if (error) {
    return (
      <div className="bg-red-50 dark:bg-red-900/30 border border-red-200 dark:border-red-800 rounded-md p-4">
        <p className="text-red-800 dark:text-red-300">{error}</p>
        <a href="/admin/instances/" className="mt-2 inline-block text-blue-600 dark:text-blue-400 hover:underline">
          Back to Instances
        </a>
      </div>
    );
  }

  if (!instance) return null;

  const tags = instance.tags ? Object.entries(instance.tags).sort(([a], [b]) => a.localeCompare(b)) : [];

  return (
    <div>
      <div className="mb-6">
        <a href="/admin/instances/" className="text-blue-600 dark:text-blue-400 hover:underline text-sm">
          &larr; Back to Instances
        </a>
      </div>

      <div className="flex items-center gap-4 mb-6">
        <h1 className="text-2xl font-bold font-mono text-gray-900 dark:text-gray-100">{instance.instance_id}</h1>
        {instance.spot && (
          <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-orange-100 dark:bg-orange-900/50 text-orange-700 dark:text-orange-300">
            Spot
          </span>
        )}
        {instance.busy && (
          <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-yellow-100 dark:bg-yellow-900/50 text-yellow-700 dark:text-yellow-300">
            Busy
          </span>
        )}
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        <Section title="Identification">
          <Field label="Instance ID" value={instance.instance_id} mono />
          <Field label="Type" value={instance.instance_type} mono />
          <Field label="Architecture" value={instance.architecture || '-'} />
          <Field label="Pool" value={instance.pool || '-'} />
        </Section>

        <Section title="State">
          <Field label="State" value={instance.state} />
          <Field label="Capacity" value={instance.spot ? 'Spot' : 'On-Demand'} />
          <Field label="Busy" value={instance.busy ? 'Yes' : 'No'} />
          <Field label="State Reason" value={instance.state_reason || '-'} />
        </Section>

        <Section title="Placement & Network">
          <Field label="Availability Zone" value={instance.availability_zone || '-'} />
          <Field label="Subnet" value={instance.subnet_id || '-'} mono />
          <Field label="Private IP" value={instance.private_ip || '-'} mono />
          <Field label="AMI" value={instance.image_id || '-'} mono />
        </Section>

        <Section title="Timestamps">
          <Field label="Launched" value={formatTimestamp(instance.launch_time)} />
        </Section>
      </div>

      {tags.length > 0 && (
        <div className="mt-6 bg-white dark:bg-gray-800 shadow rounded-lg p-6">
          <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">Tags</h2>
          <dl className="grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-2">
            {tags.map(([k, v]) => (
              <div key={k} className="flex justify-between gap-4">
                <dt className="text-sm font-mono text-gray-500 dark:text-gray-400 truncate">{k}</dt>
                <dd className="text-sm font-mono text-gray-900 dark:text-gray-100 truncate">{v}</dd>
              </div>
            ))}
          </dl>
        </div>
      )}
    </div>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-white dark:bg-gray-800 shadow rounded-lg p-6">
      <h2 className="text-lg font-semibold text-gray-900 dark:text-gray-100 mb-4">{title}</h2>
      <dl className="space-y-3">{children}</dl>
    </div>
  );
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex justify-between items-center gap-4">
      <dt className="text-sm font-medium text-gray-500 dark:text-gray-400">{label}</dt>
      <dd className={`text-sm text-gray-900 dark:text-gray-100 truncate ${mono ? 'font-mono' : ''}`}>{value}</dd>
    </div>
  );
}
