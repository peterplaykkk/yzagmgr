package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ── 各子命令的执行体 ─────────────────────────────────────────────────────────
//
// 每个 exec* 只做三件事：调 REST → 组装（需要时）→ emit/failf。
// 业务语义全在守护进程侧（internal/family、internal/tx、internal/backup），
// CLI 不做任何本地判断——它连被管文件都不打开。

func execHealth(d Deps, cmd Cmd, c *Client) int {
	data, err := c.Do(http.MethodGet, "/api/v1/health", nil)
	if err != nil {
		return failTransport(d, cmd, err)
	}
	return emit(d, cmd, data, formatHealth)
}

func execToolsList(d Deps, cmd Cmd, c *Client) int {
	data, err := c.Do(http.MethodGet, "/api/v1/tools", nil)
	if err != nil {
		return failTransport(d, cmd, err)
	}
	return emit(d, cmd, data, formatToolsList)
}

// execToolsShow 组合两个只读端点：工具详情 + 档位卡清单。
//
// 档位卡清单必须在这里带上：`preview/apply --profile <pid>` 的 pid 就出自它，
// 没有它 CLI 用户无从得知可用的档位卡（REST 的 /tools/{id} 不含卡片列表）。
//
// 组合视图手工拼接（同 emit 的纪律）：两个成员都是 REST 载荷原文，过 json.Marshal
// 会被 compact 压平空白。
func execToolsShow(d Deps, cmd Cmd, c *Client) int {
	tool, err := c.Do(http.MethodGet, "/api/v1/tools/"+url.PathEscape(cmd.Tool), nil)
	if err != nil {
		return failTransport(d, cmd, err)
	}
	profiles, err := c.Do(http.MethodGet, "/api/v1/tools/"+url.PathEscape(cmd.Tool)+"/profiles", nil)
	if err != nil {
		return failTransport(d, cmd, err)
	}
	var buf bytes.Buffer
	buf.WriteString(`{"tool":`)
	buf.Write(tool)
	buf.WriteString(`,"profiles":`)
	buf.Write(profiles)
	buf.WriteByte('}')
	return emit(d, cmd, buf.Bytes(), formatToolsShow)
}

// execPreviewApply 执行预览（KindPreview）或「预览 + 应用」（KindApply）。
//
// apply 的 REST 契约以 previewId 为入参（§4.3 的一次性租约），CLI 的 `apply --profile`
// 因此是这两步的复合命令：先拿到租约再消费它。写前备份语义在守护进程侧的事务执行器里
// （UC-004：没有备份就等于没有写），CLI 不复制也不绕过。
//
// `--model-id`（UC-043）只透传成 REST 的 `modelId`：router 形态下它是**选择器名**
// （已登记请求映射名或别名名），其余形态是策略内 / 卡清单内的模型名——取值校验与文案
// 全在守护进程侧（REST 同一套），CLI 不做任何本地判断。
func execPreviewApply(d Deps, cmd Cmd, c *Client) int {
	base := "/api/v1/tools/" + url.PathEscape(cmd.Tool)
	previewRaw, err := c.Do(http.MethodPost, base+"/preview", previewRequestBody(cmd))
	if err != nil {
		return failTransport(d, cmd, err)
	}
	if cmd.Kind == KindPreview {
		return emit(d, cmd, previewRaw, formatPreview)
	}
	var pv previewData
	if err := json.Unmarshal(previewRaw, &pv); err != nil {
		return failf(d, cmd, ExitAPI, "internal", "解析预览响应失败: %v", err)
	}
	if pv.PreviewID == "" {
		return failf(d, cmd, ExitAPI, "internal", "预览未返回 previewId，无法应用")
	}
	applyRaw, err := c.Do(http.MethodPost, base+"/apply", map[string]string{"previewId": pv.PreviewID})
	if err != nil {
		return failTransport(d, cmd, err)
	}
	return emit(d, cmd, applyRaw, formatApply)
}

// previewRequestBody 组装 §4.3 的预览请求体（preview 与 apply 的第一步共用）。
//
// `--model-id` 缺席时**不写该键**：既有调用（`{"profileId":…}`）的线上字节逐字不变，
// router 形态的模型名照常由守护进程按 UC-043 的解析顺序兜底（策略默认可路由 ⇒ 用它，
// 否则枚举首个）。给了空串同样视为「没给」——与 REST 侧 TrimSpace 后忽略空值同口径。
func previewRequestBody(cmd Cmd) map[string]string {
	body := map[string]string{"profileId": cmd.Profile}
	if modelID := strings.TrimSpace(cmd.ModelID); modelID != "" {
		body["modelId"] = modelID
	}
	return body
}

// execRestore 恢复备份。REST 的恢复通道是**按工具**的（/tools/{id}/restore/*），
// 而 CLI 的入口只有备份编号——归属工具由跨工具备份枚举反查：
//
//   - `restore --backup <id>`：恢复**指定**备份（编号必须唯一命中一个工具）；
//   - `restore`（无 --backup）：撤销**最近一次写入**——恢复全库最新那份备份
//     （分配序精确到纳秒 + 进程内序号，见 parseRecency）。同一次 apply 落下的
//     初始快照与写前备份内容相同，恢复哪份都能回到写前字节。
func execRestore(d Deps, cmd Cmd, c *Client) int {
	refs, err := c.listAllBackups()
	if err != nil {
		return failTransport(d, cmd, err)
	}
	var target *backupRef
	if cmd.Backup != "" {
		for i := range refs {
			if refs[i].Backup.BackupID == cmd.Backup {
				if target != nil {
					return failf(d, cmd, ExitAPI, "invalid_request",
						"备份编号 %s 同时属于多个工具（%s、%s），无法唯一恢复",
						cmd.Backup, target.ToolID, refs[i].ToolID)
				}
				target = &refs[i]
			}
		}
		if target == nil {
			return failf(d, cmd, ExitAPI, "not_found", "备份不存在: %s（`yzagmgr backups list` 可查全部备份）", cmd.Backup)
		}
	} else {
		if len(refs) == 0 {
			return failf(d, cmd, ExitAPI, "not_found", "没有任何备份可恢复（尚未应用过任何配置）")
		}
		target = &refs[0] // listAllBackups 已按新 → 旧排序
	}

	base := "/api/v1/tools/" + url.PathEscape(target.ToolID)
	previewRaw, err := c.Do(http.MethodPost, base+"/restore/preview",
		map[string]string{"backupId": target.Backup.BackupID})
	if err != nil {
		return failTransport(d, cmd, err)
	}
	var pv previewData
	if err := json.Unmarshal(previewRaw, &pv); err != nil {
		return failf(d, cmd, ExitAPI, "internal", "解析恢复预览响应失败: %v", err)
	}
	if pv.PreviewID == "" {
		return failf(d, cmd, ExitAPI, "internal", "恢复预览未返回 previewId，无法应用")
	}
	applyRaw, err := c.Do(http.MethodPost, base+"/restore/apply", map[string]string{"previewId": pv.PreviewID})
	if err != nil {
		return failTransport(d, cmd, err)
	}
	// 组合视图手工拼接（同 emit 的纪律）：apply 成员是 REST 载荷原文。
	// 表头只含两个字符串字段，按 encoding/json 契约 Marshal 不会失败（与 failf 的
	// 失败信封序列化同一判读）。
	head, _ := json.Marshal(struct {
		ToolID   string `json:"toolId"`
		BackupID string `json:"backupId"`
	}{target.ToolID, target.Backup.BackupID})
	var buf bytes.Buffer
	buf.Write(head[:len(head)-1]) // 去掉右花括号
	buf.WriteString(`,"apply":`)
	buf.Write(applyRaw)
	buf.WriteByte('}')
	return emit(d, cmd, buf.Bytes(), formatRestore)
}

// execBackupsList 给出跨工具的备份视图（REST 的备份清单按工具分册，§4.3）。
func execBackupsList(d Deps, cmd Cmd, c *Client) int {
	refs, err := c.listAllBackups()
	if err != nil {
		return failTransport(d, cmd, err)
	}
	wire := backupsListData{Items: make([]backupRefWire, 0, len(refs)), Total: len(refs)}
	for _, r := range refs {
		wire.Items = append(wire.Items, backupRefWire{
			ToolID:   r.ToolID,
			BackupID: r.Backup.BackupID,
			At:       r.Backup.At,
			Reason:   r.Backup.Reason,
			Initial:  r.Backup.Initial,
			Files:    r.Backup.Files,
		})
	}
	// 纯结构体（无 chan/func/NaN）按 encoding/json 契约 Marshal 不会失败
	// （与 restore 表头、failf 失败信封的序列化同一判读）。
	data, _ := json.Marshal(wire)
	return emit(d, cmd, data, formatBackupsList)
}

func execPolicy(d Deps, cmd Cmd, c *Client) int {
	method, path := http.MethodGet, "/api/v1/policy"
	if cmd.Kind == KindPolicyRefresh {
		method, path = http.MethodPost, "/api/v1/policy/refresh"
	}
	data, err := c.Do(method, path, nil)
	if err != nil {
		return failTransport(d, cmd, err)
	}
	return emit(d, cmd, data, formatPolicy)
}

// failTransport 把传输/信封错误映射到退出码：
//   - *UnreachableError ⇒ 4（守护进程未运行——CLI 不自动拉起）；
//   - *APIError ⇒ 1（业务失败，保留契约错误码）；
//   - 其余（信封不可解析等）⇒ 1。
func failTransport(d Deps, cmd Cmd, err error) int {
	var unreach *UnreachableError
	if errors.As(err, &unreach) {
		return failf(d, cmd, ExitUnreach, "daemon_unreachable", "%v", err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		msg := apiErr.Message
		if apiErr.Code == "unauthorized" {
			msg += "（管理令牌不符：守护进程可能已重启并轮换了令牌；CLI 每次自动读 <data_root>/admin.token，直接重试即可）"
		}
		if apiErr.RequestID != "" {
			msg += "（requestId=" + apiErr.RequestID + "）"
		}
		return failf(d, cmd, ExitAPI, apiErr.Code, "%s", msg)
	}
	return failf(d, cmd, ExitAPI, "internal", "%v", err)
}
