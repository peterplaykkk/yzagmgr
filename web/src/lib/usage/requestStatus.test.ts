import { describe, expect, it } from 'vitest';
import {
  failureCodeStatus,
  requestStatusValue,
  USAGE_REQUEST_STATUS_OPTIONS,
} from './requestStatus';

describe('usage request status（UC-113 A4）', () => {
  it('requestStatusValue accepts exact supported values and rejects null or invalid strings', () => {
    for (const value of USAGE_REQUEST_STATUS_OPTIONS) {
      expect(requestStatusValue(value)).toBe(value);
    }

    for (const value of [null, '', '418', '429x', ' 429', '429 ', '0429']) {
      expect(requestStatusValue(value)).toBeNull();
    }
  });

  it('failureCodeStatus exposes supported nonzero statuses only', () => {
    const supportedFailures = ['400', '401', '403', '404', '429', '500', '502', '503'];
    for (const code of supportedFailures) {
      expect(failureCodeStatus(code)).toBe(Number(code));
    }

    for (const code of [null, undefined, '', '200', '418', '429x', ' 429', '0429']) {
      expect(failureCodeStatus(code as string)).toBeNull();
    }
  });
});
