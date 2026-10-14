// 多路由清单（UC-030 的多条目形态）在写链输入侧的载体与渲染。
//
// 承载形态（2026-09-24 UC-030 实现裁决）：`family.GatewayInput.Extra[ModelEntriesExtraKey]`
// 携带一张多路由清单（JSON 数组），逐条目 name/labelOverride/supports1m/prefer1m/maxEffort；
// 形态自定但与 UC-030 验收行的字节断言吻合——ProfileBytes 按它渲染 `inferenceModels`，
// 档案字节可被 scripts/verify/claude-desktop-five-route.sh 逐条目断言。
//
// 语义锚（已实证，照抄不重推）：reports/mixed-five-route-20260923/profile-candidate：
//
//   - 条目集**恰**等于清单集：无兜底条目、无多余条目（UC-030 A1/A3）；
//   - `name` 恒为裸名（桌面 schema 拒 `[1m]` 后缀，1M 由能力位表达，R108 实证）；
//   - `supports1m`/`prefer1m` false/缺席**不伪造键**（z 的缺席=false 语义）；
//   - `maxEffort` 逐条目承载（缺省 = MaxEffort；desktop-effort-oracle 阶梯不被切断）；
//   - 渲染顺序按 name 排序（v1 档案的规范形态，保证「同输入恒同字节」，UC-003 A7）。
//
// 单条目旧形态向后兼容：Extra 里**没有**本键时 ProfileBytes 保持既有单条目行为
// （name = 固定档名、labelOverride = 上游名、能力位跟随 ContextWindow）。
package claudedesktop

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ModelEntriesExtraKey 是多路由清单在 `GatewayInput.Extra` 里的键。
//
// 键名随家族既有键位族（claudeDesktopMode / claudeDesktopProxyKey）扩展；
// 值是 `EncodeModelEntries` 的输出（JSON 数组字符串）。空串 = 不承载（单条目旧形态）。
const ModelEntriesExtraKey = "claudeDesktopModelEntries"

// ModelEntry 是多路由清单的一条（UC-030 的逐条目三元 + 承载名）。
//
// JSON 键名与 app.asar 接受集的条目键同名（name/labelOverride/supports1m/prefer1m/maxEffort），
// 载体与档案字节同构，避免两套键位各自漂移。
type ModelEntry struct {
	Name          string `json:"name"`
	LabelOverride string `json:"labelOverride"`
	Supports1m    bool   `json:"supports1m,omitempty"`
	Prefer1m      bool   `json:"prefer1m,omitempty"`
	MaxEffort     string `json:"maxEffort,omitempty"`
}

// desktopEffortLadder 是桌面 `maxEffort` 的合法档位（desktop-effort-oracle 的五名）。
var desktopEffortLadder = []string{"low", "medium", "high", "xhigh", "max"}

// EncodeModelEntries 把多路由清单编码为 Extra 载体值（确定性字节）。
//
// 编码前做与解析侧**同一套**校验并按 name 排序：装配层写进来的清单不合格时
// 在入口就拒绝，而不是等 ProfileBytes 再报一遍。
func EncodeModelEntries(entries []ModelEntry) (string, error) {
	cleaned, err := validateModelEntries(entries)
	if err != nil {
		return "", err
	}
	blob, err := json.Marshal(cleaned)
	if err != nil {
		return "", apierr.Wrap(apierr.CodeInternal, err, "编码多路由清单失败")
	}
	return string(blob), nil
}

// ParseModelEntries 解析并校验 Extra 载体值（ProfileBytes 的多条目入口）。
//
// 形态硬约束：必须是 JSON 数组（`null` / 标量 / 对象都拒）——清单是显式多条目意图，
// 形态不对时业务拒绝，**不**回落单条目（否则残缺意图被静默吞掉）。
func ParseModelEntries(raw string) ([]ModelEntry, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "[") {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"多路由清单必须是 JSON 数组（%s），实际形态不符", ModelEntriesExtraKey)
	}
	var entries []ModelEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid, "多路由清单不是合法 JSON 数组: %v", err)
	}
	return validateModelEntries(entries)
}

// validateModelEntries 校验清单并返回按 name 排序的副本（含 maxEffort 缺省归一）。
func validateModelEntries(entries []ModelEntry) ([]ModelEntry, error) {
	if len(entries) == 0 {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"多路由清单为空数组：多条目形态至少一条，单条目旧形态应省略 %s 键", ModelEntriesExtraKey)
	}
	out := make([]ModelEntry, len(entries))
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		name := strings.TrimSpace(e.Name)
		label := strings.TrimSpace(e.LabelOverride)
		if name == "" {
			return nil, apierr.New(apierr.CodeConfigInvalid, "多路由清单第 %d 条缺 name（请求侧选请求映射名）", i+1)
		}
		if strings.Contains(name, "[1m]") {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"多路由清单第 %d 条 name=%q 带 [1m] 后缀：桌面 schema 拒收该后缀，1M 由能力位表达（条目名恒为裸名）", i+1, name)
		}
		if label == "" {
			return nil, apierr.New(apierr.CodeConfigInvalid, "多路由清单第 %d 条（%s）缺 labelOverride（上游模型名）", i+1, name)
		}
		if seen[name] {
			return nil, apierr.New(apierr.CodeConfigInvalid, "多路由清单第 %d 条 name=%q 重复：条目集必须互异", i+1, name)
		}
		seen[name] = true
		effort := strings.ToLower(strings.TrimSpace(e.MaxEffort))
		if effort == "" {
			effort = MaxEffort
		}
		if !inDesktopEffortLadder(effort) {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"多路由清单第 %d 条（%s）maxEffort=%q 不是桌面档位（%s）", i+1, name, e.MaxEffort, strings.Join(desktopEffortLadder, "|"))
		}
		out[i] = ModelEntry{
			Name:          name,
			LabelOverride: label,
			Supports1m:    e.Supports1m,
			Prefer1m:      e.Prefer1m,
			MaxEffort:     effort,
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func inDesktopEffortLadder(v string) bool {
	for _, l := range desktopEffortLadder {
		if v == l {
			return true
		}
	}
	return false
}

// renderModelEntry 渲染一条 `inferenceModels` 条目（单/多条目共用）。
//
// 能力位 false/缺席**不伪造键**（§A 行 2 / R108：z 的 supports1m 缺席即 false 语义）。
func renderModelEntry(name, label, maxEffort string, supports1m, prefer1m bool) map[string]any {
	entry := map[string]any{
		entryKeyName:          name,
		entryKeyLabelOverride: label,
		entryKeyFamilyTier:    FamilyTier,
	}
	if maxEffort != "" {
		entry[entryKeyMaxEffort] = maxEffort
	}
	if supports1m {
		entry[entryKeySupports1m] = true
	}
	if prefer1m {
		entry[entryKeyPrefer1m] = true
	}
	return entry
}

// modelEntriesFromExtra 从 `GatewayInput.Extra` 取出多路由清单；未承载时返回 nil
// （单条目旧形态）。解析或校验失败一律业务拒绝（不回落单条目）。
func modelEntriesFromExtra(extra map[string]string) ([]ModelEntry, error) {
	raw := strings.TrimSpace(extra[ModelEntriesExtraKey])
	if raw == "" {
		return nil, nil
	}
	return ParseModelEntries(raw)
}
