import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './app/App';
// 副作用导入：初始化 i18n（默认简体中文）。
import './i18n';
// 副作用导入：设计 token 的 CSS 变量层（design/tokens.css 的逐值副本；light 为唯一实现基准，
// dark 段随文件保留但不接线）。构建期并入产物，MUI 主题的取值见 src/theme/tokens.ts（同源同步）。
import './styles/yzagmgr-tokens.css';

const container = document.getElementById('root');
if (container === null) {
  throw new Error('缺少 #root 挂载点：请检查 index.html 是否被守护进程原样下发');
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
