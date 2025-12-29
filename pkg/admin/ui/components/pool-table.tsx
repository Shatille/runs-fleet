import { Pool } from '@/lib/types';

interface PoolTableProps {
  pools: Pool[];
  onDelete: (poolName: string) => void;
}

export default function PoolTable({ pools, onDelete }: PoolTableProps) {
  return (
    <div className="bg-white shadow rounded-lg overflow-hidden">
      <table className="min-w-full divide-y divide-gray-200">
        <thead className="bg-gray-50">
          <tr>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Pool Name
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Instance Type
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Running
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Stopped
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Arch
            </th>
            <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
              Type
            </th>
            <th className="px-6 py-3 text-right text-xs font-medium text-gray-500 uppercase tracking-wider">
              Actions
            </th>
          </tr>
        </thead>
        <tbody className="bg-white divide-y divide-gray-200">
          {pools.map((pool) => (
            <tr key={pool.pool_name} className="hover:bg-gray-50">
              <td className="px-6 py-4 whitespace-nowrap">
                <span className="font-medium text-gray-900">{pool.pool_name}</span>
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500">
                {pool.instance_type || '-'}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500">
                {pool.desired_running}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500">
                {pool.desired_stopped}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-gray-500">
                {pool.arch || '-'}
              </td>
              <td className="px-6 py-4 whitespace-nowrap">
                {pool.ephemeral ? (
                  <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-yellow-100 text-yellow-800">
                    Ephemeral
                  </span>
                ) : (
                  <span className="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-green-100 text-green-800">
                    Persistent
                  </span>
                )}
              </td>
              <td className="px-6 py-4 whitespace-nowrap text-right text-sm font-medium">
                <a
                  href={`/admin/pools/${pool.pool_name}/`}
                  className="text-blue-600 hover:text-blue-900 mr-4"
                >
                  Edit
                </a>
                {pool.ephemeral && (
                  <button
                    onClick={() => onDelete(pool.pool_name)}
                    className="text-red-600 hover:text-red-900"
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
