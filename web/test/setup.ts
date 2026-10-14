import '@testing-library/jest-dom/vitest';
import { cleanup, configure } from '@testing-library/react';
import { afterAll, afterEach, beforeAll } from 'vitest';
// 副作用导入：所有测试都在简体中文界面下断言（与生产默认一致）。
import '../src/i18n';

/**
 * jsdom 的 AbortController/AbortSignal 与 undici 的 brand check 不兼容（测试专用垫片）。
 *
 * 现象：Node 的 `fetch`（undici）只接受**它自己**创建的 AbortSignal；jsdom 装到全局的
 * AbortController 是另一套 JS 实现，把它的 signal 传进 `Request` 会抛
 * 「Expected signal to be an instance of AbortSignal」。msw 的拦截器会用全局 `Request`
 * 重建请求（`FetchRequest extends Request`），因此**任何带 signal 的 fetch 在测试里都会炸**。
 *
 * 处置：测试环境下把全局 `Request` 换成一个会**丢弃 signal** 的子类——msw 继承它，
 * 于是重建请求时不再做 brand check。丢弃 signal 只影响「测试里提前中断请求」这一能力：
 * 数据层仍然按 `signal.aborted` 忽略过期响应，因此「旧请求不得落到新筛选下」依旧被验证。
 * 生产代码不受影响（浏览器里 signal 正常工作）。
 */
const NativeRequest = globalThis.Request;
class TestRequest extends NativeRequest {
  constructor(input: RequestInfo | URL, init?: RequestInit) {
    if (init?.signal !== undefined && init.signal !== null) {
      const { signal: _ignored, ...rest } = init;
      super(input, rest);
      return;
    }
    super(input, init);
  }
}
globalThis.Request = TestRequest as unknown as typeof Request;

// 路由级 code splitting 之后，每个页面的 chunk 要在断言前动态 import：
// 默认 1s 的异步等待在并发跑多个测试文件时会被 chunk 转译挤爆（表现为「找不到本来存在的文本」）。
// 这里放宽到 5s——等待的是**加载**，不是改变断言强度。
configure({ asyncUtilTimeout: 5000 });

// RTL 的自动清理需要全局 afterEach；这里显式注册，避免依赖 vitest globals 开关。
afterEach(() => {
  cleanup();
});

/**
 * 过滤 jsdom 的一条已知噪音：MUI 的 `CssBaseline`（enableColorScheme）会写出
 * `-webkit-print-color-scheme` 一类的声明，jsdom 的 CSS 解析器读不懂就把它整张表丢掉并
 * 走 jsdomError → console.error。它不影响任何断言（jsdom 本来也不做布局），但会把每个
 * render 变成一条 stderr，淹没真正的 React 警告。
 *
 * 只屏蔽这一条**确切消息**，其它 console.error 原样透出。
 */
const CSS_NOISE = 'Could not parse CSS stylesheet';
const originalConsoleError = console.error.bind(console);

beforeAll(() => {
  console.error = (...args: unknown[]) => {
    if (typeof args[0] === 'string' && args[0].includes(CSS_NOISE)) return;
    if (args[0] instanceof Error && args[0].message.includes(CSS_NOISE)) return;
    originalConsoleError(...args);
  };
});

afterAll(() => {
  console.error = originalConsoleError;
});
