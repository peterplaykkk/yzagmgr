import { useMemo } from 'react';
import { readModelSelection } from './modelParam';

/** Keep MUI Autocomplete's controlled value stable across non-model URL updates. */
export function useStableModelSelection(search: URLSearchParams): string[] {
  const selectionKey = useMemo(() => JSON.stringify(readModelSelection(search)), [search]);
  return useMemo(() => JSON.parse(selectionKey) as string[], [selectionKey]);
}
