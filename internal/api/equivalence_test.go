// curl 与界面等价（UC-006 A2）的断言。
//
// 契约的原话是「对同一操作，curl 调用与界面点击产生**逐字节相同**的盘上结果与相同的
// 操作日志条目形状」。中间件的 `TestCurlEquivalenceWithoutOrigin` 只守了「无 Origin 也放行」
// 这半句（否则脚本全红），这里守的是真正的等价：两次独立执行必须落到同一份字节与同一形状的日志。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/operations"
)

// browserHeaders 是管理页真实发出的那组头（Origin/Referer 决定 CSRF 判定，
// 令牌同样经 `X-Agmgr-Token` 头带——界面与 curl 走的是**同一个**入口）。
func browserHeaders(apiPort int, token string) map[string]string {
	origin := "http://127.0.0.1:" + itoa(apiPort)
	return map[string]string{
		"X-Agmgr-Token":    token,
		"Origin":           origin,
		"Referer":          origin + "/tools/claude",
		"User-Agent":       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/141.0 Safari/537.36",
		"Sec-Fetch-Mode":   "cors",
		"Sec-Fetch-Site":   "same-origin",
		"Accept":           "application/json, text/plain, */*",
		"X-Requested-With": "XMLHttpRequest",
	}
}

// curlHeaders 是脚本/curl 形态：只有令牌，没有来源头与浏览器头。
func curlHeaders(token string) map[string]string {
	return map[string]string{
		"X-Agmgr-Token": token,
		"User-Agent":    "curl/8.7.1",
		"Accept":        "*/*",
	}
}

// oplogShape 是操作日志里与调用方无关的那部分（时间、id、requestId 每次必然不同）。
type oplogShape struct {
	Action      string
	Target      string
	Result      string
	ErrorCode   string
	HasBackupID bool
}

// operationShapes 读某实例的操作日志并投影成可比形状（新 → 旧）。
func operationShapes(t *testing.T, srv *contractServer) []oplogShape {
	t.Helper()
	raw, err := os.ReadFile(srv.env.OperationsFile())
	if err != nil {
		t.Fatalf("读操作日志失败: %v", err)
	}
	out := []oplogShape{}
	for _, line := range splitJSONLines(string(raw)) {
		var op operations.Operation
		if err := json.Unmarshal([]byte(line), &op); err != nil {
			t.Fatalf("操作日志有一行不是合法记录: %v（%s）", err, line)
		}
		out = append(out, oplogShape{
			Action: op.Action, Target: op.Target, Result: op.Result,
			ErrorCode: op.ErrorCode, HasBackupID: op.BackupID != "",
		})
	}
	return out
}

// splitJSONLines 切出非空行（JSONL）。
func splitJSONLines(raw string) []string {
	out := []string{}
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i < len(raw) && raw[i] != '\n' {
			continue
		}
		if line := raw[start:i]; line != "" {
			out = append(out, line)
		}
		start = i + 1
	}
	return out
}

// normalizeApplyIdentity 把记账里「每次应用必然不同」的两处（appliedTx / appliedAt）
// 归一成占位符，供两次独立执行的逐字节比对使用。
func normalizeApplyIdentity(raw []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw
	}
	if tools, ok := doc["tools"].(map[string]any); ok {
		for _, entry := range tools {
			record, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"appliedTx", "appliedAt"} {
				if _, present := record[key]; present {
					record[key] = "<per-run>"
				}
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// runSameOperation 在给定实例上跑一遍「预览受管卡 → 应用」并返回预览响应
// （去掉每次都不同的 previewId/expiresAt 之后可比）。受管卡是派生视图，
// ensure-gateway-profiles 端点已随 §4.1 删除——design §2.2。
func runSameOperation(t *testing.T, srv *contractServer, hdr map[string]string) string {
	t.Helper()
	preview := callWithHeaders(t, srv, http.MethodPost, "/api/v1/tools/claude/preview",
		`{"profileId":"`+managedCardID+`"}`, hdr)
	if !preview.ok {
		t.Fatalf("预览失败: %s", preview.body)
	}
	var plan previewPayload
	if err := json.Unmarshal(preview.data, &plan); err != nil {
		t.Fatalf("预览载荷不是预期结构: %v", err)
	}
	apply := callWithHeaders(t, srv, http.MethodPost, "/api/v1/tools/claude/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, hdr)
	if !apply.ok {
		t.Fatalf("应用失败: %s", apply.body)
	}
	// 预览载荷里只有三处每次不同：previewId、expiresAt，以及隔离根下的**绝对路径**
	// （两个实例各有自己的临时数据根）。三处归一后再比，其余字段必须逐字节一致。
	plan.PreviewID = ""
	plan.ExpiresAt = ""
	for i := range plan.Files {
		plan.Files[i].Path = strings.ReplaceAll(plan.Files[i].Path, srv.env.Home, "<ISO-HOME>")
	}
	blob, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("预览载荷编码失败: %v", err)
	}
	return string(blob)
}

// TestCurlAndUIPathsProduceIdenticalDiskResult 守住 UC-006 A2：同一操作的 curl 调用与
// 界面点击产生**逐字节相同**的盘上结果与**相同形状**的操作日志条目。
//
// 两个独立实例各自跑一遍：同一个实例连跑两次是幂等（那不是本用例要证的东西），
// 只有两次独立执行落在同一份字节上，才说明「界面没有特权通道」。
func TestCurlAndUIPathsProduceIdenticalDiskResult(t *testing.T) {
	curlSrv := newContractServer(t)
	uiSrv := newContractServer(t)

	curlPreview := runSameOperation(t, curlSrv, curlHeaders(curlSrv.token))
	uiPreview := runSameOperation(t, uiSrv, browserHeaders(uiSrv.APIPort, uiSrv.token))
	if curlPreview != uiPreview {
		t.Errorf("两次调用的预览载荷不同：\ncurl: %s\n界面: %s", curlPreview, uiPreview)
	}

	// ① 盘上结果逐字节相同：受管文件 + 建档记账。
	//
	// 记账里的 `appliedTx`/`appliedAt`（2026-09-27 additive，UC-092 A10）是**每次应用
	// 必然不同**的事务身份与时刻（与预览载荷的 previewId/expiresAt 同性质：两次独立
	// 执行不可能落在同一个租约 ID 与同一纳秒上），因此与上面预览载荷同一处理——归一
	// 这两处后再逐字节比对，其余字段必须一字不差。
	for _, file := range []struct {
		name string
		a    string
		b    string
	}{
		{"claude 的 live 配置", curlSrv.env.ClaudeSettings(), uiSrv.env.ClaudeSettings()},
		{"档位卡记账", curlSrv.env.ManagedDir() + "/profiles.json", uiSrv.env.ManagedDir() + "/profiles.json"},
	} {
		a, errA := os.ReadFile(file.a)
		b, errB := os.ReadFile(file.b)
		if errA != nil || errB != nil {
			t.Fatalf("读 %s 失败: %v / %v", file.name, errA, errB)
		}
		if file.name == "档位卡记账" {
			a, b = normalizeApplyIdentity(a), normalizeApplyIdentity(b)
		}
		if string(a) != string(b) {
			t.Errorf("%s 逐字节不同：\ncurl: %q\n界面: %q", file.name, a, b)
		}
	}

	// ② 操作日志条目形状相同（顺序、动作、目标、结果、有无备份编号）。
	curlOps := operationShapes(t, curlSrv)
	uiOps := operationShapes(t, uiSrv)
	if len(curlOps) != len(uiOps) {
		t.Fatalf("操作日志条数不同：curl %d 条，界面 %d 条", len(curlOps), len(uiOps))
	}
	for i := range curlOps {
		if curlOps[i] != uiOps[i] {
			t.Errorf("第 %d 条操作日志形状不同：\ncurl: %+v\n界面: %+v", i+1, curlOps[i], uiOps[i])
		}
	}
	// 形状本身也要有内容：全是空形状说明投影写错了。
	if len(curlOps) < 2 {
		t.Errorf("操作日志只有 %d 条，期望至少 2 条（预览 + 应用；建档端点已随 §4.1 删除）", len(curlOps))
	}
}
