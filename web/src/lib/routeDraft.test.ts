import { describe, expect, it } from 'vitest';
import { effortRemapDraft, modelRemapDraft, type DraftArmSource } from './routeDraft';

/**
 * UC-096 A5 的草稿组装：**段级整体替换**语义下必须把当前全量摆出来再叠加本次编辑；
 * 任何一条表读不到（null）⇒ 不能组装（返回 null，调用方禁用预检，不拿半份草稿预览）。
 */

const ARMS: DraftArmSource[] = [
  {
    name: 'mimo',
    models: [{ alias: 'claude-fable-5-mp', target: 'mimo-v2.6-pro' }],
    effort: { items: [{ from: 'xhigh', to: 'high' }], forceEffort: '' },
  },
  {
    name: 'df',
    models: [{ alias: 'deepseek-flash', target: 'deepseek-flash-0324' }],
    effort: { items: [], forceEffort: 'max' },
  },
];

describe('routeDraft · 模型映射草稿段', () => {
  it('全量抄入各臂 + 目标臂叠加（新增）', () => {
    const draft = modelRemapDraft(ARMS, 'df', { alias: 'new-alias', target: 'new-target' });
    expect(draft).not.toBeNull();
    expect(Object.keys(draft ?? {}).sort()).toEqual(['df', 'mimo']);
    expect(draft?.['mimo']).toEqual([{ alias: 'claude-fable-5-mp', target: 'mimo-v2.6-pro' }]);
    expect(draft?.['df']).toEqual([
      { alias: 'deepseek-flash', target: 'deepseek-flash-0324' },
      { alias: 'new-alias', target: 'new-target' },
    ]);
  });

  it('目标臂上同名 alias 覆盖（与写链 upsert 同判），位置不变', () => {
    const draft = modelRemapDraft(ARMS, 'mimo', {
      alias: 'claude-fable-5-mp',
      target: 'mimo-v2.6-pro-plus',
    });
    expect(draft?.['mimo']).toEqual([{ alias: 'claude-fable-5-mp', target: 'mimo-v2.6-pro-plus' }]);
    // 其它臂逐字不动。
    expect(draft?.['df']).toEqual([{ alias: 'deepseek-flash', target: 'deepseek-flash-0324' }]);
  });

  it('任何一条映射表读不到（null）⇒ 组装失败（返回 null）', () => {
    const broken: DraftArmSource[] = [ARMS[0]!, { ...ARMS[1]!, models: null }];
    expect(modelRemapDraft(broken, 'mimo', { alias: 'a', target: 'b' })).toBeNull();
  });
});

describe('routeDraft · effort 草稿段', () => {
  it('全量抄入（items + forceEffort）+ 目标臂按 from 覆盖/新增', () => {
    const draft = effortRemapDraft(ARMS, 'mimo', { from: '', to: 'low' });
    expect(draft?.['mimo']).toEqual({
      items: [
        { from: 'xhigh', to: 'high' },
        { from: '', to: 'low' },
      ],
      forceEffort: '',
    });
    // 其它臂的 forceEffort 逐字保留。
    expect(draft?.['df']).toEqual({ items: [], forceEffort: 'max' });
  });

  it('同一 from 覆盖（effort 没有删除端点，改回即覆盖为恒等条目）', () => {
    const draft = effortRemapDraft(ARMS, 'mimo', { from: 'xhigh', to: 'xhigh' });
    expect(draft?.['mimo']).toEqual({ items: [{ from: 'xhigh', to: 'xhigh' }], forceEffort: '' });
  });

  it('任何一条 effort 表读不到（null）⇒ 组装失败（返回 null）', () => {
    const broken: DraftArmSource[] = [{ ...ARMS[0]!, effort: null }, ARMS[1]!];
    expect(effortRemapDraft(broken, 'mimo', { from: '', to: 'low' })).toBeNull();
  });
});
