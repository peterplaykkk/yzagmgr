import { describe, expect, it } from 'vitest';
import {
  AgmgrApiError,
  ERROR_CODES,
  ERROR_COPY,
  SERVICE_RESTART_KEY,
  UNKNOWN_CODE_ACTION_KEY,
  UNKNOWN_CODE_DETAIL_KEY,
  errorCopy,
  isKnownErrorCode,
  isOperationOutcomeUnknown,
  isServiceRestart,
  serviceRestartHintKey,
  toAgmgrApiError,
} from './errors';
import en from '../locales/en.json';
import zh from '../locales/zh.json';

/** 契约 §1.3 的错误码表（逐字抄一遍：契约改了这里必须跟着红）。 */
const CONTRACT_CODES = [
  'invalid_request',
  'unauthorized',
  'forbidden_origin',
  'not_found',
  'config_changed',
  // UC-094 B-5：个人配置的乐观并发失败（A5，契约表同批新增）
  'version_conflict',
  'config_invalid',
  'path_conflict',
  'permission_denied',
  'preview_expired',
  'preview_in_use',
  'backup_unreadable',
  'storage',
  'gateway_unreachable',
  'gateway_unexpected_response',
  'cancelled',
  // v8 吞吐速率 429（design.md §1.4④；IP4 三处同步的 web 侧落点）
  'rate_limited',
  'not_supported',
  'internal',
] as const;

describe('错误码是封闭集合', () => {
  it('与 api-v1.md §1.3 的保留码集逐字一致（19 条）', () => {
    expect([...ERROR_CODES].sort()).toEqual([...CONTRACT_CODES].sort());
    expect(ERROR_CODES).toHaveLength(19);
  });

  it('集合外的码被判定为未知', () => {
    expect(isKnownErrorCode('config_changed')).toBe(true);
    expect(isKnownErrorCode('brand_new_code')).toBe(false);
    expect(isKnownErrorCode('')).toBe(false);
  });

  it('每个码都有文案，且中英文都有对应条目', () => {
    const zhDict = zh as Record<string, string>;
    const enDict = en as Record<string, string>;
    for (const code of ERROR_CODES) {
      const copy = ERROR_COPY[code];
      expect(errorCopy(code), code).toEqual(copy);
      expect(copy.detailKey.startsWith('error.'), code).toBe(true);
      expect(copy.actionKey.startsWith('error.'), code).toBe(true);
      expect(zhDict[copy.detailKey], `zh ${copy.detailKey}`).toBeTruthy();
      expect(zhDict[copy.actionKey], `zh ${copy.actionKey}`).toBeTruthy();
      expect(enDict[copy.detailKey], `en ${copy.detailKey}`).toBeTruthy();
      expect(enDict[copy.actionKey], `en ${copy.actionKey}`).toBeTruthy();
    }
  });

  it('未知码回退到通用文案（不崩、不返回 undefined）', () => {
    const copy = errorCopy('brand_new_code');
    expect(copy.detailKey).toBe(UNKNOWN_CODE_DETAIL_KEY);
    expect(copy.actionKey).toBe(UNKNOWN_CODE_ACTION_KEY);
    const zhDict = zh as Record<string, string>;
    expect(zhDict[UNKNOWN_CODE_DETAIL_KEY]).toContain('{{code}}');
  });
});

describe('服务重启与令牌错误', () => {
  const missingToken = new AgmgrApiError({
    code: 'unauthorized',
    message: 'no admin token available on this page',
    adminTokenProblem: 'missing',
  });
  const rejectedToken = new AgmgrApiError({
    code: 'unauthorized',
    message: '缺少或错误的管理令牌',
    status: 401,
    adminTokenProblem: 'rejected',
  });
  const otherError = new AgmgrApiError({ code: 'storage', message: '磁盘写满', status: 500 });

  it('令牌缺失与被拒绝都算「服务已重启」，并给出刷新页面提示', () => {
    expect(isServiceRestart(missingToken)).toBe(true);
    expect(isServiceRestart(rejectedToken)).toBe(true);
    expect(serviceRestartHintKey(rejectedToken)).toBe(SERVICE_RESTART_KEY);
    expect((zh as Record<string, string>)[SERVICE_RESTART_KEY]).toBe('本地服务已重启，请刷新页面');
  });

  it('其他错误不会被误报成「服务已重启」', () => {
    expect(isServiceRestart(otherError)).toBe(false);
    expect(serviceRestartHintKey(otherError)).toBeNull();
    expect(isServiceRestart(new Error('boom'))).toBe(false);
    expect(isServiceRestart(undefined)).toBe(false);
  });

  it('requestId 缺失时不会渲染出 undefined', () => {
    expect(rejectedToken.requestId).toBeUndefined();
  });
});

describe('错误归一化', () => {
  it('任意抛出物都能变成 AgmgrApiError', () => {
    const original = new AgmgrApiError({ code: 'storage', message: 'x' });
    expect(toAgmgrApiError(original)).toBe(original);
    expect(toAgmgrApiError(new Error('boom')).code).toBe('internal');
    expect(toAgmgrApiError('boom').message).toBe('boom');
    expect(toAgmgrApiError(new Error('boom')).name).toBe('AgmgrApiError');
  });
});

describe('受保护写操作终态可信度（UC-098）', () => {
  it('valid envelope errors are terminal only when the reserved request identity matches', () => {
    const terminal = new AgmgrApiError({
      code: 'storage',
      message: 'write rejected',
      requestId: 'op-1',
      responseEnvelopeValid: true,
      responseIdentityMatches: true,
    });
    expect(isOperationOutcomeUnknown(terminal)).toBe(false);
    expect(isOperationOutcomeUnknown(terminal, { requestId: 'op-1' })).toBe(false);

    const mismatch = new AgmgrApiError({
      code: 'storage',
      message: 'write rejected',
      requestId: 'other-request',
      responseEnvelopeValid: true,
      responseIdentityMatches: true,
    });
    expect(isOperationOutcomeUnknown(mismatch, { requestId: 'op-1' })).toBe(true);

    const missingIdentity = new AgmgrApiError({
      code: 'storage',
      message: 'write rejected',
      responseEnvelopeValid: true,
    });
    expect(isOperationOutcomeUnknown(missingIdentity, { requestId: 'op-1' })).toBe(true);
    expect(isOperationOutcomeUnknown(new Error('transport failed'), { requestId: 'op-1' })).toBe(
      true,
    );
  });
});
