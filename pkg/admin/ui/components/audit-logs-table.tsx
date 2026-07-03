'use client';

import { Fragment, useMemo, useState } from 'react';
import { AuditEntry } from '@/lib/types';
import { useSortable } from '@/hooks/use-sortable';

type AuditSortField = 'timestamp' | 'user' | 'action' | 'result';

const AUDIT_COMPARATORS: Record<AuditSortField, (a: AuditEntry, b: AuditEntry) => number> = {
  timestamp: (a, b) => new Date(a.timestamp).getTime() - new Date(b.timestamp).getTime(),
  user: (a, b) => a.user.localeCompare(b.user),
  action: (a, b) => a.action.localeCompare(b.action),
  result: (a, b) => a.result.localeCompare(b.result),
};

const resultStyles: Record<string, string> = {
  success: 'bg-green-100 dark:bg-green-900/50 text-green-800 dark:text-green-300',
  denied: 'bg-yellow-100 dark:bg-yellow-900/50 text-yellow-800 dark:text-yellow-300',
  error: 'bg-red-100 dark:bg-red-900/50 text-red-800 dark:text-red-300',
};

interface AuditLogsTableProps {
  entries: AuditEntry[];
}

export default function AuditLogsTable({ entries }: AuditLogsTableProps) {
  const comparators = useMemo(() => AUDIT_COMPARATORS, []);
  const { sortedData, requestSort, getSortIndicator } = useSortable<AuditEntry, AuditSortField>(
    entries,
    'timestamp',
    'desc',
    comparators,
  );
  const [expandedID, setExpandedID] = useState<string | null>(null);

  const thBase = 'px-4 py-3 text-left text-xs font-medium text-gray-500 dark:text-gray-400 uppercase tracking-wider';
  const thSortable = `${thBase} cursor-pointer select-none hover:text-gray-700 dark:hover:text-gray-300`;

  return (
    <div className="bg-white dark:bg-gray-800 shadow rounded-lg overflow-hidden">
      <table className="min-w-full divide-y divide-gray-200 dark:divide-gray-700">
        <thead className="bg-gray-50 dark:bg-gray-700">
          <tr>
            <th className={thSortable} onClick={() => requestSort('timestamp')}>
              Time{getSortIndicator('timestamp')}
            </th>
            <th className={thSortable} onClick={() => requestSort('user')}>
              User{getSortIndicator('user')}
            </th>
            <th className={thSortable} onClick={() => requestSort('action')}>
              Action{getSortIndicator('action')}
            </th>
            <th className={thBase}>Target</th>
            <th className={thSortable} onClick={() => requestSort('result')}>
              Result{getSortIndicator('result')}
            </th>
            <th className={thBase}>IP</th>
          </tr>
        </thead>
        <tbody className="bg-white dark:bg-gray-800 divide-y divide-gray-200 dark:divide-gray-700">
          {sortedData.map((entry) => {
            const hasDetails = entry.details && Object.keys(entry.details).length > 0;
            const isExpanded = expandedID === entry.id;
            return (
              <Fragment key={entry.id}>
                <tr
                  className={hasDetails ? 'hover:bg-gray-50 dark:hover:bg-gray-700 cursor-pointer' : ''}
                  onClick={() => hasDetails && setExpandedID(isExpanded ? null : entry.id)}
                >
                  <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500 dark:text-gray-400">
                    {formatTime(entry.timestamp)}
                  </td>
                  <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-900 dark:text-gray-100">
                    {entry.user}
                  </td>
                  <td className="px-4 py-3 whitespace-nowrap text-sm font-mono text-gray-700 dark:text-gray-300">
                    {entry.action}
                    {hasDetails && (
                      <span className="ml-1 text-gray-400 dark:text-gray-500">{isExpanded ? '▲' : '▼'}</span>
                    )}
                  </td>
                  <td className="px-4 py-3 whitespace-nowrap text-sm text-gray-500 dark:text-gray-400 max-w-xs truncate">
                    {entry.target || '-'}
                  </td>
                  <td className="px-4 py-3 whitespace-nowrap">
                    <span className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${resultStyles[entry.result] || 'bg-gray-100 dark:bg-gray-700 text-gray-800 dark:text-gray-300'}`}>
                      {entry.result}
                    </span>
                  </td>
                  <td className="px-4 py-3 whitespace-nowrap text-sm font-mono text-gray-500 dark:text-gray-400">
                    {entry.client_ip || '-'}
                  </td>
                </tr>
                {isExpanded && hasDetails && (
                  <tr>
                    <td colSpan={6} className="px-4 py-3 bg-gray-50 dark:bg-gray-900/50">
                      <pre className="text-xs text-gray-700 dark:text-gray-300 whitespace-pre-wrap font-mono">
                        {JSON.stringify(entry.details, null, 2)}
                      </pre>
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function formatTime(isoString: string): string {
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
