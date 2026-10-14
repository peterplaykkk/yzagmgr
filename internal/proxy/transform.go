package proxy

// 本文件原承载 C 路的「四类改写」（模型名映射、effort 对齐、思考对齐、命名适配）。
// 九前缀 C 路随旧网关接入面整删后（design.md §1.1/§2.4），四类改写与 ModelRule/
// ModelMapping 一并删除；共享原语（Wire/WireFor 判定已亡、TransformResult、[1m] 剥离、
// stream 事实、JSON 字面量编码）迁往 wire.go。
//
// 留在本文件的是**保留面共用**的 effort 阶梯词表：B 路（sdk 引擎同步的 remap/explain
// 写链与解释面）按它校验档位白名单（remap.go normalizeEffortTier、explain.go 草稿校验）。

// upstreamEffortLadder 是上游网关逐字枚举出的思考档阶梯（顶档 max）。
//
// 依据：网关在 `/openai/v1/responses` 上枚举 `supported_reasoning_levels` =
// `none,minimal,low,medium,high,xhigh,max`（acceptance-recipe §A 行 5/9；
// codex 目录即照此声明），`ultra`/`persistent` 实测 422。
var upstreamEffortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// safeEffortValue 截断超长值，避免把客户端的任意大字符串回显进错误信息。
func safeEffortValue(v string) string {
	const max = 64
	if len(v) > max {
		return v[:max] + "…"
	}
	return v
}
