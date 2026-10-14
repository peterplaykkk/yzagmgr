// §2 健康与版本 + §12 状态栏 + §1.4 操作日志：进程状态的三个只读视图。
package api

import (
	"net/http"
	"runtime"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/operations"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// ── §2 ──────────────────────────────────────────────────────────────────────

// usageDBStatus 是 /health 的 usageDb 形状（§2）。
type usageDBStatus struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// healthPayload 是 GET /health 的响应（§2）。
//
// `state:"updating"` 表示自更新正在替换进程：此时 /health 可能短暂不可达，
// 界面必须把不可达解释为「可能正在更新」，恢复后刷新页面（令牌随进程轮换）。
type healthPayload struct {
	Status       string        `json:"status"`
	State        string        `json:"state"`
	UptimeSec    int64         `json:"uptimeSec"`
	DataRoot     string        `json:"dataRoot"`
	APIPort      int           `json:"apiPort"`
	ProxyPort    int           `json:"proxyPort"`
	ProxyRunning bool          `json:"proxyRunning"`
	UsageDB      usageDBStatus `json:"usageDb"`
}

func (s *Server) handleHealth(r *http.Request) (any, error) {
	return healthPayload{
		Status:       "ok",
		State:        s.processState(),
		UptimeSec:    int64(time.Since(s.started).Seconds()),
		DataRoot:     s.Env.DataRoot(),
		APIPort:      s.APIPort,
		ProxyPort:    s.ProxyPort,
		ProxyRunning: s.Proxy.Running(),
		UsageDB:      s.usageDBStatus(),
	}, nil
}

// processState 报告进程状态。
// （「自更新正在替换进程」的 updating 态已随下载/自更新面删除——design §2.2 §11；恒 ok。）
func (s *Server) processState() string { return "ok" }

// usageDBStatus 报告用量库健康。`state != "ok"` 时界面必须明示「用量历史不可用/正在重建」，
// 不得静默显示 0（UC-008 A11）。
func (s *Server) usageDBStatus() usageDBStatus {
	if s.Usage == nil {
		return usageDBStatus{State: string(usage.StateCorrupt), Detail: "用量库未打开（启动时失败，见进程日志）"}
	}
	h := s.Usage.Health()
	return usageDBStatus{State: string(h.State), Detail: h.Detail}
}

// versionPayload 是 GET /version 的响应（§2）。
type versionPayload struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	BuildTime  string `json:"buildTime"`
	GoVersion  string `json:"goVersion"`
	APIVersion string `json:"apiVersion"`
}

func (s *Server) handleVersion(*http.Request) (any, error) {
	return versionPayload{
		Version:    s.Version.Version,
		Commit:     s.Version.Commit,
		BuildTime:  s.Version.BuildTime,
		GoVersion:  runtime.Version(),
		APIVersion: "v1",
	}, nil
}

// ── §1.4 操作日志 ───────────────────────────────────────────────────────────

// operationsQuery 是 GET /operations 的过滤条件（§1.4）。
type operationsQuery struct {
	Action    string
	Target    string
	Result    string
	From      time.Time
	To        time.Time
	Limit     int
	Cursor    string
	RequestID string
}

func (s *Server) handleOperationsList(r *http.Request) (any, error) {
	q := r.URL.Query()
	filter, err := parseOperationsQuery(r)
	if err != nil {
		return nil, err
	}
	// requestId 过滤是「拿到失败响应后定位那一条」的唯一手段（UC-014 A12）。
	// operations 包没有这个过滤维度，因此这里做一次**有界扫描**：一个请求的变更
	// 记录总在日志头部附近，但游标语义会把它截断，所以按页回退扫描到上限为止。
	if rid := q.Get("requestId"); rid != "" {
		page, err := s.operationsByRequestID(filter, rid)
		if err != nil {
			return nil, err
		}
		return NewPage(page.Items, page.NextCursor, page.Total), nil
	}

	page, err := s.Ops.Query(filter)
	if err != nil {
		return nil, err
	}
	// Skipped 是解析不动的审计行数：契约里没有它的位置，但静默吞掉不可接受。
	if page.Skipped > 0 {
		s.log.Warn("操作日志存在无法解析的行", "skipped", page.Skipped)
	}
	return NewPage(page.Items, page.NextCursor, page.Total), nil
}

// maxRequestIDScan 是 requestId 定位的扫描上限（条）。超过它说明调用方给的不是
// 「刚刚那次失败」的 requestId，继续翻页没有意义。
const maxRequestIDScan = 1000

// operationsByRequestID 扫描日志找同 requestId 的记录，并按请求的 limit 分页。
func (s *Server) operationsByRequestID(filter operations.Filter, requestID string) (operations.Page, error) {
	limit, err := normalizePageLimit(filter.Limit)
	if err != nil {
		return operations.Page{}, err
	}
	scan := filter
	scan.Limit = operations.MaxLimit
	scan.Cursor = ""

	matched := make([]operations.Operation, 0, limit)
	seen := 0
	for seen < maxRequestIDScan {
		page, err := s.Ops.Query(scan)
		if err != nil {
			return operations.Page{}, err
		}
		if page.Skipped > 0 {
			s.log.Warn("操作日志存在无法解析的行", "skipped", page.Skipped)
		}
		for _, op := range page.Items {
			if op.RequestID == requestID {
				matched = append(matched, op)
			}
		}
		seen += len(page.Items)
		if page.NextCursor == "" || len(matched) >= limit {
			break
		}
		scan.Cursor = page.NextCursor
	}

	total := len(matched)
	next := ""
	if len(matched) > limit {
		next = EncodeCursor(requestID, matched[limit-1].ID)
		matched = matched[:limit]
	}
	return operations.Page{Items: matched, NextCursor: next, Total: total}, nil
}

func (s *Server) handleOperationGet(r *http.Request) (any, error) {
	op, err := s.Ops.Get(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return op, nil
}

// parseOperationsQuery 解析查询串。时间参数同时接受 unix 秒与 RFC3339
// （IA §3 用 unix 秒；契约只写 from/to 未规定编码，两种都收下不算发明口径）。
func parseOperationsQuery(r *http.Request) (operations.Filter, error) {
	q := r.URL.Query()
	limit, cursor, err := PageParams(r)
	if err != nil {
		return operations.Filter{}, err
	}
	from, err := parseTimeParam(q.Get("from"), "from")
	if err != nil {
		return operations.Filter{}, err
	}
	to, err := parseTimeParam(q.Get("to"), "to")
	if err != nil {
		return operations.Filter{}, err
	}
	return operations.Filter{
		Action: q.Get("action"),
		Target: q.Get("target"),
		Result: q.Get("result"),
		From:   from,
		To:     to,
		Limit:  limit,
		Cursor: cursor,
	}, nil
}

// filterByRequestID 只保留 requestId 命中的记录，并重算 total（§1.4 的 requestId 过滤）。
// 命中数天然很小（一个请求最多几条变更），因此这里是线性过滤而不是另建索引。
func filterByRequestID(page operations.Page, requestID string) operations.Page {
	kept := make([]operations.Operation, 0, len(page.Items))
	for _, op := range page.Items {
		if op.RequestID == requestID {
			kept = append(kept, op)
		}
	}
	return operations.Page{Items: kept, NextCursor: "", Total: len(kept)}
}

// ── 共用：错误 ──────────────────────────────────────────────────────────────

// errUsageUnavailable 是用量库不可用时的统一回应：用 storage（磁盘/IO 面）而不是 internal，
// 因为「库打不开」在界面上应给出「用量历史不可用」的明确提示（UC-008 A11）。
func (s *Server) errUsageUnavailable() error {
	if s.Usage == nil {
		return apierr.New(apierr.CodeStorage, "用量库不可用：启动时打开失败，见进程日志")
	}
	return nil
}
