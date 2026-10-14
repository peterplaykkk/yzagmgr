import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import en from './locales/en.json';
import zh from './locales/zh.json';

// i18n：扁平 key（`nav.*` / `title.*` / `page.*` / `states.*` / `error.*` / `tier.*`），
// 与仓库内同为「Go 守护进程 serve 的 React + MUI 站」的 `src/agmem/web` 同构。
//
// - 默认界面语言 **简体中文**（`.agents/skills/dev-web/references/modern-habits.md`）。
// - 缺 key 时回退：先到默认语言，再到 key 本身（`keySeparator: false` 让整串 key 直查，
//   不把 `.` 当层级路径），因此漏翻不会渲染出空白，而是显示可检索的 key。
// - 组件里禁止硬编码用户可见字符串；新文案必须同步 `locales/zh.json`（en 可滞后）。
void i18n.use(initReactI18next).init({
  lng: 'zh',
  fallbackLng: 'zh',
  keySeparator: false,
  nsSeparator: false,
  resources: { en: { translation: en }, zh: { translation: zh } },
  interpolation: { escapeValue: false },
  react: { useSuspense: false },
  // 关掉 i18next 的推广横幅：它会让每次 `npm run test` 都往 stdout 打一段广告，干扰输出预算。
  showSupportNotice: false,
});

export default i18n;
