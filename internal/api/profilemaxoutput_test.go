// UC-040 自建卡能力声明 `maxOutputTokens` 的断言：
//
//   - A1 字段闭环：创建/修改/回显/持久化四步一致；越界（0、负值、>1_000_000）被拒且文案含范围；
//   - A2 装配逐字：卡声明 131072 ⇒ pi 计划里 `maxTokens=131072`（不被夹到 384000）；
//     未声明 ⇒ 与既有用例逐字一致（零回归）。
//
// 夹具全部写在隔离数据根（t.TempDir）下，不触碰任何真实工具配置。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// createCardRaw 建一张自建卡并回显信封（不要求成功——负向用例要读失败响应）。
func createCardRaw(t *testing.T, srv *contractServer, tool, body string) envelope {
	t.Helper()
	return srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/profiles", body, true)
}

// userCardOf 从卡片列表里取出现的第一张自建卡。
func userCardOf(t *testing.T, srv *contractServer, tool string) Profile {
	t.Helper()
	list := profilesOf(t, srv, tool)
	for _, card := range list.Items {
		if card.Kind == ProfileKindUser {
			return card
		}
	}
	t.Fatalf("%s 没有自建卡: %+v", tool, list.Items)
	return Profile{}
}

// TestMaxOutputTokensRoundTrip 守住 A1 的四步闭环（含盘上持久化）。
func TestMaxOutputTokensRoundTrip(t *testing.T) {
	srv := newContractServer(t)

	created := createCardRaw(t, srv, string(family.Pi), `{"name":"glm 自建","maxOutputTokens":131072}`)
	if !created.ok {
		t.Fatalf("建卡失败: %s", created.body)
	}
	var card Profile
	if err := json.Unmarshal(created.data, &card); err != nil {
		t.Fatalf("建卡响应不是预期结构: %v（%s）", err, string(created.data))
	}
	if card.MaxOutputTokens != 131072 {
		t.Errorf("建卡回显 maxOutputTokens = %d（期望 131072）", card.MaxOutputTokens)
	}

	// 回显（列表）与持久化（盘上记账）。
	if got := userCardOf(t, srv, string(family.Pi)); got.MaxOutputTokens != 131072 {
		t.Errorf("列表回显 maxOutputTokens = %d（期望 131072）", got.MaxOutputTokens)
	}
	if got := persistedMaxOutputTokens(t, srv, string(family.Pi), card.ID); got != 131072 {
		t.Errorf("盘上持久化的 maxOutputTokens = %d（期望 131072）", got)
	}

	// 修改（改小/改大都走同一条校验）。
	updated := srv.callRaw(t, http.MethodPut,
		"/api/v1/tools/pi/profiles/"+card.ID, `{"maxOutputTokens":65536}`, true)
	if !updated.ok {
		t.Fatalf("改卡失败: %s", updated.body)
	}
	if got := userCardOf(t, srv, string(family.Pi)); got.MaxOutputTokens != 65536 {
		t.Errorf("改后回显 maxOutputTokens = %d（期望 65536）", got.MaxOutputTokens)
	}
	if got := persistedMaxOutputTokens(t, srv, string(family.Pi), card.ID); got != 65536 {
		t.Errorf("改后盘上 maxOutputTokens = %d（期望 65536）", got)
	}

	// 未声明：键**缺席**（不是 0）——「没声明」与「声明了 0」在线上必须是两件事。
	// 用**干净的数据根**做这一格：盘上记账是整份工具卡片列表，混着上面那张已声明的卡。
	t.Run("未声明 ⇒ 键缺席（响应与盘上都不出现）", func(t *testing.T) {
		fresh := newContractServer(t)
		plain := createCardRaw(t, fresh, string(family.Pi), `{"name":"未声明"}`)
		if !plain.ok {
			t.Fatalf("建未声明卡失败: %s", plain.body)
		}
		if strings.Contains(plain.body, "maxOutputTokens") {
			t.Errorf("未声明的卡在响应里出现了 maxOutputTokens 键: %s", plain.body)
		}
		if strings.Contains(string(persistedProfiles(t, fresh)), `"maxOutputTokens"`) {
			t.Errorf("未声明的卡在盘上记账里出现了 maxOutputTokens 键")
		}
	})
}

// TestMaxOutputTokensRangeIsEnforced 守住 A1 的越界拒绝与文案（含范围）。
func TestMaxOutputTokensRangeIsEnforced(t *testing.T) {
	srv := newContractServer(t)
	for _, value := range []string{"0", "-5", "1000001"} {
		t.Run("建卡 "+value, func(t *testing.T) {
			res := createCardRaw(t, srv, string(family.Pi), `{"name":"越界","maxOutputTokens":`+value+`}`)
			if res.ok || res.code != "invalid_request" {
				t.Fatalf("越界 %s 的响应 = ok:%v code:%q（期望 invalid_request）: %s",
					value, res.ok, res.code, res.body)
			}
			if !strings.Contains(res.message, "1..1000000") {
				t.Errorf("文案没有指出范围: %q", res.message)
			}
		})
		t.Run("改卡 "+value, func(t *testing.T) {
			pid := createUserCard(t, srv, family.Pi, "改卡越界"+value)
			res := srv.callRaw(t, http.MethodPut,
				"/api/v1/tools/pi/profiles/"+pid, `{"maxOutputTokens":`+value+`}`, true)
			if value == "0" {
				// UC-042：改卡 0 = **清除声明**（不是越界）。
				if !res.ok {
					t.Fatalf("改卡 0（清除声明）被拒: %s", res.body)
				}
				return
			}
			if res.ok || res.code != "invalid_request" {
				t.Fatalf("越界 %s 的改卡响应 = ok:%v code:%q（期望 invalid_request）: %s",
					value, res.ok, res.code, res.body)
			}
			if !strings.Contains(res.message, "0..1000000") {
				t.Errorf("文案没有指出范围: %q", res.message)
			}
		})
	}
	// 区间两端合法（1 与 1_000_000 都在区间内）。
	for _, ok := range []string{"1", "1000000"} {
		res := createCardRaw(t, srv, string(family.Pi), `{"name":"边界`+ok+`","maxOutputTokens":`+ok+`}`)
		if !res.ok {
			t.Errorf("边界值 %s 被拒: %s", ok, res.body)
		}
	}
}

// TestMaxOutputTokensIsAdoptedVerbatimInPiPlan 守住 A2：声明值逐字采用、未声明零回归。
func TestMaxOutputTokensIsAdoptedVerbatimInPiPlan(t *testing.T) {
	srv := newContractServer(t)

	t.Run("声明 131072 ⇒ maxTokens=131072（不被夹到 384000）", func(t *testing.T) {
		pid := createCardWithBody(t, srv, family.Pi, `{"name":"glm 自建","maxOutputTokens":131072}`)
		got := plannedPiMaxTokens(t, srv, pid)
		if got != 131072 {
			t.Fatalf("计划的 maxTokens = %d（期望 131072；被夹到 %d 就会在上游 400）", got, got)
		}
	})
	t.Run("未声明 ⇒ 现行行为逐字不变（384000）", func(t *testing.T) {
		pid := createUserCard(t, srv, family.Pi, "未声明")
		if got := plannedPiMaxTokens(t, srv, pid); got != 384000 {
			t.Fatalf("未声明卡的计划 maxTokens = %d（期望现行行为 384000）", got)
		}
	})
	t.Run("受管卡不受影响（384000）", func(t *testing.T) {
		if got := plannedPiMaxTokens(t, srv, managedCardID); got != 384000 {
			t.Fatalf("受管卡的计划 maxTokens = %d（期望 384000）", got)
		}
	})
	t.Run("复制跟随声明值", func(t *testing.T) {
		pid := createCardWithBody(t, srv, family.Pi, `{"name":"源卡","maxOutputTokens":131072}`)
		res := srv.callRaw(t, http.MethodPost,
			"/api/v1/tools/pi/profiles/"+pid+"/duplicate", "", true)
		if !res.ok {
			t.Fatalf("复制失败: %s", res.body)
		}
		var copyCard Profile
		if err := json.Unmarshal(res.data, &copyCard); err != nil {
			t.Fatalf("复制响应不是预期结构: %v", err)
		}
		if copyCard.MaxOutputTokens != 131072 {
			t.Errorf("副本的 maxOutputTokens = %d（期望跟随源卡的 131072）", copyCard.MaxOutputTokens)
		}
	})
}

// TestMaxOutputTokensOnCardWithoutTheFamilyKeyIsInert 守住 UC-040 的边界：
// 家族没有该键时字段照样存得住，但计划里不会凭空多出一个键（不伪造）。
func TestMaxOutputTokensOnCardWithoutTheFamilyKeyIsInert(t *testing.T) {
	srv := newContractServer(t)
	pid := createCardWithBody(t, srv, family.Claude, `{"name":"claude 自建","maxOutputTokens":131072}`)
	plans, err := srv.planForProfile(t.Context(), family.Claude, pid, "")
	if err != nil {
		t.Fatalf("计划计算失败: %v", err)
	}
	for _, plan := range plans {
		if strings.Contains(string(plan.After), "maxTokens") {
			t.Errorf("claude 的计划里不该有 pi 的键 maxTokens: %s", plan.After)
		}
	}
	if got := userCardOf(t, srv, string(family.Claude)); got.MaxOutputTokens != 131072 {
		t.Errorf("claude 自建卡的能力位没有被保存: %d", got.MaxOutputTokens)
	}
}

// ── 夹具工具 ────────────────────────────────────────────────────────────────

// createCardWithBody 建一张自建卡（完整请求体）并返回卡 id。
func createCardWithBody(t *testing.T, srv *contractServer, tool family.ID, body string) string {
	t.Helper()
	res := createCardRaw(t, srv, string(tool), body)
	if !res.ok {
		t.Fatalf("%s 建卡失败（%s）: %s", tool, body, res.body)
	}
	var card Profile
	if err := json.Unmarshal(res.data, &card); err != nil || card.ID == "" {
		t.Fatalf("建卡响应里没有 id: %v（%s）", err, string(res.data))
	}
	return card.ID
}

// persistedProfiles 读盘上的档位卡记账原文。
func persistedProfiles(t *testing.T, srv *contractServer) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(srv.env.ManagedDir(), "profiles.json"))
	if err != nil {
		t.Fatalf("读取档位卡记账失败: %v", err)
	}
	return raw
}

// persistedMaxOutputTokens 读某张卡在盘上记账里的能力位。
func persistedMaxOutputTokens(t *testing.T, srv *contractServer, tool, pid string) int {
	t.Helper()
	var doc struct {
		Tools map[string]struct {
			User []struct {
				ID              string `json:"id"`
				MaxOutputTokens int    `json:"maxOutputTokens"`
			} `json:"user"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(persistedProfiles(t, srv), &doc); err != nil {
		t.Fatalf("解析盘上记账失败: %v", err)
	}
	for _, card := range doc.Tools[tool].User {
		if card.ID == pid {
			return card.MaxOutputTokens
		}
	}
	t.Fatalf("盘上记账里没有卡 %s", pid)
	return 0
}

// plannedPiMaxTokens 从 pi 的计划字节里读出写入的 maxTokens。
func plannedPiMaxTokens(t *testing.T, srv *contractServer, pid string) int {
	t.Helper()
	plans, err := srv.planForProfile(t.Context(), family.Pi, pid, "")
	if err != nil {
		t.Fatalf("计算 pi 计划失败: %v", err)
	}
	for _, plan := range plans {
		if !strings.HasSuffix(plan.Path, "models.json") {
			continue
		}
		var doc struct {
			Providers map[string]struct {
				Models []struct {
					MaxTokens int `json:"maxTokens"`
				} `json:"models"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(plan.After, &doc); err != nil {
			t.Fatalf("计划里的 models.json 不是合法 JSON: %v", err)
		}
		for _, provider := range doc.Providers {
			if len(provider.Models) > 0 {
				return provider.Models[0].MaxTokens
			}
		}
	}
	t.Fatalf("pi 的计划里没有带 models 的 provider: %+v", plans)
	return 0
}
