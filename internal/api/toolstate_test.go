// toolState 空记账分支的契约（2026-09-24 细化 + 本轮 externalmanaged 删除后修订，
// 契约 §4.1 的 tier 语义行）。
//
// 语义：「记账与盘上不一致」以**有记账**为前提——空记账不属于不一致，按盘上真值报档位
// （absent/official 照实）；空记账 + 网关形态 = 未知外部写入 ⇒ drift（fail-safe）——
// 旧「外部托管认领」面已随 externalmanaged 删除（design §2.2）。夹具全部写在
// 隔离数据根（t.TempDir）下，不触碰任何真实工具配置。
package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// writeClaudeLive 在隔离数据根下写一份 claude live 夹具（错误即中止）。
func writeClaudeLive(t *testing.T, srv *contractServer, body string) {
	t.Helper()
	settings := srv.env.ClaudeSettings()
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatalf("建立 live 目录失败: %v", err)
	}
	if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
		t.Fatalf("写入隔离 live 夹具失败: %v", err)
	}
}

// TestToolStateEmptyBookkeepingStates 守住空记账分支的判定：
//  1. 空盘 ⇒ absent（不变）；
//  2. 空记账 + 官方形态 ⇒ official（工具原生状态，不是漂移）；
//  3. 空记账 + 网关形态（任意回环） ⇒ drift（未知外部写入的 fail-safe，不放松；
//     旧 externalManaged 认领分支已注销——design §2.2）。
func TestToolStateEmptyBookkeepingStates(t *testing.T) {
	t.Run("空盘 ⇒ absent", func(t *testing.T) {
		srv := newContractServer(t)
		tier, active, _, err := srv.toolState(family.Claude)
		if err != nil {
			t.Fatalf("toolState 报错: %v", err)
		}
		if tier != family.TierAbsent || active != "" {
			t.Fatalf("空盘 = tier=%q active=%q，期望 absent/空串", tier, active)
		}
	})

	t.Run("空记账+官方形态 ⇒ official（原生状态，不是漂移）", func(t *testing.T) {
		srv := newContractServer(t)
		// 官方种子形态就是键不存在的 `{"env":{}}`（§A 行 1）。
		writeClaudeLive(t, srv, `{"env":{}}`)
		tier, active, _, err := srv.toolState(family.Claude)
		if err != nil {
			t.Fatalf("toolState 报错: %v", err)
		}
		if tier != family.TierOfficial {
			t.Fatalf("空记账+官方形态 tier = %q，期望 official（原生状态，不是漂移）", tier)
		}
		if active != "" {
			t.Fatalf("空记账时 activeProfileId = %q，期望空串", active)
		}
	})

	t.Run("空记账+网关形态 ⇒ drift（fail-safe 保持）", func(t *testing.T) {
		srv := newContractServer(t)
		// 任意回环网关形态 + 空记账 = 未知外部写入（externalManaged 认领面已删）：
		// probe 判网关档（claude 的判定认任意回环），toolState 必须保持 drift。
		writeClaudeLive(t, srv, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8080/claude"}}`)
		tier, active, _, err := srv.toolState(family.Claude)
		if err != nil {
			t.Fatalf("toolState 报错: %v", err)
		}
		if tier != family.TierDrift {
			t.Fatalf("空记账+网关形态 tier = %q，期望 drift（fail-safe 不放松）", tier)
		}
		if active != "" {
			t.Fatalf("漂移时 activeProfileId = %q，期望空串", active)
		}
	})

}

// probeErrorFamily 是 Probe 必报错的替身（toolState 的 Probe 错误路径在真实家族上
// 不可构造：十族对缺席/非法文件一律不报错，只记 Notes）。
type probeErrorFamily struct{ emptyPathsFamily }

func (probeErrorFamily) Probe(*config.Env) (family.ProbeResult, error) {
	return family.ProbeResult{}, errors.New("probe 故意坏掉")
}

// TestToolStateErrorPaths 守住 toolState 的两条错误路径：未注册单元 ⇒ not_supported；
// Probe 报错 ⇒ **TierUnknown**（UC-003 修订「状态拆分」，2026-09-27）。
//
// 修订前这里断言「Probe 报错原样上抛」——那保证了不误判成 absent/official，但把
// 「探测失败」表达成了一个错误响应；修订后档位词汇里有了为它准备的显式答案
// `unknown`（family.TierUnknown），真正要守的不变量因此变成：**既不吞成确定档位、
// 也不把不确定性塞回错误码**，且在 GET /tools 面上双字段（tier/installedState）同报 unknown。
func TestToolStateErrorPaths(t *testing.T) {
	t.Run("未注册单元 ⇒ not_supported", func(t *testing.T) {
		srv := newContractServer(t)
		_, _, _, err := srv.toolState(family.ID("no-such-unit"))
		if err == nil || !strings.Contains(err.Error(), "no-such-unit") {
			t.Fatalf("未注册单元的 toolState 错误 = %v，期望含 not_supported 与 id", err)
		}
	})

	t.Run("Probe 报错 ⇒ unknown（探测失败不是盘上事实，也不是错误响应）", func(t *testing.T) {
		srv := stubServer(t, probeErrorFamily{})
		tier, active, probe, err := srv.toolState(family.Claude)
		if err != nil {
			t.Fatalf("Probe 报错不得上抛成错误（unknown 是显式判定值）: %v", err)
		}
		if tier != family.TierUnknown {
			t.Fatalf("Probe 报错 tier = %q，期望 unknown（不得误判成 absent/official/drift）", tier)
		}
		if active != "" {
			t.Fatalf("unknown 时 activeProfileId = %q，期望空串", active)
		}
		// 失败原因留在 Notes（诊断面），不进 API 响应正文。
		if len(probe.Notes) == 0 {
			t.Errorf("探测失败没有留下 Notes（诊断线索不应被丢弃）")
		}
		// GET 面双字段同报：tier=unknown + installedState=unknown。
		view := toolViewOf(t, srv, "claude")
		if view.Tier != string(family.TierUnknown) {
			t.Errorf("GET /tools/claude 的 tier = %q，期望 unknown", view.Tier)
		}
		if view.InstalledState != installStateUnknown {
			t.Errorf("GET /tools/claude 的 installedState = %q，期望 unknown", view.InstalledState)
		}
		// 列表端点同样不得整条失败（单族探测失败不拖垮列表）。
		res := srv.callRaw(t, http.MethodGet, "/api/v1/tools", "", true)
		if !res.ok {
			t.Fatalf("探测失败的家族不得拖垮 GET /tools: %s", res.body)
		}
		if !strings.Contains(string(res.data), `"tier":"unknown"`) {
			t.Errorf("列表里探测失败的家族应呈现 tier=unknown: %s", res.data)
		}
	})
}

// TestToolStateSelfBuiltEndpointCardNotDrift 钉 UC-032 独立端点卡的档位语义（2026-09-25
// 由验证面登记、本轮修复）：自建卡声明独立端点 ⇒ probe 的受管词汇（只认回环）
// 认不出它、归入 official ⇒ wantsTier（gateway）不符、看起来是漂移；但盘上决定键指向卡上
// 端点时，「生效项 = 该自建卡」就是盘上事实（字节是本产品经 §4.3 事务写的）。
// 外改（决定键指向别处）照常报 drift——补偿只认自己写的字节。
func TestToolStateSelfBuiltEndpointCardNotDrift(t *testing.T) {
	srv := newContractServer(t)

	res := srv.callRaw(t, "POST", "/api/v1/tools/pi/profiles",
		`{"name":"自有端点","endpoint":"https://selfbuilt.example/v1"}`, true)
	if !res.ok {
		t.Fatalf("建自建卡失败: %s", res.body)
	}
	cardID := rawString(t, mustObject(t, res), "id")
	if err := srv.profiles.setApplied(string(family.Pi), cardID, "pv_test", time.Now()); err != nil {
		t.Fatalf("setApplied: %v", err)
	}

	models := srv.env.PiModels()
	if err := os.MkdirAll(filepath.Dir(models), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	write := func(baseURL string) {
		t.Helper()
		body := `{"providers":{"yzagmgr-u_seed":{"models":[{"baseUrl":"` + baseURL + `"}]}}}`
		if err := os.WriteFile(models, []byte(body), 0o600); err != nil {
			t.Fatalf("写盘上夹具失败: %v", err)
		}
	}

	// 正向：决定键指向卡上端点 ⇒ (gateway, 卡 ID)，不是漂移。
	write("https://selfbuilt.example/v1")
	tier, active, _, err := srv.toolState(family.Pi)
	if err != nil {
		t.Fatalf("toolState 报错: %v", err)
	}
	if tier != family.TierGateway || active != cardID {
		t.Fatalf("盘上指向卡上端点 = (tier=%q, active=%q)，期望 (gateway, %s)", tier, active, cardID)
	}

	// 负向：外改指向别处 ⇒ (drift, 空串)，补偿不认外改。
	write("https://hijacked.example/v1")
	tier, active, _, err = srv.toolState(family.Pi)
	if err != nil {
		t.Fatalf("toolState 报错: %v", err)
	}
	if tier != family.TierDrift || active != "" {
		t.Fatalf("外改后 = (tier=%q, active=%q)，期望 (drift, \"\")", tier, active)
	}
}
