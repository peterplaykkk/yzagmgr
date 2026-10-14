// 安装三态与「配置形态不得顶替安装状态」的 API 语义（UC-003 修订「状态拆分」/
// UC-091 A2/A3，2026-09-27）。
//
// 背景：修订前界面用「未安装」顶替配置形态（toolStatus 先看 installed 再看 tier），
// 配套的 API 侧根因是 `installed` 布尔把两件事混在一起（受管配置文件在场即计安装）。
// 本文件钉住修订后的语义：`tier`（盘上配置形态）与 `installedState`（安装三态）
// 是两个独立维度、谁都如实报；旧 `installed` 布尔保留原口径以零回归（与
// installedState 并排出现时以 installedState 为准，见 doc/api-v1.md §4.1）。
//
// 夹具纪律：安装探测缓存被**显式钉死**（stubInstallProbe）——测试机上可能真的装着
// 这些工具，不钉死就会机器相关。受管配置文件一律写在 t.TempDir() 隔离根下。
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// stubInstallProbe 把安装探测缓存替换为确定性状态（不执行任何子进程）。
func stubInstallProbe(srv *contractServer, id family.ID, st probeState) {
	srv.probe.mu.Lock()
	defer srv.probe.mu.Unlock()
	srv.probe.state[id] = st
}

// TestInstalledStateDoesNotReplaceConfigForm 守住「未安装不得顶替配置形态」：
// 工具未安装（无任何安装证据）但受管配置在场时——
//   - `tier` 照常报盘上配置形态（official），配置判定不被「未安装」吞掉；
//   - `installedState` 如实报 not_installed（配置文件在场**不是**安装证据，不得反过来
//     把「未安装」顶替成「已安装」）；
//   - 旧 `installed` 布尔逐字保留原口径（路径在场即 true）——零回归锚。
func TestInstalledStateDoesNotReplaceConfigForm(t *testing.T) {
	srv := newContractServer(t)
	writeClaudeLive(t, srv, `{"env":{}}`) // 官方形态的受管配置在场
	stubInstallProbe(srv, family.Claude, probeState{})

	view := toolViewOf(t, srv, "claude")
	if view.Tier != string(family.TierOfficial) {
		t.Errorf("配置形态被顶替：tier = %q，期望 official（配置在场照常判定）", view.Tier)
	}
	if view.InstalledState != installStateNotInstalled {
		t.Errorf("installedState = %q，期望 not_installed（配置文件在场不是安装证据）", view.InstalledState)
	}
	if !view.Installed {
		t.Errorf("旧 installed 布尔语义被改动：路径在场时应为 true（零回归）")
	}
}

// TestInstalledStateEvidenceWins 安装证据优先：可执行副本 / 版本可读 ⇒ installed，
// 与配置形态各自独立（tier=absent 时照样报 installed）。
func TestInstalledStateEvidenceWins(t *testing.T) {
	srv := newContractServer(t)
	// 只有版本证据（无副本）：仍是 installed（版本读得到就说明装着）。
	stubInstallProbe(srv, family.Claude, probeState{version: "9.9.9"})

	view := toolViewOf(t, srv, "claude")
	if view.InstalledState != installStateInstalled {
		t.Errorf("installedState = %q，期望 installed（版本证据在场）", view.InstalledState)
	}
	if view.Tier != string(family.TierAbsent) {
		t.Errorf("两维度互相独立：配置不在场时 tier 应仍为 absent，实际 %q", view.Tier)
	}
	if view.Version != "9.9.9" {
		t.Errorf("version = %q，期望 9.9.9", view.Version)
	}
}

// TestInstalledStateBothAbsentIsNotInstalled 两无（无配置、无安装证据、探测成功）⇒
// not_installed：这才满足「明确缺少可执行工具才显示未安装」。
func TestInstalledStateBothAbsentIsNotInstalled(t *testing.T) {
	srv := newContractServer(t)
	stubInstallProbe(srv, family.Claude, probeState{})

	view := toolViewOf(t, srv, "claude")
	if view.Tier != string(family.TierAbsent) || view.InstalledState != installStateNotInstalled {
		t.Errorf("(tier, installedState) = (%q, %q)，期望 (absent, not_installed)", view.Tier, view.InstalledState)
	}
}

func TestInstalledStateTreatsUnprobedCacheAsUnknown(t *testing.T) {
	t.Run("unprobed cache is unknown", func(t *testing.T) {
		srv := newContractServer(t)
		srv.probe.mu.Lock()
		delete(srv.probe.state, family.Claude)
		srv.probe.mu.Unlock()

		view := toolViewOf(t, srv, "claude")
		if view.Tier != string(family.TierAbsent) {
			t.Fatalf("isolated Claude tier=%q, want absent to isolate installation evidence", view.Tier)
		}
		if view.InstalledState != installStateUnknown {
			t.Errorf("unprobed installation cache reported %q, want unknown", view.InstalledState)
		}
	})

	t.Run("successful empty probe is not installed", func(t *testing.T) {
		srv := newContractServer(t)
		stubInstallProbe(srv, family.Claude, probeState{copies: []installCopy{}})

		view := toolViewOf(t, srv, "claude")
		if view.InstalledState != installStateNotInstalled || view.InstallCopies != 0 || view.Version != "" {
			t.Errorf("completed empty install probe = state:%q copies:%d version:%q, want not_installed/0/empty",
				view.InstalledState, view.InstallCopies, view.Version)
		}
	})

	t.Run("copy is installation evidence even when version is empty", func(t *testing.T) {
		srv := newContractServer(t)
		stubInstallProbe(srv, family.Claude, probeState{
			copies: []installCopy{{Path: "/synthetic/bin/claude", Active: true}},
		})

		view := toolViewOf(t, srv, "claude")
		if view.InstalledState != installStateInstalled || view.InstallCopies != 1 || view.Version != "" {
			t.Errorf("copy without readable version = state:%q copies:%d version:%q, want installed/1/empty",
				view.InstalledState, view.InstallCopies, view.Version)
		}
	})
}

// TestInstalledStateUnknownOnProbeFailure 探测失败 ⇒ tier 与 installedState 双 unknown；
// 而**安装证据**独立于档位探测：副本在场时安装状态仍报 installed（不因配置探测失败改口）。
func TestInstalledStateUnknownOnProbeFailure(t *testing.T) {
	t.Run("无证据 ⇒ 双 unknown", func(t *testing.T) {
		srv := stubServer(t, probeErrorFamily{})
		stubInstallProbe(srv, family.Claude, probeState{})
		view := toolViewOf(t, srv, "claude")
		if view.Tier != string(family.TierUnknown) || view.InstalledState != installStateUnknown {
			t.Errorf("(tier, installedState) = (%q, %q)，期望 (unknown, unknown)", view.Tier, view.InstalledState)
		}
	})

	t.Run("副本在场 ⇒ installed（安装证据独立）", func(t *testing.T) {
		srv := stubServer(t, probeErrorFamily{})
		stubInstallProbe(srv, family.Claude, probeState{
			copies: []installCopy{{Path: "/opt/homebrew/bin/claude", Version: "9.9.9", Active: true}},
		})
		view := toolViewOf(t, srv, "claude")
		if view.Tier != string(family.TierUnknown) {
			t.Errorf("tier = %q，期望 unknown", view.Tier)
		}
		if view.InstalledState != installStateInstalled {
			t.Errorf("installedState = %q，期望 installed（副本在场是安装证据）", view.InstalledState)
		}
	})
}

// TestToolViewCarriesInstalledStateKey 契约面：响应里 `installedState` 键恒出现且取值在闭集内
// （列表与单条同构）。
func TestToolViewCarriesInstalledStateKey(t *testing.T) {
	srv := newContractServer(t)
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools", "", true)
	if !res.ok {
		t.Fatalf("GET /tools 失败: %s", res.body)
	}
	views := toolViewsOf(t, res)
	if len(views) == 0 {
		t.Fatalf("GET /tools 返回空列表（夹具失去判别力）")
	}
	for _, view := range views {
		switch view.InstalledState {
		case installStateInstalled, installStateNotInstalled, installStateUnknown:
		default:
			t.Errorf("@%s installedState = %q，不在闭集 {installed, not_installed, unknown} 内",
				view.ID, view.InstalledState)
		}
	}
	// 键恒出现（omitempty 会让旧客户端把「没这个键」误当不存在）：原始正文里必须有该键。
	if !strings.Contains(string(res.data), `"installedState"`) {
		t.Errorf("GET /tools 的正文里缺 installedState 键: %s", res.data)
	}
}

// toolViewsOf 解析 GET /tools 的列表载荷。
func toolViewsOf(t *testing.T, res envelope) []toolView {
	t.Helper()
	var views []toolView
	if err := json.Unmarshal(res.data, &views); err != nil {
		t.Fatalf("工具列表不是预期结构: %v（%s）", err, res.data)
	}
	return views
}
