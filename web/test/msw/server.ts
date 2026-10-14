import { setupServer } from 'msw/node';
import { handlers } from './handlers';

/**
 * msw 的 Node 侧 mock server。
 *
 * `onUnhandledRequest: 'error'`：任何**没被处理器覆盖**的请求都会让测试直接失败——
 * 这样页面若偷偷打了契约外的端点（或我在 handler 里漏了某个契约端点）会立刻暴露，
 * 而不是静默返回 404 让断言以别的方式飘红。
 */
export const server = setupServer(...handlers);
