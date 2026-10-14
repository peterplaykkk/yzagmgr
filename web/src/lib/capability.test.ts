import { describe, expect, it } from 'vitest';
import { AgmgrApiError } from '../api/errors';
import {
  ackCodes,
  acksBoundToForm,
  applicableFactRows,
  capabilitiesEqual,
  capabilitiesToForm,
  checkLabelKey,
  hasDeclaredCapabilities,
  normalizeDeclaredCapabilities,
  parseCapabilityConflict,
  resolutionLabelKey,
  severityLabelKey,
} from './capability';

/**
 * UC-094 A7 能力声明/校验的纯判据（`doc/api-v1.md` §4.2 additive）：
 * - `resolution=unknown` ⇒ `applicable` 整块缺席 ⇒ 四行事实全是「未知」（**不填最大值**）；
 * - 声明相等判据（表单脏判据）；既有确认的绑定判据（模型/声明一变即失效）。
 */

function capabilityError(details: Record<string, unknown> | undefined): AgmgrApiError {
  return new AgmgrApiError({
    code: 'capability_conflict',
    message: '能力冲突',
    status: 409,
    ...(details === undefined ? {} : { details }),
  });
}

describe('capability · 冲突解析', () => {
  it('解析 details.report 与 confirmRequired（非本码 / 形态不合约 ⇒ null）', () => {
    const report = {
      model: 'no-vision',
      resolution: 'model',
      applicable: { contextWindow: 1000000, maxOutput: 131072, supportsImages: false },
      checks: [
        {
          code: 'vision_unsupported',
          severity: 'confirm',
          model: 'no-vision',
          detail: '所声明的模态含 vision…',
        },
      ],
    };
    const parsed = parseCapabilityConflict(
      capabilityError({ report, confirmRequired: ['vision_unsupported'] }),
    );
    expect(parsed).not.toBeNull();
    expect(parsed?.report.resolution).toBe('model');
    expect(parsed?.confirmRequired).toEqual(['vision_unsupported']);

    expect(
      parseCapabilityConflict(new AgmgrApiError({ code: 'internal', message: 'x' })),
    ).toBeNull();
    expect(parseCapabilityConflict(capabilityError(undefined))).toBeNull();
    expect(
      parseCapabilityConflict(capabilityError({ report: { resolution: 'model' } })),
    ).toBeNull();
    expect(parseCapabilityConflict(capabilityError({ confirmRequired: [] }))).toBeNull();
    // confirmRequired 里的非字符串条目被滤掉（不把坏形态带进界面）。
    const filtered = parseCapabilityConflict(
      capabilityError({ report, confirmRequired: ['vision_unsupported', 7] }),
    );
    expect(filtered?.confirmRequired).toEqual(['vision_unsupported']);
  });

  it('词条 key 映射：封闭枚举给 key，未知取值给 null（显示原始值，不编造译名）', () => {
    expect(resolutionLabelKey('model')).toBe('capability.resolution.model');
    expect(resolutionLabelKey('default')).toBe('capability.resolution.default');
    expect(resolutionLabelKey('unknown')).toBe('capability.resolution.unknown');
    expect(resolutionLabelKey('weird')).toBeNull();
    expect(checkLabelKey('vision_unsupported')).toBe('capability.check.vision_unsupported');
    expect(checkLabelKey('thinking_unsupported')).toBe('capability.check.thinking_unsupported');
    expect(checkLabelKey('capability_unknown')).toBe('capability.check.capability_unknown');
    expect(checkLabelKey('other')).toBeNull();
    expect(severityLabelKey('confirm')).toBe('capability.severity.confirm');
    expect(severityLabelKey('unknown')).toBe('capability.severity.unknown');
    expect(severityLabelKey('info')).toBeNull();
  });
});

describe('capability · 适用能力事实（未知不自动填最大值）', () => {
  it('applicable 缺席（resolution=unknown）⇒ 四行全未知，没有任何数字可填', () => {
    const rows = applicableFactRows(undefined);
    expect(rows.map((row) => row.value.kind)).toEqual(['unknown', 'unknown', 'unknown', 'unknown']);
    expect(rows.map((row) => row.field)).toEqual([
      'contextWindow',
      'maxOutput',
      'supportsImages',
      'supportsReasoning',
    ]);
  });

  it('逐字段呈现：数字/布尔按事实；单字段缺席只影响那一行（仍为未知）', () => {
    const rows = applicableFactRows({
      contextWindow: 1000000,
      supportsImages: false,
      supportsReasoning: true,
      // maxOutput 刻意缺席
    });
    const byField = Object.fromEntries(rows.map((row) => [row.field, row.value]));
    expect(byField['contextWindow']).toEqual({ kind: 'number', value: 1000000 });
    expect(byField['maxOutput']).toEqual({ kind: 'unknown' });
    expect(byField['supportsImages']).toEqual({ kind: 'boolean', value: false });
    expect(byField['supportsReasoning']).toEqual({ kind: 'boolean', value: true });
  });
});

describe('capability · 声明与确认绑定', () => {
  it('声明规整：模态按词表固定序、空声明 = undefined（不伪造空对象）', () => {
    expect(normalizeDeclaredCapabilities([], '')).toBeUndefined();
    expect(normalizeDeclaredCapabilities(['vision', 'text'], ' high ')).toEqual({
      modalities: ['text', 'vision'],
      thinkingTier: 'high',
    });
    expect(normalizeDeclaredCapabilities(['vision'], '')).toEqual({ modalities: ['vision'] });
    expect(normalizeDeclaredCapabilities([], 'xhigh')).toEqual({ thinkingTier: 'xhigh' });
    expect(hasDeclaredCapabilities(undefined)).toBe(false);
    expect(hasDeclaredCapabilities({})).toBe(false);
    expect(hasDeclaredCapabilities({ modalities: ['vision'] })).toBe(true);
  });

  it('声明相等：顺序无关、空白无关；capabilitiesToForm 是往返可逆的', () => {
    expect(capabilitiesEqual({ modalities: ['vision'] }, { modalities: ['vision'] })).toBe(true);
    expect(
      capabilitiesEqual({ modalities: ['text', 'vision'] }, { modalities: ['vision', 'text'] }),
    ).toBe(true);
    expect(capabilitiesEqual(undefined, {})).toBe(true);
    expect(
      capabilitiesEqual(
        { modalities: ['vision'], thinkingTier: 'high' },
        { modalities: ['vision'] },
      ),
    ).toBe(false);
    const form = capabilitiesToForm({ modalities: ['vision', 'text'], thinkingTier: ' high ' });
    expect(form).toEqual({ modalities: ['text', 'vision'], thinkingTier: 'high' });
    expect(normalizeDeclaredCapabilities(form.modalities, form.thinkingTier)).toEqual({
      modalities: ['text', 'vision'],
      thinkingTier: 'high',
    });
  });

  it('既有确认的绑定判据：模型或声明任一变化即不再绑定（呈现一致，不把失效确认当有效）', () => {
    const card = { modelId: 'no-vision', capabilities: { modalities: ['vision'] } };
    expect(
      acksBoundToForm({ modelId: 'no-vision', capabilities: { modalities: ['vision'] } }, card),
    ).toBe(true);
    expect(
      acksBoundToForm({ modelId: 'other', capabilities: { modalities: ['vision'] } }, card),
    ).toBe(false);
    expect(acksBoundToForm({ modelId: 'no-vision', capabilities: undefined }, card)).toBe(false);
    expect(
      acksBoundToForm({ modelId: ' no-vision ', capabilities: { modalities: ['vision'] } }, card),
    ).toBe(true);
  });

  it('确认码表去重且稳定序（逐条勾选的呈现顺序确定）', () => {
    expect(ackCodes(['thinking_unsupported', 'vision_unsupported', 'vision_unsupported'])).toEqual([
      'thinking_unsupported',
      'vision_unsupported',
    ]);
  });
});
