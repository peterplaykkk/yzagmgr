/** Status values exposed by the UsageRequests filter and failure-code drilldown. */
export const USAGE_REQUEST_STATUS_OPTIONS = [
  '200',
  '400',
  '401',
  '403',
  '404',
  '429',
  '500',
  '502',
  '503',
] as const;

const FAILURE_STATUS_OPTIONS = new Set<string>(
  USAGE_REQUEST_STATUS_OPTIONS.filter((value) => value !== '200'),
);
const REQUEST_STATUS_OPTIONS = new Set<string>(USAGE_REQUEST_STATUS_OPTIONS);

/** A chart action exists only when the status can be selected on the request page. */
export function failureCodeStatus(code: string): number | null {
  return FAILURE_STATUS_OPTIONS.has(code) ? Number(code) : null;
}

/** Return the exact supported request-status value, without accepting numeric prefixes. */
export function requestStatusValue(value: string | null): string | null {
  return value !== null && REQUEST_STATUS_OPTIONS.has(value) ? value : null;
}
