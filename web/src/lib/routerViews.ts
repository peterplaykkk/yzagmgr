// 请求映射页的三段子区（UC-036）：模型映射 / 思考深度映射 / 自定义字段映射。
//
// 契约：
// - `?view=` 是「同一页面的变体」（dev-web URL 四段式：身份进 path、变体进 query）；
// - 默认子区 `model` 按仓库约定从规范 URL 省略（`/arms`），非法值载入即 replace 归一；
// - 侧栏二级目录（`/arms?view=…`）与页面 Tabs 共用这一份取值域，不各自硬编码。

import { readOption } from './url';

export const ROUTER_VIEWS = ['model', 'effort', 'fields'] as const;
export type RouterView = (typeof ROUTER_VIEWS)[number];
export const DEFAULT_ROUTER_VIEW: RouterView = 'model';

/** 读 `?view=`：缺省与非法值都按默认解读（`valid=false` 时由页面把 URL 归一化回默认）。 */
export function readRouterView(raw: string | null): { value: RouterView; valid: boolean } {
  return readOption<RouterView>(raw, ROUTER_VIEWS, DEFAULT_ROUTER_VIEW);
}
