// `/diagnostics` 本机诊断（独立页）。
//
// 页面契约（本波冻结面）：
// - slim 页头：h1 = `title.diag`（「本机诊断」，与诊断面同文；`diagnostics.title` 的区块标题
//   随 `MachineDiagnostics` 自身区块头保留）；面包屑「配置切换」是返回工作台的**真链接**
//   （与工具详情页同一约定：面包屑首项就是上位视图）。
// - 正文 = `MachineDiagnostics` **全量内容**（六类故障 × 作用范围 × 可行下一步、环境覆盖对照、
//   诊断包复制、离线重连、建议修复预览与确认、逐项恢复对照）。「重新检测 / 复制诊断包」动作
//   由该组件自己的区块头承载——它们与组件内部状态强耦合（探测在飞态、bundle 预取态），
//   页面不复制第二枚同文案按钮（先例：workbench 区块动作只在其宿主渲染）。
// - 工具名单以 `listTools`（`GET /tools`）同源解析；取数失败时组件内按 id 原文兜底。
// - 本路由在稳定侧栏（NAV）中有一项；命令面板与深链同样可达。

import Box from '@mui/material/Box';
import { useTranslation } from 'react-i18next';
import { listTools } from '../api/endpoints';
import { MachineDiagnostics } from '../components/MachineDiagnostics';
import { PageHeader } from '../components/PageHeader';
import { useResource } from '../lib/useResource';
import { spacing } from '../theme';

export function DiagnosticsPage() {
  const { t } = useTranslation();
  // 工具名单：同一 GET /tools（`MachineDiagnostics` 的 tools prop 语义不变）。
  const tools = useResource((signal) => listTools({ signal }), { deps: [] });

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: spacing.x4 }}>
      <PageHeader
        title={t('title.diag')}
        breadcrumb={[{ label: t('title.usage'), to: '/usage' }, { label: t('title.diag') }]}
      />
      <MachineDiagnostics tools={tools.data ?? []} />
    </Box>
  );
}

export default DiagnosticsPage;
