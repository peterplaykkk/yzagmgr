//go:build unix

// §4.6 配置测试执行面的**真实子进程**输出流断言（C-2fix，2026-09-27）。
//
// 背景（C-2 首轮核验 issue1）：execProfileTestCLI 曾在 cmd.Start() 成功后立刻读
// cappedStream.String()/errOut.String()——读方（请求 goroutine）与 os/exec 的
// stdout/stderr 拷贝 goroutine（同一 bytes.Buffer 的写方）无同步并发，-race 下
// 桩 CLI 只要写出任何字节就报 DATA RACE（红验日志：
// reports/evidence-c2fix-go-20260927/f2-race-red.log）。回归口径：桩 CLI
// **打印 stdout+stderr** 的自然完成与停止取消两个用例，在 -race -count=3 下零
// DATA RACE；同时断言两条流的内容真的被有界捕获（防修复把「早读」改成「不读」）。
//
// 与「单元测试绝不跑真 CLI」纪律的边界同 profiletest_cancel_unix_test.go：跑的是
// 一次性桩脚本（sh），不是任何真实工具 CLI。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// TestProfileTestExecStubOutputStreamsToCompletion 守自然完成路径：桩 CLI 向
// stdout/stderr 各写一行后正常退出 ⇒ 两条流都被有界捕获（模型解析自 stdout 事件流），
// 全过程（-race 下）不得出现对捕获缓冲的无同步并发读写。
func TestProfileTestExecStubOutputStreamsToCompletion(t *testing.T) {
	srv, _ := testableServer(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "stub-print-exit.sh")
	challengeFile := filepath.Join(dir, "challenge.txt")
	body := profileTestShellChallengePrelude(challengeFile) +
		"printf '%s\\n' 'stub stderr line' >&2\n" +
		profileTestShellStructuredAnswer("pi") +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写桩脚本失败: %v", err)
	}
	// 走**生产**执行面（真 spawn 桩进程）；解析面钉到桩脚本（配方参数被 sh 忽略）。
	srv.profileTestExecute = nil
	srv.profileTestResolveCLI = func(family.ID, string) string { return script }

	res, payload := profileTestResponseOf(t, srv, "pi")
	if payload.Status != profileTestStatusOK {
		t.Fatalf("自然完成终态 = %q（期望 ok）：reason=%q output=%q",
			payload.Status, payload.Reason, payload.Output)
	}
	if payload.ExitCode != 0 || payload.TimedOut {
		t.Errorf("退出码/超时 = %d/%v（期望 0/false）", payload.ExitCode, payload.TimedOut)
	}
	if payload.Model != "stub-stream-model" || payload.Provider != "stub-stream-provider" {
		t.Errorf("模型解析 = %q/%q（期望来自桩 stdout 事件流）", payload.Model, payload.Provider)
	}
	if payload.Output != "原始输出已脱敏并隐藏" {
		t.Errorf("legacy test result should expose only the safe output summary: %q", payload.Output)
	}
	challenge, err := os.ReadFile(challengeFile)
	if err != nil {
		t.Fatalf("stub did not receive the per-run challenge prompt: %v", err)
	}
	assertProfileTestRunDoesNotLeak(t, srv, res, strings.TrimSpace(string(challenge)), "")
	if strings.Contains(payload.Output, "stub stderr line") {
		t.Errorf("provider diagnostics must not be returned: %q", payload.Output)
	}
}

// TestProfileTestExecStubOutputSurvivesCancel 守停止取消路径的同一性质：桩 CLI 先向
// stdout/stderr 各写一行、再挂住，停止入口按进程组回收 ⇒ 取消前已产出的行如实进终态
// 载荷（缓冲等 Wait 收尾后再读，不丢已写内容），且（-race 下）无数据竞争。
func TestProfileTestExecStubOutputSurvivesCancel(t *testing.T) {
	srv, _ := testableServer(t)

	dir := t.TempDir()
	pidsFile := filepath.Join(dir, "pids.txt")
	readyFile := filepath.Join(dir, "ready.txt")
	challengeFile := filepath.Join(dir, "challenge.txt")
	script := filepath.Join(dir, "stub-print-hold.sh")
	body := profileTestShellChallengePrelude(challengeFile) +
		"printf '%s\\n' 'stub stdout before cancel'\n" +
		"printf '%s\\n' 'stub stderr before cancel' >&2\n" +
		"sleep 300 &\n" +
		"printf '%s\\n' \"$$\" > '" + pidsFile + "'\n" +
		"printf '%s\\n' \"$!\" >> '" + pidsFile + "'\n" +
		"printf 'ready\\n' > '" + readyFile + "'\n" +
		"wait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写桩脚本失败: %v", err)
	}
	srv.profileTestExecute = nil
	srv.profileTestResolveCLI = func(family.ID, string) string { return script }

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/tools/pi/profiles/"+managedCardID+"/test", nil)
		req.Header.Set("X-Agmgr-Token", srv.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		done <- rec
	}()

	// 兜底清理：任何断言失败都不留孤儿（pid <= 0 时绝不调用——kill(0) 会打到本进程组）。
	leader, child := 0, 0
	t.Cleanup(func() {
		for _, pid := range []int{leader, child} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		if leader > 0 {
			_ = syscall.Kill(-leader, syscall.SIGKILL)
		}
	})
	waitFor(t, "桩脚本打印两行后落 ready 标记", func() bool {
		if raw, err := os.ReadFile(pidsFile); err == nil {
			fields := strings.Fields(strings.TrimSpace(string(raw)))
			if len(fields) == 2 {
				leader, _ = strconv.Atoi(fields[0])
				child, _ = strconv.Atoi(fields[1])
			}
		}
		_, readyErr := os.Stat(readyFile)
		return readyErr == nil
	})

	_, data := cancelTestRun(t, srv, "pi")
	if !data.Active || !data.Stopped || data.Kind != controlledRunTest {
		t.Fatalf("停止载荷 = %+v（期望 active/cancelled/test）", data)
	}
	if data.Result == nil || data.Result.Status != profileTestStatusCancelled || !data.Result.Cancelled {
		t.Fatalf("终态载荷 = %+v（期望 status=cancelled）", data.Result)
	}
	if data.Result.Output != "原始输出已脱敏并隐藏" {
		t.Errorf("cancelled profile-test result should expose only the safe output summary: %q", data.Result.Output)
	}
	if strings.Contains(data.Result.Output, "stub stdout before cancel") || strings.Contains(data.Result.Output, "stub stderr before cancel") {
		t.Errorf("cancel response exposed raw provider output: %q", data.Result.Output)
	}
	if raw, err := os.ReadFile(challengeFile); err != nil {
		t.Fatalf("stub did not receive the per-run challenge prompt: %v", err)
	} else if strings.Contains(data.Result.Output, strings.TrimSpace(string(raw))) {
		t.Errorf("cancel response exposed its challenge: %q", data.Result.Output)
	}

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("原测试请求状态码 = %d（期望 200）: %s", rec.Code, rec.Body.String())
		}
		var parsed struct {
			OK   bool                `json:"ok"`
			Data *profileTestPayload `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil || !parsed.OK || parsed.Data == nil {
			t.Fatalf("原测试请求信封不可解析: %v（%s）", err, rec.Body.String())
		}
		if parsed.Data.Status != profileTestStatusCancelled || !parsed.Data.Cancelled {
			t.Fatalf("原测试请求终态 = %+v（期望 cancelled）", parsed.Data)
		}
		if parsed.Data.Output != "原始输出已脱敏并隐藏" {
			t.Errorf("profile-test HTTP result exposed provider output: %q", parsed.Data.Output)
		}
		if strings.Contains(rec.Body.String(), "stub stdout before cancel") ||
			strings.Contains(rec.Body.String(), "stub stderr before cancel") {
			t.Errorf("profile-test HTTP response leaked provider diagnostics: %s", rec.Body.String())
		}
		if raw, err := os.ReadFile(challengeFile); err != nil {
			t.Errorf("stub did not receive the per-run challenge prompt: %v", err)
		} else if strings.Contains(rec.Body.String(), strings.TrimSpace(string(raw))) {
			t.Errorf("profile-test HTTP response leaked its challenge: %s", rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("停止后原测试请求没有返回")
	}
	// 桩进程组整组回收（与既有停止用例同口径；避免桩进程在测试结束后继续存活）。
	waitFor(t, "桩进程组整组回收", func() bool { return !pidAlive(leader) && !pidAlive(child) })
}
