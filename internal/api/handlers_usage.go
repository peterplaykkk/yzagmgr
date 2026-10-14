// §8 用量观测面：summary / trends / breakdown / requests / pricing。
//
// 本文件不重算任何金额或 token：全部来自 `internal/usage` 的聚合查询。金额未知时的
// `cost:null + costUnknown:true` 由服务包给出，这里只做透传（UC-008 A7）。
package api

import (
	"net/http"
	"net/url"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// windowFrom 解析时间窗。两个端点族的宽严不同：
// summary/trends/breakdown 必须给窗（服务包 requireWindow 会把空窗挡掉），
// requests 允许完全不给窗（全量），但**只给一半**是错的——那种调用几乎都是漏参数。
func windowFrom(r *http.Request) (usage.Window, error) {
	q := r.URL.Query()
	from, err := parseTimeParam(q.Get("from"), "from")
	if err != nil {
		return usage.Window{}, err
	}
	to, err := parseTimeParam(q.Get("to"), "to")
	if err != nil {
		return usage.Window{}, err
	}
	if from.IsZero() != to.IsZero() {
		return usage.Window{}, apierr.New(apierr.CodeInvalidRequest,
			"时间窗必须同时给出 from 与 to（闭开区间 [from, to)）")
	}
	return usage.Window{From: from, To: to}, nil
}

// requireUsageStore 取用量库；不可用时用 storage 明确报出（界面据此提示「用量历史不可用」）。
func (s *Server) requireUsageStore() (*usage.Store, error) {
	if s.Usage == nil {
		return nil, apierr.New(apierr.CodeStorage,
			"用量库不可用：启动时打开失败，见进程日志")
	}
	return s.Usage, nil
}

func (s *Server) handleUsageSummary(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	from, to, err := requireTimeWindow(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	summary, err := store.Summary(r.Context(), usage.SummaryQuery{
		Window:   usage.Window{From: from, To: to},
		App:      q.Get("app"),
		Provider: q.Get("provider"),
	})
	if err != nil {
		return nil, err
	}
	return summary, nil
}

// modelMultiFrom 取出 `model` 多值过滤参数：允许重复 key（model=a&model=b），
// 也兼容逗号分隔的单参数（model=a,b）；两种写法可混用。参数缺席返回 nil（不过滤）。
//
// 这里只做「取出」，逗号拆分/trim/丢空段/形状校验统一在 usage.normalizeModels——
// 语义只有一处实现，HTTP 面与直接调用服务包的调用方得到同一条判定。
func modelMultiFrom(q url.Values) []string { return q["model"] }

// parseBoolFlag 解析 0/1 布尔开关参数：""/"0" = 关（缺省 = 加参数之前逐字节不变）、
// "1" = 开；其它取值按形状防线报 invalid_request，不静默猜测（与 sparse/order/bucket
// 同一条「非法值不猜」纪律）。sparse 与 upstream_only 共用这条判定。
func parseBoolFlag(value, name string) (bool, error) {
	switch value {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, apierr.New(apierr.CodeInvalidRequest, "%s 只能是 0 或 1: %q", name, value)
	}
}

func (s *Server) handleUsageTrends(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	from, to, err := requireTimeWindow(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	bucket := usage.Bucket(q.Get("bucket"))
	if bucket == "" {
		bucket = usage.BucketHour
	}
	by := usage.Dimension(q.Get("by"))
	if by == "" {
		by = usage.ByAll
	}
	// sparse（R222a，opt-in 零段压缩）与 upstream_only（2026-09-29 additive，唯一化
	// 展示）同一条 0/1 开关纪律（parseBoolFlag）：只有 "1" 开启；""/"0" 关闭（缺省 =
	// 加参数之前逐字节不变）；非法值 invalid_request 不静默猜。
	// upstream_only 为真时排除未达上游的行：model_upstream 为 NULL/空串的行，与
	// 自回显拒绝行（upstream == requested 且 status ≥ 400；0.6.2+ 记录路径把这批
	// 未达上游即被拒的请求的 upstream 写成 requested 自身）。真映射失败与直通成功照常进聚合。
	sparse, err := parseBoolFlag(q.Get("sparse"), "sparse")
	if err != nil {
		return nil, err
	}
	upstreamOnly, err := parseBoolFlag(q.Get("upstream_only"), "upstream_only")
	if err != nil {
		return nil, err
	}
	trends, err := store.Trends(r.Context(), usage.TrendsQuery{
		Window:       usage.Window{From: from, To: to},
		Bucket:       bucket,
		By:           by,
		App:          q.Get("app"),
		Provider:     q.Get("provider"),
		Models:       modelMultiFrom(q),
		Sparse:       sparse,
		UpstreamOnly: upstreamOnly,
	})
	if err != nil {
		return nil, err
	}
	return trends, nil
}

func (s *Server) handleUsageBreakdown(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	from, to, err := requireTimeWindow(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	by := usage.Dimension(q.Get("by"))
	if by == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest, "by 必填（app|provider|model）")
	}
	// app/provider 是可选的收窄过滤（UC-039 请求详情页按工具过滤），与 summary 同语义；
	// 缺省（两个参数都缺席）时行为与加过滤之前逐字相同。
	// model 多值过滤（2026-09-28 additive）与 trends 同参同语义（两面同参）。
	// upstream_only（2026-09-29 additive，唯一化展示）与 trends 同参同语义（两面同参）。
	upstreamOnly, err := parseBoolFlag(q.Get("upstream_only"), "upstream_only")
	if err != nil {
		return nil, err
	}
	breakdown, err := store.Breakdown(r.Context(), usage.BreakdownQuery{
		Window:       usage.Window{From: from, To: to},
		By:           by,
		App:          q.Get("app"),
		Provider:     q.Get("provider"),
		Models:       modelMultiFrom(q),
		UpstreamOnly: upstreamOnly,
	})
	if err != nil {
		return nil, err
	}
	return breakdown, nil
}

// requestLogPage 与 Page 同形，只是 total 用 int64（请求数可能超过 32 位计数）。
type requestLogPage struct {
	Items      []usage.RequestLog `json:"items"`
	NextCursor string             `json:"nextCursor"`
	Total      int64              `json:"total"`
}

func (s *Server) handleUsageRequests(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	window, err := windowFrom(r)
	if err != nil {
		return nil, err
	}
	limit, cursor, err := PageParams(r)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	order := usage.Order(q.Get("order"))
	if order != "" && !order.Valid() {
		return nil, apierr.New(apierr.CodeInvalidRequest, "order 只能是 asc 或 desc: %q", q.Get("order"))
	}
	status, ok, err := parseOptionalInt(q.Get("status"), "status")
	if err != nil {
		return nil, err
	}
	upstreamOnly, err := parseBoolFlag(q.Get("upstream_only"), "upstream_only")
	if err != nil {
		return nil, err
	}
	var statusPtr *int
	if ok {
		statusPtr = &status
	}
	page, err := store.Requests(r.Context(), usage.RequestsQuery{
		Window:       window,
		App:          q.Get("app"),
		Provider:     q.Get("provider"),
		Models:       modelMultiFrom(q),
		UpstreamOnly: upstreamOnly,
		Status:       statusPtr,
		Order:        order,
		Limit:        limit,
		Cursor:       cursor,
	})
	if err != nil {
		return nil, err
	}
	items := page.Items
	if items == nil {
		items = []usage.RequestLog{}
	}
	return requestLogPage{Items: items, NextCursor: page.NextCursor, Total: page.Total}, nil
}

func (s *Server) handleUsageRequestGet(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	detail, err := store.RequestDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return detail, nil
}

func (s *Server) handleUsagePricing(r *http.Request) (any, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, err
	}
	table, err := store.Pricing(r.Context())
	if err != nil {
		return nil, err
	}
	if table.Entries == nil {
		table.Entries = []usage.PricingEntry{}
	}
	return table, nil
}

// handleUsagePricingSave 是整表替换（契约 §8：`{entries:[...]}` 回读一致）。
func (s *Server) handleUsagePricingSave(r *http.Request) (any, string, error) {
	store, err := s.requireUsageStore()
	if err != nil {
		return nil, "", err
	}
	var body usage.PricingTable
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	table, err := store.ReplacePricing(r.Context(), body)
	if err != nil {
		return nil, "", err
	}
	if table.Entries == nil {
		table.Entries = []usage.PricingEntry{}
	}
	return table, "", nil
}
