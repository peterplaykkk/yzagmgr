// §7.1 UC-096 additive：请求映射解释预览（纯计算）。
//
// 端点纪律：这是**只读**面——不写任何字节、不建租约、不落操作日志（可反复试输入、调用
// 多少次都不改变任何状态）；POST 只因输入是结构体（草稿规则集可能很大，不适合塞 query）。
// 语义（匹配/effort/冲突/规则版本）全部在 internal/proxy：本文件只做「解析入参 →
// 一次调用 → 包信封」。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// routeExplainDraftWire 是草稿规则集的线上形状（段级整体替换）。
//
// 三个字段都用**指针**：用来区分「段缺席」（nil ⇒ 沿用生效表）与「段在场但为空」
// （`[]`/`{}` ⇒ 该段整体替换为空）——两者语义不同，不能靠零值混判。
type routeExplainDraftWire struct {
	Routes      *[]proxy.ArmEntry                 `json:"routes"`
	ModelRemap  *map[string][]proxy.ModelMapEntry `json:"modelRemap"`
	EffortRemap *map[string]proxy.EffortTable     `json:"effortRemap"`
}

// routeExplainBody 是 POST /routes/explain 的请求体（未列字段一律拒绝，§1.5 口径）。
type routeExplainBody struct {
	// Model 必填：请求侧模型名（逐字比较）。
	Model string `json:"model"`
	// Effort 可选：请求侧思考档位；缺席 = 请求体缺 output_config.effort（与数据面同义）。
	Effort *string `json:"effort"`
	// Draft 可选：候选（草稿）规则集；nil = 对生效表解释。
	Draft *routeExplainDraftWire `json:"draft"`
}

// handleRoutesExplain 处理 POST /routes/explain（UC-096 A1–A6 的纯计算入口）。
func (s *Server) handleRoutesExplain(r *http.Request) (any, error) {
	var body routeExplainBody
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	in := proxy.ExplainInput{Model: body.Model, Effort: body.Effort}
	if body.Draft != nil {
		d := &proxy.ExplainDraft{}
		if body.Draft.Routes != nil {
			d.HasRoutes, d.Routes = true, *body.Draft.Routes
		}
		if body.Draft.ModelRemap != nil {
			d.HasModelRemap, d.ModelRemap = true, *body.Draft.ModelRemap
		}
		if body.Draft.EffortRemap != nil {
			d.HasEffortRemap, d.EffortRemap = true, *body.Draft.EffortRemap
		}
		in.Draft = d
	}
	return s.Proxy.ExplainRoute(in)
}
