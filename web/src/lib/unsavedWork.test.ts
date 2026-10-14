import { describe, expect, it, vi } from 'vitest';
import { emptyFields } from './profileForm';
import en from '../locales/en.json';
import zh from '../locales/zh.json';
import {
  PROFILE_FIELD_LABELS,
  UNSAVED_KIND_LABELS,
  dedupeDraftEntries,
  draftDiffRows,
  hasBlockingEntries,
  hasDraftEntries,
  type UnsavedEntry,
} from './unsavedWork';

/**
 * UC-098 A2/A3 的未保存态登记模型（纯函数面）：
 * - 差异行只列与基线不同的字段，字段序与表单词序一致（草稿前后比较的数据源）；
 * - 「有草稿」/「只能等待」两条判据分别对应三选对话框与升级等待说明。
 */

const baseline = { ...emptyFields(), name: 'yzagmgr-glm', endpoint: 'https://gw.example.com/v1' };

function draft(kind: UnsavedEntry['kind'], label = 'x'): UnsavedEntry {
  return { kind, label };
}

describe('unsavedWork（未保存态模型）', () => {
  it('draftDiffRows：只列真正不同的字段（逐字比较），顺序与表单词序一致', () => {
    const rows = draftDiffRows({ ...baseline, name: '草稿名', maxOutputText: '131072' }, baseline);
    expect(rows.map((row) => row.field)).toEqual(['name', 'maxOutputText']);
    expect(rows[0]).toEqual({ field: 'name', saved: 'yzagmgr-glm', draft: '草稿名' });
    expect(rows[1]).toEqual({ field: 'maxOutputText', saved: '', draft: '131072' });
    // 无差异 ⇒ 空（与 isDirty 同判据）。
    expect(draftDiffRows(baseline, baseline)).toEqual([]);
  });

  it('字段标签词表覆盖全部表单字段（i18n key 存在，不复制第二套文案）', () => {
    const fields = Object.keys(emptyFields());
    expect(Object.keys(PROFILE_FIELD_LABELS).sort()).toEqual([...fields].sort());
    for (const key of Object.values(PROFILE_FIELD_LABELS)) {
      expect(key.startsWith('tool.config.dialog.')).toBe(true);
    }
    expect(Object.keys(UNSAVED_KIND_LABELS).sort()).toEqual(['apply', 'draft', 'proxy', 'test']);
    for (const key of Object.values(UNSAVED_KIND_LABELS)) {
      expect(typeof (en as Record<string, unknown>)[key]).toBe('string');
      expect(typeof (zh as Record<string, unknown>)[key]).toBe('string');
    }
  });

  it('两条判据：draft ⇒ 三选；apply/test ⇒ 只能等待（升级等待说明）', () => {
    expect(hasDraftEntries([draft('draft')])).toBe(true);
    expect(hasBlockingEntries([draft('draft')])).toBe(false);
    expect(hasDraftEntries([draft('apply')])).toBe(false);
    expect(hasBlockingEntries([draft('apply')])).toBe(true);
    expect(hasBlockingEntries([draft('test')])).toBe(true);
    expect(hasDraftEntries([])).toBe(false);
    expect(hasBlockingEntries([])).toBe(false);
    // 混合：两条判据各自成立。
    expect(hasDraftEntries([draft('apply'), draft('draft')])).toBe(true);
    expect(hasBlockingEntries([draft('apply'), draft('draft')])).toBe(true);
  });

  it('登记条目可带处置句柄：persist/discard 只对草稿有意义（模型不强制）', () => {
    const persist = vi.fn();
    const discard = vi.fn();
    const entry: UnsavedEntry = { kind: 'draft', label: 'x', persist, discard };
    entry.persist?.();
    entry.discard?.();
    expect(persist).toHaveBeenCalledTimes(1);
    expect(discard).toHaveBeenCalledTimes(1);
    const blocking: UnsavedEntry = { kind: 'test', label: 'y' };
    expect(blocking.persist).toBeUndefined();
    expect(blocking.discard).toBeUndefined();
  });

  it('dedupeDraftEntries：同目标的活表单与保留草稿只留活表单（最新内容）；其余原样', () => {
    const live: UnsavedEntry = { kind: 'draft', label: '卡', draftKey: 'edit:p1' };
    const retained: UnsavedEntry = {
      kind: 'draft',
      label: '卡',
      draftKey: 'edit:p1',
      retained: true,
    };
    const otherRetained: UnsavedEntry = {
      kind: 'draft',
      label: '别的卡',
      draftKey: 'edit:p2',
      retained: true,
    };
    const apply: UnsavedEntry = { kind: 'apply', label: 'Codex' };
    const keyless: UnsavedEntry = { kind: 'draft', label: '无键草稿' };

    // 活表单在场 ⇒ 同目标的保留草稿让位；其余条目（其他目标 / 无键 / 在飞）原样保留。
    const mixed = dedupeDraftEntries([live, retained, otherRetained, apply, keyless]);
    expect(mixed).toEqual([live, otherRetained, apply, keyless]);

    // 活表单缺席 ⇒ 保留草稿保留（顺序不变）。
    expect(dedupeDraftEntries([retained, apply])).toEqual([retained, apply]);

    // retained 不是 true（活表单）即使同键也不互相吞。
    const live2: UnsavedEntry = { kind: 'draft', label: '卡2', draftKey: 'edit:p1' };
    expect(dedupeDraftEntries([live, live2])).toEqual([live, live2]);

    // 无重复时返回等值副本（不原地改）。
    const source = [retained, apply];
    const result = dedupeDraftEntries(source);
    expect(result).toEqual(source);
    expect(result).not.toBe(source);
  });
});
