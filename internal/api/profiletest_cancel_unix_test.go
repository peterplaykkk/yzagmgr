//go:build unix

// §4.6 停止入口的**真实子进程**断言（UC-097 A4 的自动化面）。
//
// 仓库纪律「单元测试绝不跑真 CLI」在这里的边界：跑的不是任何真实工具 CLI，而是一个
// 一次性**桩脚本**（脚本自己再 spawn 一个子进程）——这正是 A4 指定的自动化形态
// （真机进程树/时间线的取证属授权人工 lane，登记于 doc/api-v1.md §4.6）。
// 它断言的是停止入口的真实语义：
//   - 终止走**进程组**回收（脚本与它拉起的子进程一起收掉，cancel 后无孤儿存活）；
//   - 临时根按超时同一路径清理（零残留）；
//   - 原测试请求与停止响应都拿到 status=cancelled 的终态（不谎称成功、不谎称回滚）。
package api

import (
	"encoding/json"
	"errors"
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

// pidAlive 报告 pid 是否仍然存在（ESRCH = 不存在；EPERM 等按「存在」处理，宁可保守）。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return !errors.Is(err, syscall.ESRCH)
}

// TestProfileTestCancelReapsStubSubprocessGroup 是停止入口面唯一 spawn 真进程的用例
// （执行面输出流/竞态的桩进程用例另见 profiletest_exec_unix_test.go）：
// 桩 CLI 脚本（`sh`）再起一个 `sleep 300` 子进程并 `wait`，停止入口必须把**整组**回收。
func TestProfileTestCancelReapsStubSubprocessGroup(t *testing.T) {
	srv, _ := testableServer(t)

	// 桩脚本：写回两个 PID（自己与子进程）与工作目录（= 临时隔离根）后挂住。
	dir := t.TempDir()
	pidsFile := filepath.Join(dir, "pids.txt")
	rootFile := filepath.Join(dir, "root.txt")
	script := filepath.Join(dir, "stub-cli.sh")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$$\" > '" + pidsFile + "'\n" +
		"printf '%s\\n' \"$PWD\" > '" + rootFile + "'\n" +
		"sleep 300 &\n" +
		"printf '%s\\n' \"$!\" >> '" + pidsFile + "'\n" +
		"wait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写桩脚本失败: %v", err)
	}

	// 走**生产**执行面（真 spawn 子进程）；解析面钉到桩脚本（配方参数被 sh 忽略）。
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

	leader, child := 0, 0
	waitFor(t, "桩脚本写回两个 PID", func() bool {
		raw, err := os.ReadFile(pidsFile)
		if err != nil {
			return false
		}
		fields := strings.Fields(strings.TrimSpace(string(raw)))
		if len(fields) != 2 {
			return false
		}
		leader, err = strconv.Atoi(fields[0])
		if err != nil {
			return false
		}
		child, err = strconv.Atoi(fields[1])
		return err == nil && leader > 0 && child > 0
	})
	// 兜底清理：任何断言失败都不留孤儿（pid <= 0 时绝不调用——kill(0) 会打到本进程组）。
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
	if !pidAlive(leader) || !pidAlive(child) {
		t.Fatalf("桩进程树未存活（leader=%d child=%d）：夹具没起来", leader, child)
	}

	var root string
	waitFor(t, "桩脚本写回临时根", func() bool {
		raw, err := os.ReadFile(rootFile)
		if err != nil {
			return false
		}
		root = strings.TrimSpace(string(raw))
		return root != ""
	})
	if !strings.Contains(root, profileTestDirPrefix) {
		t.Fatalf("桩脚本的工作目录 = %q（期望临时隔离根）", root)
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		t.Fatalf("临时根状态异常: %v / %v", st, err)
	}

	// 停止：响应必须如实给 cancelled 终态（进程组已回收）。
	_, data := cancelTestRun(t, srv, "pi")
	if !data.Active || !data.Stopped || data.Kind != controlledRunTest {
		t.Fatalf("停止载荷 = %+v（期望 active/cancelled/test）", data)
	}
	if data.Result == nil || data.Result.Status != profileTestStatusCancelled || !data.Result.Cancelled {
		t.Fatalf("终止后的终态载荷 = %+v（期望 status=cancelled）", data.Result)
	}

	// ① 无孤儿：脚本与它拉起的子进程都必须消失（整组回收的直接证据）。
	waitFor(t, "进程组整组回收（无孤儿存活）", func() bool {
		return !pidAlive(leader) && !pidAlive(child)
	})
	// ② 临时根零残留（与超时路径同一套清理）。
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("停止后临时根未清理: %v", err)
	}

	// ③ 原测试请求拿到的终态与停止响应同源：status=cancelled、cancelled=true。
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
	case <-time.After(10 * time.Second):
		t.Fatal("停止后原测试请求没有返回")
	}
}
