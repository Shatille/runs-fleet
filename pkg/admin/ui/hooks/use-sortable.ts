'use client';

import { useState, useMemo } from 'react';

export type SortDirection = 'asc' | 'desc';

export interface SortState<K extends string> {
  field: K;
  direction: SortDirection;
}

export interface UseSortableReturn<T, K extends string> {
  sortedData: T[];
  sortState: SortState<K>;
  requestSort: (field: K) => void;
  getSortIndicator: (field: K) => string;
}

type CompareFn<T> = (a: T, b: T) => number;

export function useSortable<T, K extends string>(
  data: T[],
  defaultField: K,
  defaultDirection: SortDirection,
  comparators: Record<K, CompareFn<T>>,
): UseSortableReturn<T, K> {
  const [sortState, setSortState] = useState<SortState<K>>({
    field: defaultField,
    direction: defaultDirection,
  });

  const requestSort = (field: K) => {
    setSortState((prev) => ({
      field,
      direction: prev.field === field && prev.direction === 'asc' ? 'desc' : 'asc',
    }));
  };

  const sortedData = useMemo(() => {
    const compare = comparators[sortState.field];
    if (!compare) return data;

    const sorted = [...data].sort((a, b) => {
      const result = compare(a, b);
      return sortState.direction === 'asc' ? result : -result;
    });
    return sorted;
  }, [data, sortState, comparators]);

  const getSortIndicator = (field: K): string => {
    if (sortState.field !== field) return '';
    return sortState.direction === 'asc' ? ' \u25B2' : ' \u25BC';
  };

  return { sortedData, sortState, requestSort, getSortIndicator };
}
