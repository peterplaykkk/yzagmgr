import { describe, expect, it } from 'vitest';
import type { RemapModelEntry } from '../api/types';
import {
  readRouterSort,
  readRouterSortDir,
  routerSortKeyOf,
  sortArmsForView,
  sortModelEntriesForView,
} from './routerSort';

/**
 * UC-096 A7 的排序口径（C-6 · web-c6-096a7 lane）：
 * 每个键只作用于拥有该列的列表；未排序/异键时保持传入相对序（= 服务端序）；
 * 码元序比较、稳定排序、只重排引用不改传入数组/对象。
 */

function entry(alias: string, target: string): RemapModelEntry {
  return { alias, target };
}

describe('readRouterSort / readRouterSortDir（URL 编解码）', () => {
  it('缺省 = 未排序（value=null 且合法）；合法键直读', () => {
    expect(readRouterSort(null)).toEqual({ value: null, valid: true });
    expect(readRouterSort('name')).toEqual({ value: 'name', valid: true });
    expect(readRouterSort('alias')).toEqual({ value: 'alias', valid: true });
    expect(readRouterSort('target')).toEqual({ value: 'target', valid: true });
  });

  it('非法值回落未排序并标 valid=false（调用方据此 replace 归一）', () => {
    expect(readRouterSort('bogus')).toEqual({ value: null, valid: false });
    expect(readRouterSort('')).toEqual({ value: null, valid: false });
  });

  it('方向：缺省/合法直读，非法值回落 asc 且标 valid=false', () => {
    expect(readRouterSortDir(null)).toEqual({ value: 'asc', valid: true });
    expect(readRouterSortDir('asc')).toEqual({ value: 'asc', valid: true });
    expect(readRouterSortDir('desc')).toEqual({ value: 'desc', valid: true });
    expect(readRouterSortDir('sideways')).toEqual({ value: 'asc', valid: false });
  });

  it('列头 aria-sort 取值：命中列给方向（ARIA 词表），其余 false（不冒充当前排序列）', () => {
    expect(routerSortKeyOf(null, 'name')).toBe(false);
    expect(routerSortKeyOf({ key: 'alias', dir: 'asc' }, 'name')).toBe(false);
    expect(routerSortKeyOf({ key: 'alias', dir: 'asc' }, 'alias')).toBe('ascending');
    expect(routerSortKeyOf({ key: 'alias', dir: 'desc' }, 'alias')).toBe('descending');
  });
});

describe('sortArmsForView（请求映射列表：name 键专用）', () => {
  const arms = [{ arm: { name: 'zulu' } }, { arm: { name: 'alpha' } }, { arm: { name: 'mike' } }];

  it('未排序 / 非 name 键：保持传入相对序（= 服务端序），返回副本', () => {
    const names = (items: readonly { arm: { name: string } }[]) =>
      items.map((item) => item.arm.name);
    expect(names(sortArmsForView(arms, null))).toEqual(['zulu', 'alpha', 'mike']);
    expect(names(sortArmsForView(arms, { key: 'alias', dir: 'desc' }))).toEqual([
      'zulu',
      'alpha',
      'mike',
    ]);
    expect(names(sortArmsForView(arms, { key: 'target', dir: 'asc' }))).toEqual([
      'zulu',
      'alpha',
      'mike',
    ]);
  });

  it('name asc / desc：码元序全序（不是只看首行）', () => {
    const names = (items: readonly { arm: { name: string } }[]) =>
      items.map((item) => item.arm.name);
    expect(names(sortArmsForView(arms, { key: 'name', dir: 'asc' }))).toEqual([
      'alpha',
      'mike',
      'zulu',
    ]);
    expect(names(sortArmsForView(arms, { key: 'name', dir: 'desc' }))).toEqual([
      'zulu',
      'mike',
      'alpha',
    ]);
  });

  it('只重排引用：不改传入数组、元素是同一引用；空列表原样返回空', () => {
    const before = arms.map((item) => item.arm.name);
    const sorted = sortArmsForView(arms, { key: 'name', dir: 'desc' });
    expect(arms.map((item) => item.arm.name)).toEqual(before);
    expect(sorted).not.toBe(arms);
    expect(sorted[0]).toBe(arms[0]); // zulu（原 index 0）
    expect(sortArmsForView([], { key: 'name', dir: 'asc' })).toEqual([]);
  });
});

describe('sortModelEntriesForView（模型映射表：alias/target 键）', () => {
  // 别名升序与目标升序**故意不同**：识别「按哪一列排」。
  const entries = [entry('mid', 'z-up'), entry('alpha', 'a-up'), entry('zeta', 'm-up')];
  const aliases = (items: readonly RemapModelEntry[]) => items.map((item) => item.alias);

  it('未排序 / name 键：保持传入相对序（name 是请求映射列表的键，不串扰映射表）', () => {
    expect(aliases(sortModelEntriesForView(entries, null))).toEqual(['mid', 'alpha', 'zeta']);
    expect(aliases(sortModelEntriesForView(entries, { key: 'name', dir: 'asc' }))).toEqual([
      'mid',
      'alpha',
      'zeta',
    ]);
  });

  it('alias asc / desc：按别名码元序', () => {
    expect(aliases(sortModelEntriesForView(entries, { key: 'alias', dir: 'asc' }))).toEqual([
      'alpha',
      'mid',
      'zeta',
    ]);
    expect(aliases(sortModelEntriesForView(entries, { key: 'alias', dir: 'desc' }))).toEqual([
      'zeta',
      'mid',
      'alpha',
    ]);
  });

  it('target asc / desc：按目标码元序（与 alias 序明显不同）', () => {
    expect(aliases(sortModelEntriesForView(entries, { key: 'target', dir: 'asc' }))).toEqual([
      'alpha',
      'zeta',
      'mid',
    ]);
    expect(aliases(sortModelEntriesForView(entries, { key: 'target', dir: 'desc' }))).toEqual([
      'mid',
      'zeta',
      'alpha',
    ]);
  });

  it('同键（同名）保持传入相对序（稳定排序）；不改传入数组与条目对象', () => {
    const tied = [entry('same', 'up-b'), entry('same', 'up-a')];
    expect(sortModelEntriesForView(tied, { key: 'alias', dir: 'asc' })).toEqual(tied);
    const before = [...entries];
    const sorted = sortModelEntriesForView(entries, { key: 'target', dir: 'desc' });
    expect(entries).toEqual(before);
    expect(sorted[0]).toBe(entries[0]); // mid: 元素是同一引用（只重排）
    expect(sortModelEntriesForView([], { key: 'alias', dir: 'asc' })).toEqual([]);
  });
});
