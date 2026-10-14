// 响应脱敏的断言（UC-003 A12）：preview 与 apply 的响应正文不含 `sk-` 形态的密钥。
//
// 这条是**安全断言**，此前没有载体：`buildDiff` 直接把写前/写后字节按行原样放进 `diff[].text`，
// 而写前字节就是用户自己的 live 配置——里面常常就有 `ANTHROPIC_AUTH_TOKEN: "sk-ant-..."`。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family/claude"
)

// TestPreviewAndApplyPayloadAreRedacted 守住 UC-003 A12：preview 与 apply 的响应正文
// （差异与掩码配置）不含 `sk-[A-Za-z0-9_-]{16,}` 形态。
//
// 构造刻意让**用户的密钥就在被改动的行上**：把 live 配置写成官方档 + 一枚真密钥，
// 再预览网关档——那一行会从「带密钥」变成「占位符」，因此它必然进 diff。
// 如果 diff 原样输出，密钥就随响应进了浏览器、前端日志与任何抓包。
func TestPreviewAndApplyPayloadAreRedacted(t *testing.T) {
	const (
		seedToken = "sk-live-seed-0123456789abcdefghijklmnop"
		userKey   = "sk-ant-api03-USERLIVEKEY-0123456789abcdef"
	)
	srv := newContractServer(t)
	deps := srv.Deps
	deps.TokenSeed = seedToken
	deps.Logger = quietLog()
	managed, err := New(deps)
	if err != nil {
		t.Fatalf("装配（带真令牌种子）失败: %v", err)
	}
	srv = &contractServer{Server: managed, env: srv.env, token: srv.token, upstream: srv.upstream}

	// live 配置：官方形态（决定键为空）+ 一枚用户自己的密钥。
	live := srv.env.ClaudeSettings()
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatalf("建立 live 目录失败: %v", err)
	}
	seed := `{
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "` + userKey + `",
    "ANTHROPIC_BASE_URL": ""
  }
}
`
	if err := os.WriteFile(live, []byte(seed), 0o600); err != nil {
		t.Fatalf("写入 live 配置失败: %v", err)
	}

	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview",
		`{"profileId":"`+managedCardID+`"}`, true)
	if !preview.ok {
		t.Fatalf("预览失败: %s", preview.body)
	}
	var plan previewPayload
	if err := json.Unmarshal(preview.data, &plan); err != nil {
		t.Fatalf("预览载荷不是预期结构: %v（%s）", err, string(preview.data))
	}
	if len(plan.Files) == 0 || len(plan.Files[0].Diff) == 0 {
		t.Fatalf("预览没有给出差异，本用例没有断言对象: %+v", plan.Files)
	}

	// 「差异确实展示了那一行」——否则「响应里没有密钥」可能只是因为什么都没显示，
	// 断言会变成空转（脱敏 = 掩码值，不是把整行藏起来，见 UC-003 主流程第 3 步）。
	mentionsKey := false
	for _, file := range plan.Files {
		for _, line := range file.Diff {
			if strings.Contains(line.Text, "ANTHROPIC_AUTH_TOKEN") {
				mentionsKey = true
			}
		}
	}
	if !mentionsKey {
		t.Errorf("预览差异里没有承载密钥的键名，脱敏断言无法判定（差异被整体隐藏了？）")
	}

	// ① 用户自己的密钥不许出现在预览响应里。
	if strings.Contains(preview.body, userKey) {
		t.Errorf("预览响应带出了 live 配置里的密钥明文")
	}
	// ② 装配期就绪的真令牌同样不许出现。
	if strings.Contains(preview.body, seedToken) {
		t.Errorf("预览响应带出了装配期令牌明文")
	}
	// ③ 任何 `sk-` 形态都不许出现（前端会把这些响应打进日志与错误上报）。
	for _, file := range plan.Files {
		for _, line := range file.Diff {
			if match := secretShape.FindString(line.Text); match != "" {
				t.Errorf("差异行带出令牌形态 %q：%s", match, strings.TrimSpace(line.Text))
			}
		}
	}
	if match := secretShape.FindString(preview.body); match != "" {
		t.Errorf("预览响应正文出现令牌形态 %q", match)
	}

	// ④ apply 的响应同样要干净。
	applied := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	if !applied.ok {
		t.Fatalf("应用失败: %s", applied.body)
	}
	if strings.Contains(applied.body, userKey) || strings.Contains(applied.body, seedToken) {
		t.Errorf("应用响应带出了令牌明文")
	}
	if match := secretShape.FindString(applied.body); match != "" {
		t.Errorf("应用响应正文出现令牌形态 %q", match)
	}

	// ⑤ 落盘侧不受影响：占位符照写（脱敏是**响应**的事，不是写盘的事）。
	raw, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("读回 live 配置失败: %v", err)
	}
	if !strings.Contains(string(raw), claude.TokenPlaceholder) {
		t.Errorf("网关档的凭据位应是占位符，实际: %s", raw)
	}
	if strings.Contains(string(raw), userKey) {
		t.Errorf("网关档落盘后仍带着用户密钥: %s", raw)
	}
}
