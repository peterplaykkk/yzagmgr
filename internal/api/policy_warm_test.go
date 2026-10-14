package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/policy"
)

// 本文件守 warmPolicy 的启动接线与「缺策略是一种状态而不是错误」语义。
// 源已切换为本地策略文件（design §2.3：匿名拉取删除，Refresh = 重读磁盘，
// 零出网）；原「假网关应答」夹具改为**写盘载荷**，原状态栏子菜单断言随
// tray 桌面面删除改指 profiles 列表。

// warmPolicyData 是种子策略的 payload（形状 = GET /policy 的响应，doc/api-v1.md §3）。
const warmPolicyData = `{"version":8,"defaultModel":"deepseek-flash",` +
	`"models":[{"id":"deepseek-flash","name":"DeepSeek Flash","supportsImages":true,` +
	`"maxInputTokens":1000000,"maxOutputTokens":384000}],` +
	`"catalogs":{}}`

// writePolicyFile 把 payload 包成策略仓库的盘上形态并写入（模拟 YZAGMGR_POLICY_FILE 播种）。
func writePolicyFile(t *testing.T, srv *contractServer, payload string) {
	t.Helper()
	document := fmt.Sprintf(`{"schema":1,"origin":%q,"fetchedAt":%q,"payload":%s}`,
		contractOrigin, time.Now().Format(time.RFC3339), payload)
	if err := os.WriteFile(srv.env.PolicyFile(), []byte(document), 0o600); err != nil {
		t.Fatalf("写入策略文件失败: %v", err)
	}
}

// freshPolicyRoot 把夹具还原成「全新数据根」：策略文件不在、内存也没有缓存。
//
// 为什么不能直接用夹具现状：newContractServer 刻意把种子策略写进盘，别的契约用例才有
// 默认模型可用；而首启缺陷恰恰只在**一份策略都没见过**时出现。
func freshPolicyRoot(t *testing.T, srv *contractServer) {
	t.Helper()
	if err := os.Remove(srv.env.PolicyFile()); err != nil {
		t.Fatalf("移除种子策略失败: %v", err)
	}
	srv.Policy = policy.NewStore(srv.env.PolicyFile(), policy.Options{})
}

// TestWarmPolicyPopulatesFreshRoot 守住首启症状：全新数据根下，预热前 profileList 直接
// 报「模型策略」缺失，预热（读盘）采纳播种的策略后给出真实卡片。
//
// 存在理由（原样保留）：策略的另一条读取路径只有管理页挂载时打的 `POST /policy/refresh`，
// 于是「装完只挂菜单栏、还没打开过管理页」的人看到一个全灰的面——首启在这里退步。
// 源切换后「预热」= 把装配期播种（YZAGMGR_POLICY_FILE → managed/policy.json）读进内存。
func TestWarmPolicyPopulatesFreshRoot(t *testing.T) {
	srv := newContractServer(t)
	freshPolicyRoot(t, srv)

	// 现场：策略缺失让 profileList 直接报错。
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/claude/profiles", "", true)
	if res.ok {
		t.Fatalf("策略缺失时 profileList 不该成功: %s", res.body)
	}
	if !strings.Contains(res.body, "模型策略") {
		t.Errorf("失败原因应指向策略缺失，实际: %s", res.body)
	}

	// 播种策略文件（装配期形态）后预热：读盘采纳，零出网。
	writePolicyFile(t, srv, warmPolicyData)
	srv.warmPolicy()

	res = srv.callRaw(t, http.MethodGet, "/api/v1/tools/claude/profiles", "", true)
	if !res.ok {
		t.Fatalf("预热后 profileList 仍失败: %s", res.body)
	}
	var listed struct {
		Items []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(res.data, &listed); err != nil {
		t.Fatalf("profiles 载荷不是预期结构: %v（%s）", err, res.data)
	}
	names := make([]string, 0, len(listed.Items))
	for _, item := range listed.Items {
		names = append(names, item.Name)
	}
	// 三张派生卡：受管（yzagmgr）/ 官方 / 请求映射（RouterForm，UC-024）。
	if len(names) != 3 || names[0] != "yzagmgr" || names[1] != "官方" || names[2] != "请求映射" {
		t.Errorf("预热后卡片名 = %v，期望 [yzagmgr 官方 请求映射]", names)
	}
}

// TestWarmPolicyStaysQuietWhenPolicyFileMissing 守住「读不到就不编造」：
// 策略文件缺席时预热必须静默——不写盘、不报错、不凭空得到策略。
func TestWarmPolicyStaysQuietWhenPolicyFileMissing(t *testing.T) {
	srv := newContractServer(t)
	freshPolicyRoot(t, srv)

	srv.warmPolicy()

	if p, err := srv.Policy.Load(); err != nil {
		t.Errorf("「一份策略都没见过」是一种状态而不是错误: %v", err)
	} else if p != nil {
		t.Errorf("文件缺席时不该凭空得到策略: %+v", p)
	}
	if _, err := os.Stat(srv.env.PolicyFile()); !os.IsNotExist(err) {
		t.Errorf("读取失败却写了策略文件（stat err=%v）", err)
	}
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/claude/profiles", "", true)
	if res.ok || !strings.Contains(res.body, "模型策略") {
		t.Errorf("缺席时应保留诚实的策略缺失错误，实际: %s", res.body)
	}
}

// TestStartBackgroundWarmsPolicyWithoutBlocking 守住启动装配那一行：预热真的接在
// 启动路径上，且不挡就绪（源是本地盘读，恒不阻塞——断言仍走真实 StartBackground，
// 因为本产品已两次栽在「接线了但没人调」上）。
// 探测那半边用 nil 注册表摘掉——`installProbe.start()` 见 nil 即直接返回。
func TestStartBackgroundWarmsPolicyWithoutBlocking(t *testing.T) {
	srv := newContractServer(t)
	freshPolicyRoot(t, srv)
	writePolicyFile(t, srv, warmPolicyData)
	srv.probe = newInstallProbe(nil, time.Hour)
	t.Cleanup(srv.StopBackground)

	returned := make(chan struct{})
	go func() {
		srv.StartBackground()
		close(returned)
	}()

	select {
	case <-returned:
		// 启动路径立即返回——预热是异步任务，不拖就绪。
	case <-time.After(2 * time.Second):
		t.Fatal("StartBackground 没有交出控制权：预热被写成同步阻塞的了")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		// Load 与 Refresh 共用同一把锁，因此「读到非 nil」意味着装载已经完成，
		// 不会与临时目录清理赛跑。
		if p, err := srv.Policy.Load(); err == nil && p != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("启动路径交出控制权后策略仍未异步落地")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
