import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// 管理页由 yzagmgr 守护进程在**根路径** serve。
//
// base 必须是绝对的 `/`：本应用用普通 history 路由，深链接（`/tools/claude`、
// `/usage/requests/rq_1`）会被守护进程回落成 app shell；若 base 写成相对值（`./`），
// shell 里的资源引用会相对当前路径解析成 `/tools/assets/…`，而守护进程对「缺失的静态资源
// 绝不下发 HTML」（IA §1「证据回退」），页面会直接白屏。根路径部署下 `/` 同时满足
// 「深链接可用」与「守护进程从根 serve」两个要求。
export default defineConfig({
  base: '/',
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    // 开发期把 API 反代到本机守护进程。端口由守护进程决定（启动时选定，不静默换端口），
    // 默认取 IA 线框图中的示例端口；本机端口不同时改这里一处即可（刻意不读 process.env：
    // 那需要给全项目引入 Node 全局类型，而浏览器侧代码不应看见 Node 全局）。
    proxy: {
      '/api': 'http://127.0.0.1:16741',
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./test/setup.ts'],
    globals: false,
    restoreMocks: true,
    // 每个测试自行 stub 全局 fetch；跑完必须收回，避免跨文件污染。
    unstubGlobals: true,
  },
});
