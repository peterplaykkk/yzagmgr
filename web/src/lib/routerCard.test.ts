import { describe, expect, it } from 'vitest';
import { routerCardForm } from './routerCard';

/**
 * 「请求映射」卡的消费形态（UC-043 / UC-046）：选择器只对**吃单一模型**的卡有意义。
 *
 * 判据是家族事实且只此一份：claude-desktop（别名集形态）不渲染选择器，其余（四 CLI 家族的
 * 单模型形态）逐字保持 UC-043 行为——漏登记/多登记的代价不对称，故默认单模型。
 */
describe('「请求映射」卡的消费形态（UC-043 / UC-046）', () => {
  it('claude-desktop → 别名集（无单一模型可选，不渲染「使用映射」选择器）', () => {
    expect(routerCardForm('claude-desktop')).toBe('alias-set');
  });

  it('四 CLI 家族 → 单模型（UC-043 选择器行为不变）', () => {
    for (const family of ['claude', 'codex', 'opencode', 'pi']) {
      expect(routerCardForm(family), family).toBe('single-model');
    }
  });

  it('未登记的工具一律按单模型处理（多登记会让单选卡失去选择能力，代价更重）', () => {
    for (const other of ['gemini', 'grokbuild', 'openclaw', 'hermes', 'workbuddy', '']) {
      expect(routerCardForm(other), other).toBe('single-model');
    }
  });
});
