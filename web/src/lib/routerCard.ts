// 「请求映射」卡（kind=router）的**消费形态**判据（UC-043 / UC-046）：这张卡吃不吃**单一模型**。
//
// 两种形态（同一个 kind，两种写进工具配置的东西）：
//
// - `single-model`（四 CLI 家族 claude / codex / opencode / pi）：卡写进工具配置的是**一个**
//   模型选择器名（已登记的映射名或别名名）⇒ 切卡时必须有「使用映射」选择器（UC-043）。
// - `alias-set`（claude-desktop，UC-046）：卡写进配置的是**「请求映射」页登记的别名集**
//   （每条别名一个条目，随登记变化、无兜底、无多余）⇒ **没有单一模型可选**：不渲染选择器，
//   预览也不带 `modelId`（计划期守卫按「别名集非空」判定；集合为空由服务端拒绝并指向登记，
//   错误面由既有对话框照常呈现，前端不另造 UI）。
//
// 判据为什么落在**工具身份**而不是卡载荷：UC-046 冻结契约写明该卡「与四 CLI 家族同构」——
// id/kind/名称/可编辑性逐字相同，卡载荷里没有任何字段能区分两种形态；「是否吃单模型」是
// **家族事实**（该家族实现的 RouterForm 渲染的是什么），因此在这里显式登记一处，
// 不让对话框/测试各自猜（判据只此一份，消费者 import 本模块）。
//
// 登记取向（两种误判的代价不对称，所以只登记例外）：
// - 漏登记（别名集卡被当单模型卡）⇒ 多出一个无意义的选择器，且预览多带一个服务端不消费的
//   `modelId`——轻；
// - 多登记（单模型卡被当别名集卡）⇒ UC-043 的选择能力整体失效——重。
// 故未登记的工具一律按单模型卡处理；新家族实现 RouterForm 且形态是别名集时，必须加进下表。

/** 「请求映射」卡的消费形态：写一个模型选择器名（单模型）或写一整组别名（别名集）。 */
export type RouterCardForm = 'single-model' | 'alias-set';

/** 别名集形态的家族（UC-046；当前只有 claude-desktop）。 */
const ALIAS_SET_ROUTER_TOOLS: readonly string[] = ['claude-desktop'];

/** 该工具的「请求映射」卡是否按**别名集**消费映射（UC-046；其余家族按单一模型，UC-043）。 */
export function routerCardForm(toolId: string): RouterCardForm {
  return ALIAS_SET_ROUTER_TOOLS.includes(toolId) ? 'alias-set' : 'single-model';
}
