// 入参解析的共用层：查询串与请求体的统一口径。
//
// 纪律：
//   - 时间参数同时接受 **unix 秒**（IA §3 的 URL 契约）与 RFC3339（脚本手写友好），
//     但只有这一处解释，避免每个 handler 各写一遍；
//   - 分页参数复用 middleware.go 的 PageParams（limit ∈ 1..200，默认 50）；
//   - 请求体一律走 DecodeBody（拒绝未知字段），不在这里放宽。
package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// normalizePageLimit 把已解析的 limit 归一：0 表示默认值（§1.5 的 50）。
func normalizePageLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return 50, nil
	case limit < 1 || limit > 200:
		return 0, apierr.New(apierr.CodeInvalidRequest, "limit 必须在 1..200 之间: %d", limit)
	}
	return limit, nil
}

// parseTimeParam 解析时间参数。
//
// 契约只写了 `from,to` 没规定编码，IA §3 用 unix 秒；这里两种都收下并在此处收口，
// 避免「前端传秒、脚本传 RFC3339」被拒。空串返回零值（表示不限）。
func parseTimeParam(raw, field string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, nil
	}
	// 纯数字（可带负号）按 unix 秒解释；毫秒级时间戳（13 位）也一并容忍。
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 { // 毫秒
			return time.UnixMilli(n), nil
		}
		return time.Unix(n, 0), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, apierr.New(apierr.CodeInvalidRequest,
		"%s 必须是 unix 秒或 RFC3339 时间: %q", field, raw)
}

// parseOptionalInt 解析可选的整数查询参数；空串返回 0 且 ok=false。
func parseOptionalInt(raw, field string) (int, bool, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false, apierr.New(apierr.CodeInvalidRequest, "%s 必须是整数: %q", field, raw)
	}
	return n, true, nil
}

// requireTimeWindow 取 from/to 并校验 from <= to（闭开区间 [from, to)）。
func requireTimeWindow(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	from, err := parseTimeParam(q.Get("from"), "from")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parseTimeParam(q.Get("to"), "to")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if from.IsZero() || to.IsZero() {
		return time.Time{}, time.Time{}, apierr.New(apierr.CodeInvalidRequest, "必须同时给出 from 与 to（闭开区间 [from, to)）")
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, apierr.New(apierr.CodeInvalidRequest, "时间窗终点早于起点")
	}
	return from, to, nil
}

// decode 是 DecodeBody 的薄包装，把「缺请求体」也归到 invalid_request。
func decode(r *http.Request, dst any) error {
	return DecodeBody(r, dst)
}

// decodeOptional 允许空请求体（POST 无 body 的端点，如 probe/start/stop）。
// 有 body 时仍严格解析（拒绝未知字段）。
func decodeOptional(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return DecodeBody(r, dst)
}

// requireEnum 校验值属于封闭集合，否则 invalid_request（契约里的 mode/op/bucket 等）。
func requireEnum(field, value string, allowed ...string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return apierr.New(apierr.CodeInvalidRequest,
		"%s 取值非法: %q（允许 %s）", field, value, strings.Join(allowed, "|"))
}
