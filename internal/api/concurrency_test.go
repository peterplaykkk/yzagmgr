// 并发 apply 的串行化（UC-006 A10）。
//
// 全仓此前没有并发 apply 的断言，而 `preview_in_use` 这个错误码在 Go 侧**没有任何产出点**
// （只在 web 的文案表里被枚举）。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// ── UC-006 A10 ─────────────────────────────────────────────────────────────

// TestConcurrentApplySerializes 守住 UC-006 A10：对同一工具并发发起两次 apply，
// **恰有一次**成功、另一次得到 `config_changed` 或 `preview_in_use`，盘上不出现交错写入。
//
// 这里用 6 份租约同时开火而不是两份：窗口越窄越容易「看起来像串行化」，
// 并发度提高才能让「有没有一把针对同一工具的锁」这件事显形。
func TestConcurrentApplySerializes(t *testing.T) {
	const concurrent = 6

	srv := newContractServer(t)
	// 参照进程：单独一次应用产生的**目标字节**，用来判定并发结果有没有被交错写坏。
	reference := newContractServer(t)
	applyCard(t, reference, family.Claude, managedCardID)
	wantBytes, err := os.ReadFile(reference.env.ClaudeSettings())
	if err != nil {
		t.Fatalf("读参照 live 配置失败: %v", err)
	}

	// 先取 6 份预览（每份一个 previewId），再同时应用。
	previewIDs := make([]string, 0, concurrent)
	for i := 0; i < concurrent; i++ {
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/preview",
			`{"profileId":"`+managedCardID+`"}`, true)
		if !res.ok {
			t.Fatalf("第 %d 份预览失败: %s", i+1, res.body)
		}
		var plan previewPayload
		if err := json.Unmarshal(res.data, &plan); err != nil {
			t.Fatalf("预览载荷不是预期结构: %v", err)
		}
		previewIDs = append(previewIDs, plan.PreviewID)
	}

	type outcome struct {
		ok   bool
		code apierr.Code
		body string
	}
	results := make([]outcome, concurrent)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, id := range previewIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			<-start
			// 直接打 Server（in-package 调用），避开 callRaw 里的 t.Fatalf（它在非测试 goroutine 里不安全）。
			res := rawCall(srv, http.MethodPost, "/api/v1/tools/claude/apply",
				`{"previewId":"`+id+`"}`, tokenHeaderFor(srv))
			results[i] = outcome{ok: res.ok, code: res.code, body: res.body}
		}(i, id)
	}
	close(start)
	wg.Wait()

	successes, rejects := 0, map[apierr.Code]int{}
	for i, res := range results {
		if res.ok {
			successes++
			continue
		}
		rejects[res.code]++
		if res.code != apierr.CodeConfigChanged && res.code != apierr.CodePreviewInUse {
			t.Errorf("第 %d 次并发 apply 的拒绝码 = %q，期望 config_changed 或 preview_in_use（%s）",
				i+1, res.code, res.body)
		}
	}
	if successes != 1 {
		t.Errorf("并发 %d 次 apply 的成功数 = %d，期望恰为 1（其余必须被拒：%v）",
			concurrent, successes, rejects)
	}

	// 盘上不得出现交错写入：最终字节必须与「单独应用一次」逐字节相同。
	gotBytes, err := os.ReadFile(srv.env.ClaudeSettings())
	if err != nil {
		t.Fatalf("读 live 配置失败: %v", err)
	}
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("并发 apply 后的 live 字节与单次应用不同：\n实际 %q\n期望 %q", gotBytes, wantBytes)
	}
	// 也不许留下任何临时文件。
	entries, err := os.ReadDir(filepath.Dir(srv.env.ClaudeSettings()))
	if err != nil {
		t.Fatalf("读 live 目录失败: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("并发 apply 留下了临时文件: %s", entry.Name())
		}
	}

	// 操作日志与响应必须对得上：成功 1 条、失败 concurrent-1 条。
	ops := srv.callRaw(t, http.MethodGet, "/api/v1/operations?limit=200", "", true)
	if !ops.ok {
		t.Fatalf("读操作日志失败: %s", ops.body)
	}
	var page struct {
		Items []struct {
			Action string `json:"action"`
			Result string `json:"result"`
		} `json:"items"`
	}
	if err := json.Unmarshal(ops.data, &page); err != nil {
		t.Fatalf("操作日志不是预期结构: %v", err)
	}
	okCount, failCount := 0, 0
	for _, op := range page.Items {
		if op.Action != "tool.apply" {
			continue
		}
		switch op.Result {
		case "ok":
			okCount++
		case "failed":
			failCount++
		}
	}
	if okCount != successes {
		t.Errorf("操作日志里 tool.apply 的 ok 数 = %d，与响应里的成功数 %d 不一致", okCount, successes)
	}
	if failCount != rejects[apierr.CodeConfigChanged]+rejects[apierr.CodePreviewInUse] {
		t.Errorf("操作日志里 tool.apply 的 failed 数 = %d，与响应里的失败数 %d 不一致",
			failCount, rejects[apierr.CodeConfigChanged]+rejects[apierr.CodePreviewInUse])
	}
}

// rawCall 是不依赖 testing.T 的请求执行器（供并发 goroutine 使用）。
func rawCall(srv *contractServer, method, path, body string, hdr map[string]string) envelope {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	res := rec.Result()
	out := envelope{status: res.StatusCode, header: res.Header, body: rec.Body.String()}
	var parsed struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *apierr.Error   `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err == nil {
		out.ok = parsed.OK
		out.hasData = parsed.Data != nil
		out.data = parsed.Data
		if parsed.Error != nil {
			out.code = parsed.Error.Code
			out.message = parsed.Error.Message
		}
	}
	return out
}
