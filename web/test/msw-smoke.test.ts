import { describe, expect, it } from 'vitest';
import { listTools } from '../src/api/endpoints';
import { setMetaToken } from './token';
import './msw/lifecycle';

/**
 * msw 冒烟：证明契约 mock 与本仓库的 API 客户端能对上（含**相对 URL** 的处理）。
 * 这条一旦红了，后面所有页面测试的失败都不必再猜是页面问题还是 mock 基础设施问题。
 */
describe('msw 契约 mock 冒烟', () => {
  it('客户端用相对 URL 也能被 msw 拦截并解包信封', async () => {
    setMetaToken('tok_test');
    const tools = await listTools();
    expect(tools.length).toBe(10);
    expect(tools[0]?.id).toBe('claude');
  });
});
