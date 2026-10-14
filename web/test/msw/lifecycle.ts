import { afterAll, afterEach, beforeAll } from 'vitest';
import { resetMockState } from './handlers';
import { server } from './server';

// msw 的生命周期与「每个测试之间重置可变状态」：状态重置保证用例互不串味。
beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' });
});

afterEach(() => {
  server.resetHandlers();
  resetMockState();
});

afterAll(() => {
  server.close();
});
