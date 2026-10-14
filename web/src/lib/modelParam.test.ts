// `model` 参数解析/归一的纯函数用例（UC-110 A1；与 Go `normalizeModels` 同一形状口径）。

import { describe, expect, it } from 'vitest';
import {
  isCanonicalModelParam,
  isValidModelName,
  readModelSelection,
  readSingleModel,
  setModelParam,
} from './modelParam';

const url = (search: string) => new URLSearchParams(search);

describe('modelParam（UC-110 A1）', () => {
  it('形态判定与 Go modelRe 同形：封闭字符集、≤200 字符、首字符字母数字', () => {
    expect(isValidModelName('model-alpha')).toBe(true);
    expect(isValidModelName('claude-fable-5-1')).toBe(true);
    expect(isValidModelName('deepseek-flash')).toBe(true);
    expect(isValidModelName('a/b:c@d_e+f[g](h)i')).toBe(true);
    expect(isValidModelName('')).toBe(false);
    expect(isValidModelName(' leading')).toBe(false);
    expect(isValidModelName('bad value')).toBe(false);
    expect(isValidModelName('逗号')).toBe(false);
    expect(isValidModelName('-leading')).toBe(false);
    expect(isValidModelName('x'.repeat(201))).toBe(false);
    expect(isValidModelName('x'.repeat(200))).toBe(true);
  });

  it('读取：逗号写法与重复 key 合流，trim/去重/码位升序，非法段剔除', () => {
    expect(readModelSelection(url('model=b&model=a'))).toEqual(['a', 'b']);
    expect(readModelSelection(url('model=b%2Ca'))).toEqual(['a', 'b']);
    expect(readModelSelection(url('model=a%2Ca'))).toEqual(['a']);
    expect(readModelSelection(url('model=%20bad%20value'))).toEqual([]);
    expect(readModelSelection(url('model=a&model=%20bad%20value'))).toEqual(['a']);
    expect(readModelSelection(url('model='))).toEqual([]);
    expect(readModelSelection(url(''))).toEqual([]);
  });

  it('写出：规范形态 = 升序重复 key；空集/非法值删除或忽略', () => {
    expect(setModelParam(url('x=1'), ['b', 'a']).toString()).toBe('x=1&model=a&model=b');
    expect(setModelParam(url('model=zz&x=1'), ['a']).toString()).toBe('x=1&model=a');
    expect(setModelParam(url('model=zz'), []).toString()).toBe('');
    expect(setModelParam(url('model=a'), ['bad value', 'a']).toString()).toBe('model=a');
  });

  it('规范判定：逐位比较（乱序/逗号/重复/非法都算非规范，交给 replace 归一）', () => {
    expect(isCanonicalModelParam(url('model=a&model=b'), ['a', 'b'])).toBe(true);
    expect(isCanonicalModelParam(url('model=b&model=a'), ['a', 'b'])).toBe(false);
    expect(isCanonicalModelParam(url('model=a%2Cb'), ['a', 'b'])).toBe(false);
    expect(isCanonicalModelParam(url(''), [])).toBe(true);
    expect(isCanonicalModelParam(url('model=a'), [])).toBe(false);
  });

  it('明细单值读取：多值/逗号取首个，空值跳过', () => {
    expect(readSingleModel(url('model=claude'))).toBe('claude');
    expect(readSingleModel(url('model=claude&model=gemini'))).toBe('claude');
    expect(readSingleModel(url('model=claude%2Cgemini'))).toBe('claude');
    expect(readSingleModel(url('model=&model=b'))).toBe('b');
    expect(readSingleModel(url(''))).toBe('');
  });
});
