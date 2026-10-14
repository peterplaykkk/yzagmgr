// claude-desktop 多路由清单的派生（UC-030：本地 router 多臂模型 → 写链多条目）。
//
// 冻结契约（scripts/verify/claude-desktop-five-route.sh 头注释「多条目派生」段）：
// 本地 router 多臂模型**在册**（每臂：路由 + 别名 remap + effort 归属齐备）时，
// claude-desktop 受管卡的网关档目标档案**每条 remap 别名恰一条目**：
//
//	name            = 别名（请求侧选臂名，裸名无 [1m] 后缀）
//	labelOverride   = 目标模型（v2 ProfileBytes 既有约定：labelOverride 承载上游模型名）
//	supports1m/prefer1m = remap model set 注册的能力位（false/缺席不伪造键）
//	maxEffort       = claudedesktop.MaxEffort（desktop-effort-oracle 阶梯不被条目上限切断）
//
// 条目集恰等于别名集（无兜底条目、无多余条目）。路由模型**为空** ⇒ 不承载清单
// （保持单条目旧形态）；**不完整** ⇒ 业务拒绝（不产出残缺多条目，UC-030 边界
// 「不得现场改字节」口径：宁可响亮拒绝，不静默猜一种形态）。
// 统计口径是**用户臂**：下发臂（UC-034 遗留#1，归属记账命中）不参与派生（见
// desktopRouteEntriesRaw 的注释），因此「只有下发臂」等价于「路由模型为空」。
//
// 衔接语义（UC-023/028 的选臂面）：条目 `name` = `remap model set` 的 alias，
// 数据面 `model=<name>` 经 resolveRouterArm 命中该条目所属臂并一次改写为
// `labelOverride`（target）——档案条目名与 router 臂命名由此可衔接。
package api

import (
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
)

// desktopRouteEntriesRaw 从本地 router 多臂模型派生多路由清单载体（Extra 值）。
//
// 返回 ("", nil) = 路由模型为空（调用方保持单条目旧形态）；
// 返回非 nil error = 模型在册但不完整/清单非法（业务拒绝）。
//
// （下发臂排除分支已随登录下发删除——design §2.2 desktoproutes 行：归属记账
// deliveredArms 不复存在，全部在册臂都按用户臂派生。）
func (s *Server) desktopRouteEntriesRaw() (string, error) {
	arms := s.Proxy.Arms()
	var incomplete []string
	entries := make([]claudedesktop.ModelEntry, 0, len(arms))
	userArms := 0
	for _, arm := range arms {
		userArms++
		maps := s.Proxy.ModelMapFor(arm.Name)
		switch {
		case len(maps) == 0:
			incomplete = append(incomplete, arm.Name+"（缺别名 remap）")
			continue
		case !s.Proxy.EffortClampEngaged(arm.Name):
			incomplete = append(incomplete, arm.Name+"（缺 effort 归属）")
			continue
		}
		for _, m := range maps {
			c := s.modelCapsFor(arm.Name, m.Alias)
			entries = append(entries, claudedesktop.ModelEntry{
				Name:          m.Alias,
				LabelOverride: m.Target,
				Supports1m:    c.Supports1m,
				Prefer1m:      c.Prefer1m,
				MaxEffort:     claudedesktop.MaxEffort,
			})
		}
	}
	if userArms == 0 {
		// 没有用户臂（含「只有下发臂」）：不承载清单，保持单条目旧形态。
		return "", nil
	}
	if len(incomplete) > 0 {
		return "", apierr.New(apierr.CodeConfigInvalid,
			"请求映射模型不完整（%s）：拒绝派生残缺多条目（每条请求映射须「路由 + 别名 remap + effort 归属」齐备；"+
				"补齐或移除后再生成 claude-desktop 网关档，UC-030）", strings.Join(incomplete, "；"))
	}
	// EncodeModelEntries 校验并按 name 排序（裸名/互异/maxEffort 档位），产出确定性载体。
	return claudedesktop.EncodeModelEntries(entries)
}

// desktopRouteEntries 是 desktopRouteEntriesRaw 的清单形态（测试与派生断言用）。
func (s *Server) desktopRouteEntries() ([]claudedesktop.ModelEntry, error) {
	raw, err := s.desktopRouteEntriesRaw()
	if err != nil || raw == "" {
		return nil, err
	}
	return claudedesktop.ParseModelEntries(raw)
}

// attachDesktopModelEntries 把多路由清单写进网关档输入（claude-desktop 专用）。
//
// 路由模型为空时**不写键**：ProfileBytes 保持单条目旧形态（既有调用方不受影响）。
func (s *Server) attachDesktopModelEntries(in *family.GatewayInput) error {
	raw, err := s.desktopRouteEntriesRaw()
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	if in.Extra == nil {
		in.Extra = map[string]string{}
	}
	in.Extra[claudedesktop.ModelEntriesExtraKey] = raw
	return nil
}

// planDesktopRouter 走 claude-desktop 的**请求映射形态**（UC-046）。
//
// 与另外四族 router 卡的差别（冻结契约，逐条对齐 UC-046）：
//
//   - 可路由性判据 = **别名集非空**（不是单一模型名）：别名集为空 ⇒ 拒绝并指向「请求映射」
//     页登记。空档案在 Desktop 里没有任何可选档位——写出去等于把用户静默推进死路，故
//     fail-closed（UC-044 对四 CLI 家族的「空表透传」放宽不适用：那些族的配置里没有
//     「条目集」这个槽位，本族的条目集**就是**它的配置本体）。
//   - 条目集复用 UC-030 的**同一条**派生（[Server.attachDesktopModelEntries]：name = 别名、
//     labelOverride = 目标、能力位随登记、maxEffort = 家族常量）：同一份登记在两种形态下
//     写出同一批条目，绝不长出第二份派生器（两处各写一份必然漂移）。派生器的完整性拒绝
//     （某条请求映射缺别名/缺 effort 归属）在这里同样成立——它指明的正是缺什么。
//   - UC-043 的 `modelId` 覆盖**不适用**（该形态没有单一模型可选）：显式给出即拒绝。
//     不静默忽略的理由：界面上那个「使用映射」选择器会把「选了一个名字」显示成生效，
//     而这张卡根本不消费它——宁可响亮报错，也不让选择器假装有用。
func (s *Server) planDesktopRouter(
	in family.GatewayInput, rf family.RouterForm, modelOverride string,
) ([]family.FilePlan, error) {
	if modelOverride != "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"claude-desktop 的请求映射形态没有单一模型可选（条目=别名集，由「请求映射」页管理）："+
				"去掉 modelId 即按整份别名集生成，或在「请求映射」页增删别名")
	}
	if err := s.attachDesktopModelEntries(&in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Extra[claudedesktop.ModelEntriesExtraKey]) == "" {
		return nil, routerPlanNoAliasError()
	}
	return rf.PlanRouter(s.Env, in)
}

// routerPlanNoAliasError 是「别名集为空」的计划期拒绝（UC-046 A5）。
//
// 文案同 UC-043 的纠错风格：说明判据 + 指出**出路**（去「请求映射」页登记），
// 而不是只报一句「不能启用」。
func routerPlanNoAliasError() error {
	return apierr.New(apierr.CodeInvalidRequest,
		"别名集为空（该卡按别名集生成档位，没有兜底条目也没有单条目回退）："+
			"先在「请求映射」页登记请求映射与别名，再启用该卡")
}
