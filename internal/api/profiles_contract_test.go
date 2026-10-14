// 档位卡的契约断言：受管卡唯一且幂等（UC-018 A2）、生效标记以盘上字节为准（UC-018 A7）、
// 卡片内容不含明文令牌（UC-018 A12）。
//
// 三条都属于「实现里写着、但没有任何载体」的那一类：`ensureManaged` 的幂等分支、
// `toolState` 的 drift 判定与 `tokenRefFor()` 的引用形态此前都没有断言。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claude"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// profilesOf 读某工具的卡片列表。
func profilesOf(t *testing.T, srv *contractServer, tool string) profileListBody {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+tool+"/profiles", "", true)
	if !res.ok {
		t.Fatalf("读 %s 的卡片列表失败: %s", tool, res.body)
	}
	var payload profileListBody
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("卡片列表不是预期结构: %v（%s）", err, string(res.data))
	}
	return payload
}

// toolViewOf 读某工具视图（盘上字节判定的档位 + 生效档位卡）。
func toolViewOf(t *testing.T, srv *contractServer, tool string) toolView {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+tool, "", true)
	if !res.ok {
		t.Fatalf("读 %s 的工具视图失败: %s", tool, res.body)
	}
	var view toolView
	if err := json.Unmarshal(res.data, &view); err != nil {
		t.Fatalf("工具视图不是预期结构: %v（%s）", err, string(res.data))
	}
	return view
}

// countKind 数某类卡片。
func countKind(payload profileListBody, kind string) int {
	n := 0
	for _, card := range payload.Items {
		if card.Kind == kind {
			n++
		}
	}
	return n
}

// ── UC-018 A2 ──────────────────────────────────────────────────────────────

// TestManagedProfileIsUniqueAndIdempotent 守住 UC-018 A2：`ensure-gateway-profiles` 之后
// 每个受管单元**恰有**一张受管卡；重复调用幂等、不产生第二张。
//
// 受管卡是**派生**的（由策略与家族能力算出，不是存储的），这条断言守的是派生逻辑没有
// 顺手落成存储——一旦落成存储，第二次调用就会多出一张同 id 的卡，而界面上会出现两张「yzagmgr」。
func TestManagedProfileIsUniqueAndIdempotent(t *testing.T) {
	srv := newContractServer(t)

	// ① 每个受管单元恰有一张受管卡（受管卡由策略与家族能力**派生**，不落存储；
	// ensure-gateway-profiles 端点已随 §4.1 删除——design §2.2，派生语义不变）。
	for _, id := range family.Nines {
		list := profilesOf(t, srv, string(id))
		if got := countKind(list, ProfileKindManaged); got != 1 {
			t.Errorf("%s 的受管卡数 = %d，期望 1: %+v", id, got, list.Items)
		}
		// 受管卡的 id 必须稳定（重复读取不许换 id，否则界面的「当前生效」会跟丢）。
		if list.Items[0].ID != managedCardID || list.Items[0].Kind != ProfileKindManaged {
			t.Errorf("%s 的首张卡 = %+v，期望 %s 且在首位", id, list.Items[0], managedCardID)
		}
	}

	// ② 再读两次：卡片数不变、id 不变，且**记账文件不该被重写**——
	// 「幂等」在盘上的可判定形式就是「这次没有产生写入」。
	storeFile := filepath.Join(srv.env.ManagedDir(), "profiles.json")
	beforeStore := snapshotTree(t, storeFile)
	for round := 0; round < 2; round++ {
		if list := profilesOf(t, srv, "claude"); countKind(list, ProfileKindManaged) != 1 {
			t.Errorf("第 %d 次重复读取后受管卡数 = %d，期望 1", round+2, countKind(list, ProfileKindManaged))
		}
	}
	assertTreeUnchanged(t, "重复读取后的记账文件", beforeStore, snapshotTree(t, storeFile))

	// ③ 盘上记账只留一条 applied 口径：不许出现同一受管卡的重复条目。
	// （受管卡是纯派生视图：只读列表连记账文件都不该创建——缺席即最强断言。）
	raw, err := os.ReadFile(filepath.Join(srv.env.ManagedDir(), "profiles.json"))
	if os.IsNotExist(err) {
		return // 没有任何写入：派生语义成立
	}
	if err != nil {
		t.Fatalf("读档位卡记账失败: %v", err)
	}
	var doc profileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("档位卡记账不是合法 JSON: %v", err)
	}
	for tool, entry := range doc.Tools {
		for _, card := range entry.User {
			if strings.HasPrefix(card.ID, ManagedProfilePrefix) {
				t.Errorf("%s 的记账里出现了受管卡（受管卡是派生的，不该被存下来）: %+v", tool, card)
			}
		}
	}
}

// ── UC-018 A7 ──────────────────────────────────────────────────────────────

// TestActiveCardComesFromDiskNotBookkeeping 守住 UC-018 A7：卡片列表的「当前生效」
// 由**盘上字节**判定；与内部记账不一致时显式报出「漂移」。
//
// 两个方向都要试，因为它们是两种不同的说谎方式：
//   - 记账说已应用、盘上却不是那个形态 ⇒ 若信记账，界面会把「已回滚的外部改动」显示成生效；
//   - 记账为空、盘上却是外部写入的网关形态 ⇒ fail-safe 报 drift（旧 externalManaged
//     认领面已删——design §2.2；空记账仍**不属于**「不一致」，但未知外部写入不放松）。
func TestActiveCardComesFromDiskNotBookkeeping(t *testing.T) {
	srv := newContractServer(t)
	live := srv.env.ClaudeSettings()

	t.Run("记账与盘上一致时生效项来自盘上", func(t *testing.T) {
		applyCard(t, srv, family.Claude, managedCardID)
		view := toolViewOf(t, srv, "claude")
		if view.Tier != string(family.TierGateway) || view.ActiveProfileID != managedCardID {
			t.Fatalf("应用后 = tier=%q active=%q，期望 gateway/%s", view.Tier, view.ActiveProfileID, managedCardID)
		}
		list := profilesOf(t, srv, "claude")
		if list.ActiveProfileID != managedCardID {
			t.Errorf("列表的 activeProfileId = %q，期望 %q", list.ActiveProfileID, managedCardID)
		}
		for _, card := range list.Items {
			if card.Active != (card.ID == managedCardID) {
				t.Errorf("卡片 %s 的 active=%v 与生效项 %q 不一致", card.ID, card.Active, managedCardID)
			}
		}
	})

	t.Run("盘上被改回官方形态 ⇒ 漂移且不报生效项", func(t *testing.T) {
		// 外部把 live 文件改回官方形态（官方种子的形态就是 `{"env":{}}`）。
		if err := os.WriteFile(live, []byte("{\n  \"env\": {}\n}\n"), 0o600); err != nil {
			t.Fatalf("外部改动 live 配置失败: %v", err)
		}
		view := toolViewOf(t, srv, "claude")
		if view.Tier != string(family.TierDrift) {
			t.Errorf("盘上与记账不一致时 tier = %q，期望 drift", view.Tier)
		}
		if view.ActiveProfileID != "" {
			t.Errorf("漂移时必须清空 activeProfileId，实际 %q（信记账就会骗人）", view.ActiveProfileID)
		}
		list := profilesOf(t, srv, "claude")
		if list.ActiveProfileID != "" {
			t.Errorf("列表的 activeProfileId = %q，期望空串", list.ActiveProfileID)
		}
		for _, card := range list.Items {
			if card.Active {
				t.Errorf("漂移时不该有任何卡被标成生效: %+v", card)
			}
		}
	})

	t.Run("盘上改回网关形态即恢复（记账未变）", func(t *testing.T) {
		// 重新应用一次官方档再应用网关档：盘上回到我们的形态，生效项自动恢复。
		applyCard(t, srv, family.Claude, OfficialProfileID)
		applyCard(t, srv, family.Claude, managedCardID)
		view := toolViewOf(t, srv, "claude")
		if view.Tier != string(family.TierGateway) || view.ActiveProfileID != managedCardID {
			t.Errorf("盘上回到网关形态后 = tier=%q active=%q，期望 gateway/%s",
				view.Tier, view.ActiveProfileID, managedCardID)
		}
	})

	t.Run("无记账但盘上是外部写入的网关形态 ⇒ fail-safe 报 drift", func(t *testing.T) {
		fresh := newContractServer(t)
		settings := fresh.env.ClaudeSettings()
		if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
			t.Fatalf("建立 live 目录失败: %v", err)
		}
		// 直接把 live 写成网关档（本地 route 形态）——本产品从没写过它。
		body := `{"env":{"ANTHROPIC_BASE_URL":"` + fresh.localRouteBase() + `/claude"}}`
		if err := os.WriteFile(settings, []byte(body), 0o600); err != nil {
			t.Fatalf("写入 live 配置失败: %v", err)
		}
		view := toolViewOf(t, fresh, "claude")
		// externalManaged 认领面已随 externalmanaged 删除（design §2.2）：
		// 空记账 + 未知外部网关写入 ⇒ fail-safe drift，active 为空。
		if view.Tier != string(family.TierDrift) {
			t.Errorf("无记账+外部网关形态时 tier = %q，期望 drift（fail-safe）", view.Tier)
		}
		if view.ActiveProfileID != "" {
			t.Errorf("无记账时 activeProfileId = %q，期望空串", view.ActiveProfileID)
		}
	})
}

// readFileOrFail 读一份必须存在的文件（保留给需要对照盘上原始字节的断言）。
func readFileOrFail(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return raw
}

// ── UC-018 A12 ─────────────────────────────────────────────────────────────

// TestProfilePayloadUsesTokenReferenceNotPlaintext 守住 UC-018 A12：卡片内容不含明文令牌，
// 凭据以**引用**形式表达。
//
// 判据分两块：
//   - 响应侧：任何卡片载荷都不含 `sk-` 形态，也不含装配期种下的真令牌；
//   - 内部侧：喂给家族写链的凭据是引用串（`yzagmgr-managed-token://<uid>`），
//     受管卡 id / 托管 provider id 都从 uid 派生（`yzagmgr-<uid>`），与引用串同源。
func TestProfilePayloadUsesTokenReferenceNotPlaintext(t *testing.T) {
	const seedToken = "sk-live-seed-0123456789abcdefghijklmnop"
	srv := newContractServer(t)
	deps := srv.Deps
	deps.TokenSeed = seedToken
	managed, err := New(deps)
	if err != nil {
		t.Fatalf("装配（带真令牌种子）失败: %v", err)
	}
	srv = &contractServer{Server: managed, env: srv.env, token: srv.token, upstream: srv.upstream}

	// 走一遍完整通道：预览 → 应用（真令牌在这条链上真的会被取用；
	// 建档响应条目随 ensure-gateway-profiles 删除——design §2.2）。
	applyCard(t, srv, family.Claude, managedCardID)

	bodies := map[string]string{}
	for _, call := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"卡片列表", http.MethodGet, "/api/v1/tools/claude/profiles", ""},
		{"工具视图", http.MethodGet, "/api/v1/tools/claude", ""},
	} {
		res := srv.callRaw(t, call.method, call.path, call.body, true)
		if !res.ok {
			t.Fatalf("%s 失败: %s", call.name, res.body)
		}
		bodies[call.name] = res.body
	}
	// 新建一张自建卡也是卡片载荷的一种。
	create := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/profiles", `{"name":"仅引用卡"}`, true)
	if !create.ok {
		t.Fatalf("建卡失败: %s", create.body)
	}
	bodies["建卡响应"] = create.body

	for name, body := range bodies {
		if strings.Contains(body, seedToken) {
			t.Errorf("%s 带上了装配期令牌明文", name)
		}
		if match := secretShape.FindString(body); match != "" {
			t.Errorf("%s 出现令牌形态 %q", name, match)
		}
	}

	// 卡片本身只有可编辑键（`modelId`），没有任何凭据位——这是「以引用表达」的另一半：
	// 载荷里根本没有放明文令牌的地方。
	list := profilesOf(t, srv, "claude")
	for _, card := range list.Items {
		for key := range card.Settings {
			if !isEditableSetting(key) {
				t.Errorf("卡片 %s 的 settings 出现非可编辑键 %q", card.ID, key)
			}
		}
		if card.Kind == ProfileKindManaged {
			if _, ok := card.Settings["modelId"]; !ok {
				t.Errorf("受管卡的 settings 缺 modelId: %+v", card.Settings)
			}
		}
	}

	// 引用串的形态：`yzagmgr-managed-token://<uid>`，且受管卡 / 托管 provider id 与它同源。
	if got, want := tokenRefFor(anonymousUID), managedTokenScheme+anonymousUID; got != want {
		t.Errorf("tokenRefFor(%q) = %q，期望 %q", anonymousUID, got, want)
	}
	if got, want := managedProfileIDFor("u-7"), managedProviderPrefix+"u-7"; got != want {
		t.Errorf("managedProfileIDFor(u-7) = %q，期望 %q", got, want)
	}
	// live 配置里落的是引用派生的形态，不是令牌（claude 的凭据位是占位符）。
	raw := readFileOrFail(t, srv.env.ClaudeSettings())
	if strings.Contains(string(raw), seedToken) {
		t.Errorf("live 配置里落了明文令牌")
	}
	// claude 的网关档是**本地 route** 形态：凭据位是占位符，基址指向本进程的回环。
	if !strings.Contains(string(raw), srv.localRouteBase()) {
		t.Errorf("live 配置里没有本地 route 基址（%s）: %s", srv.localRouteBase(), raw)
	}
	if !strings.Contains(string(raw), claude.TokenPlaceholder) {
		t.Errorf("live 配置里没有占位符凭据（%s）: %s", claude.TokenPlaceholder, raw)
	}
}
