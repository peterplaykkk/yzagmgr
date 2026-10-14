package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Client 是管理 API 的最小 REST 客户端（纯客户端：零 API 变更）。
//
// 鉴权走 `X-Agmgr-Token` 头（doc/api-v1.md §1.1）；**绝不**发送 Origin/Referer
// （Guard 把非本机管理页来源判成 forbidden_origin，命令行调用不带来源即天然合规）。
type Client struct {
	Base  string // 如 http://127.0.0.1:17022
	Token string // 管理令牌（可为空：health/version 免鉴权）
	HTTP  *http.Client
}

// maxResponseBytes 是响应体上限：apply/restore 的 diff 载荷可能不小，但也不该无界。
const maxResponseBytes = 32 << 20

// UnreachableError 是「管理 API 不可达」——守护进程未运行（或端口不对）。
// 退出码 4 的唯一来源；CLI 刻意**不**自动拉起守护进程。
type UnreachableError struct {
	Base string
	Err  error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("无法连接管理 API %s —— 守护进程未运行？先运行 `yzagmgr serve`（或启动 yzagmgr.app）再执行命令: %v",
		e.Base, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// APIError 是信封 ok:false 的业务失败（doc/api-v1.md §1.2/§1.3）。
type APIError struct {
	Code      string
	Message   string
	Details   map[string]any
	RequestID string
}

func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s: %s (requestId=%s)", e.Code, e.Message, e.RequestID)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Do 发一次请求并解信封：成功返回 `data` 载荷原文（逐字节保留，不重编码），
// 失败返回 *APIError，传输层失败返回 *UnreachableError。
func (c *Client) Do(method, path string, body any) (json.RawMessage, error) {
	if c.Base == "" {
		return nil, fmt.Errorf("管理 API 基址未设置")
	}
	var reader io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("编码请求体失败: %w", err)
		}
		reader = bytes.NewReader(blob)
	}
	req, err := http.NewRequest(method, c.Base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("X-Agmgr-Token", c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &UnreachableError{Base: c.Base, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &UnreachableError{Base: c.Base, Err: fmt.Errorf("读取响应失败: %w", err)}
	}

	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		excerpt := string(raw)
		if len(excerpt) > 200 {
			excerpt = excerpt[:200] + "…"
		}
		return nil, fmt.Errorf("管理 API 响应不是契约信封（HTTP %d）: %s", resp.StatusCode, excerpt)
	}
	if !envelope.OK {
		ae := &APIError{RequestID: envelope.RequestID}
		if envelope.Error != nil {
			ae.Code = envelope.Error.Code
			ae.Message = envelope.Error.Message
			ae.Details = envelope.Error.Details
		} else {
			ae.Code = "internal"
			ae.Message = "响应缺少 error 字段"
		}
		return nil, ae
	}
	if len(envelope.Data) == 0 {
		// 契约保证成功恒带 data（§1.2）；真出现缺位时给空对象而不是让调用方解出 null。
		return json.RawMessage("{}"), nil
	}
	return envelope.Data, nil
}

// ── 备份枚举（restore / backups list 共用）───────────────────────────────────

// backupRef 是一份备份加它的归属工具（REST 的备份清单按工具分册，CLI 需要跨工具视图）。
type backupRef struct {
	ToolID  string
	Backup  backupItem
	Recency recency
}

// recency 是备份编号里的分配序（bk-<纳秒时间戳>-<进程内序号>）。
//
// 为什么不用 `at`：它截断到秒，同一次 apply 里落的初始快照与写前备份同一秒，
// 分不出先后；编号里的分配序是精确的分配次序（registry.allocIDLocked）。
type recency struct {
	Stamp    int64
	Seq      int64
	Parsed   bool
	At       string // RFC3339（fallback 排序用）
	BackupID string
}

// parseRecency 解 `bk-<ns>-<seq>`；形态不符时 Parsed=false（退回 at/id 字符串排序）。
func parseRecency(id, at string) recency {
	r := recency{At: at, BackupID: id}
	rest, ok := strings.CutPrefix(id, "bk-")
	if !ok {
		return r
	}
	nsStr, seqStr, ok := strings.Cut(rest, "-")
	if !ok {
		return r
	}
	ns, err1 := strconv.ParseInt(nsStr, 10, 64)
	seq, err2 := strconv.ParseInt(seqStr, 10, 64)
	if err1 != nil || err2 != nil {
		return r
	}
	r.Stamp, r.Seq, r.Parsed = ns, seq, true
	return r
}

// newerThan 报告 r 是否比 o 更新（分配序优先，解析不了退回 at 再退回编号）。
func (r recency) newerThan(o recency) bool {
	if r.Parsed && o.Parsed {
		if r.Stamp != o.Stamp {
			return r.Stamp > o.Stamp
		}
		if r.Seq != o.Seq {
			return r.Seq > o.Seq
		}
	}
	if r.At != o.At {
		return r.At > o.At
	}
	return r.BackupID > o.BackupID
}

// listAllBackups 枚举全部工具的全部备份（新 → 旧）。
func (c *Client) listAllBackups() ([]backupRef, error) {
	toolsRaw, err := c.Do(http.MethodGet, "/api/v1/tools", nil)
	if err != nil {
		return nil, err
	}
	var tools []toolBrief
	if err := json.Unmarshal(toolsRaw, &tools); err != nil {
		return nil, fmt.Errorf("解析工具清单失败: %w", err)
	}
	out := make([]backupRef, 0, 16)
	for _, t := range tools {
		cursor := ""
		for page := 0; page < 64; page++ {
			path := "/api/v1/tools/" + url.PathEscape(t.ID) + "/backups?limit=200"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			raw, err := c.Do(http.MethodGet, path, nil)
			if err != nil {
				return nil, err
			}
			var p backupPage
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("解析备份清单失败（%s）: %w", t.ID, err)
			}
			for _, b := range p.Items {
				out = append(out, backupRef{ToolID: t.ID, Backup: b, Recency: parseRecency(b.BackupID, b.At)})
			}
			if p.NextCursor == "" {
				break
			}
			cursor = p.NextCursor
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Recency.newerThan(out[j].Recency) })
	return out, nil
}
