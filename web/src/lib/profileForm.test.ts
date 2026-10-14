import { describe, expect, it } from 'vitest';
import type { Profile } from '../api/types';
import {
  baselineFields,
  capabilityFormSnapshotText,
  emptyFields,
  endpointProblem,
  fieldsEqual,
  isDirty,
  looksLikeMask,
  nameProblem,
  parseCapabilityFormSnapshot,
  secretProblem,
} from './profileForm';

/**
 * 配置编辑表单的纯判据（UC-094 A2/A4/A9）。
 *
 * 口径（与 `doc/api-v1.md` §4.2/§4.7 同向）：
 * - 端点：留空合法；否则必须是绝对 http(s) URL、不内嵌凭据；
 * - 名称：trim 后必填；同工具重名（trim 精确串）就地报；
 * - 密钥：掩码形态（含 `****` 或恰为 `<已脱敏>`）不作为新密钥保存；控制字符先拦；
 * - 草稿：脏判据 = 与已保存值的字段快照逐字比较。
 */

const CARD: Profile = {
  id: 'user-pi-1',
  name: 'yzagmgr-glm',
  kind: 'user',
  active: false,
  tier: 'gateway',
  modelId: 'glm-5.3-flash',
  models: ['glm-5.3-flash', 'bd-glm-5.3-flash'],
  editable: true,
  deletable: true,
  hasUsageScript: false,
  endpoint: 'https://gw.example.com/v1',
  credRef: 'yzagmgr-managed-token://mt-1',
  maxOutputTokens: 131072,
  capabilities: { modalities: ['vision', 'text'], thinkingTier: 'medium' },
};

describe('profileForm · 名称（A2）', () => {
  it('空/空白 = required；与同工具其他卡 trim 后同名 = duplicate；否则通过', () => {
    expect(nameProblem('', [])).toBe('required');
    expect(nameProblem('   ', ['x'])).toBe('required');
    expect(nameProblem('yzagmgr', ['yzagmgr'])).toBe('duplicate');
    expect(nameProblem(' yzagmgr ', ['yzagmgr'])).toBe('duplicate');
    expect(nameProblem('yzagmgr', ['yzagmgr-glm', 'other'])).toBeNull();
    // 大小写不同 = 不同名（服务端规则是权威；这里不做过度拦截）。
    expect(nameProblem('GLM', ['glm'])).toBeNull();
  });
});

describe('profileForm · 端点（A2）', () => {
  it('留空合法（不登记/不修改）；非 URL 与非 http(s) 报 format', () => {
    expect(endpointProblem('')).toBeNull();
    expect(endpointProblem('   ')).toBeNull();
    for (const bad of ['gw.example.com/v1', 'ftp://gw.example.com', 'https://', 'not a url']) {
      expect(endpointProblem(bad), bad).toBe('format');
    }
  });

  it('绝对 http(s) URL 通过（含回环与 IPv6 回环）；内嵌凭据报 credentials', () => {
    expect(endpointProblem('https://gw.example.com/v1')).toBeNull();
    expect(endpointProblem('http://127.0.0.1:17021/v1')).toBeNull();
    expect(endpointProblem('http://[::1]:17021/v1')).toBeNull();
    expect(endpointProblem('https://user:pass@gw.example.com/v1')).toBe('credentials');
    expect(endpointProblem('https://user@gw.example.com/v1')).toBe('credentials');
  });
});

describe('profileForm · 一次性密钥（A9）', () => {
  it('空 = required；掩码形态 = mask；控制字符（含尾随换行）= control；否则通过', () => {
    expect(secretProblem('')).toBe('required');
    expect(secretProblem('  ')).toBe('required');
    expect(secretProblem('sk-****a1b2')).toBe('mask');
    expect(secretProblem('<已脱敏>')).toBe('mask');
    expect(secretProblem('sk-live-abcdef123456\n')).toBe('control');
    expect(secretProblem('sk-live-abcdef123456')).toBeNull();
    expect(looksLikeMask('sk-live-abcdef123456')).toBe(false);
    expect(looksLikeMask('tp-' + '****' + 'zzzz')).toBe(true);
  });
});

describe('profileForm · 草稿脏判据（A4）', () => {
  it('基线：edit 取回读值逐字落成并保留 canonical capabilities，create 全空；models 以换行连接', () => {
    const base = baselineFields(CARD);
    expect(base).toEqual({
      name: 'yzagmgr-glm',
      modelId: 'glm-5.3-flash',
      modelsText: 'glm-5.3-flash\nbd-glm-5.3-flash',
      endpoint: 'https://gw.example.com/v1',
      endpointIsFullBase: false,
      credRef: 'yzagmgr-managed-token://mt-1',
      maxOutputText: '131072',
      capabilities: JSON.stringify({ modalities: ['text', 'vision'], thinkingTier: 'medium' }),
    });
    expect(baselineFields(null)).toEqual(emptyFields());
    // 未声明 maxOutputTokens（键缺席）⇒ 空串基线（不是 '0'——不伪造声明）。
    const noMax: Profile = { ...CARD, maxOutputTokens: undefined };
    expect(baselineFields(noMax).maxOutputText).toBe('');
    const noEndpointOrCredential: Profile = { ...CARD, endpoint: undefined, credRef: undefined };
    expect(baselineFields(noEndpointOrCredential)).toMatchObject({ endpoint: '', credRef: '' });

    // 表单快照只含非秘密的凭据引用与声明字段；不保存 secret 或 test proof/result。
    expect(Object.keys(base).sort()).toEqual([
      'capabilities',
      'credRef',
      'endpoint',
      'endpointIsFullBase',
      'maxOutputText',
      'modelId',
      'modelsText',
      'name',
    ]);
    expect(base).not.toHaveProperty('secret');
    expect(base).not.toHaveProperty('testResult');
    expect(base).not.toHaveProperty('draftFingerprint');
  });

  it('dirty = 任一字段与基线逐字不同；与基线相同（含手动改回去）不算脏', () => {
    const base = baselineFields(CARD);
    expect(isDirty(base, base)).toBe(false);
    expect(fieldsEqual(base, { ...base })).toBe(true);
    expect(isDirty({ ...base, name: 'yzagmgr-glm-2' }, base)).toBe(true);
    expect(isDirty({ ...base, endpoint: '' }, base)).toBe(true);
    expect(isDirty({ ...base, maxOutputText: '' }, base)).toBe(true);
    expect(
      isDirty({ ...base, capabilities: capabilityFormSnapshotText(['vision'], 'high') }, base),
    ).toBe(true);
  });
});

describe('profileForm · 能力快照（UC-094 A7 / UC-098 A2 v3）', () => {
  it('创建稳定、排序并去重的 JSON；解析时裁剪档位空格', () => {
    const text = capabilityFormSnapshotText(['vision', 'text', 'vision', 'unknown'], ' high ');
    expect(text).toBe(JSON.stringify({ modalities: ['text', 'vision'], thinkingTier: 'high' }));
    expect(parseCapabilityFormSnapshot(text)).toEqual({
      modalities: ['text', 'vision'],
      thinkingTier: 'high',
    });
  });

  it('损坏/不完整/异构/重复快照拒绝；合法的非 canonical 顺序会规范化', () => {
    for (const malformed of [
      '{bad json',
      'null',
      '[]',
      JSON.stringify({ modalities: 'text', thinkingTier: 'high' }),
      JSON.stringify({ modalities: ['text'] }),
      JSON.stringify({ modalities: ['audio'], thinkingTier: 'high' }),
      JSON.stringify({ modalities: ['text', 'text'], thinkingTier: 'high' }),
    ]) {
      expect(parseCapabilityFormSnapshot(malformed), malformed).toBeNull();
    }
    expect(
      parseCapabilityFormSnapshot(
        JSON.stringify({ modalities: ['vision', 'text'], thinkingTier: ' high ' }),
      ),
    ).toEqual({ modalities: ['text', 'vision'], thinkingTier: 'high' });
  });
});
