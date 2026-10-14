// 开发构建的命名（UC-010 A11 的后半句）：「开发构建必须以 `-dev` 后缀命名」。
//
// 为什么这条要在 Go 侧钉住：正式版与开发版同名（`.app` 名或 bundle id）会让 LaunchServices
// 在两者之间互顶——装过开发版的人再装正式版，点开的可能是旧的那个；而 A11 前半句
// 「只保留一个正式版副本」的检测面也会因此失去意义（两个同名副本分不出来谁是正式版）。
//
// 断言字面量随 R236 唯一化改名重写（依据=UC-010 A12 修订，见 usecase/UC-010 A12）：
// 正式名 `yzagmgr.app` / `tech.yzbx.yzagmgr`，dev 名 `yzagmgr-dev.app` / `tech.yzbx.yzagmgr.dev`。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDevBuildCarriesDevSuffix 守住 UC-010 A11 的命名纪律：
//   - Go 侧默认版本串带 `-dev`（正式版的版本由 -ldflags 注入，不带后缀）；
//   - 打包脚本产出的 `.app` 名与 bundle id 也分开（dev 与 product 两个形态）。
func TestDevBuildCarriesDevSuffix(t *testing.T) {
	// ① 默认（未经 -ldflags 注入）的版本必须一眼看出是开发构建。
	if !strings.HasSuffix(version, "-dev") {
		t.Errorf("默认版本串 = %q，期望以 -dev 结尾（开发构建不得与正式版同名）", version)
	}
	if commit != "unknown" || buildTime != "unknown" {
		// 不是断言：注入值与默认值的形状不同，这里只登记事实供报告使用。
		t.Logf("构建期注入：commit=%q buildTime=%q", commit, buildTime)
	}

	//（② 打包脚本断言随 tray 桌面面删除注销；
	// 开发/正式命名纪律改由发布脚本与 web 打包面各自承接。）
}

// TestReleaseBuildInjectsCommitAndBuildTime 守住第 1 轮评估缺陷 #2 的修复面：
// 发布构建（YZAGMGR_RELEASE_VERSION 注入分支）必须**同时**注入 main.commit 与
// main.buildTime——此前只注 version，`/api/v1/version` 的 commit/buildTime 恒为
// unknown，发布件无法自证构建来源。commit 取 `git rev-parse --short HEAD`（非 git
// 树回落 unknown），buildTime 取 ISO8601 UTC 构建时刻。本地 dev 构建不注入（允许 unknown）。
func TestReleaseBuildInjectsCommitAndBuildTime(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("读 Makefile 失败: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"-X main.version=$$YZAGMGR_RELEASE_VERSION", // version 注入必须并存（不回归）
		"-X main.commit=$$build_commit",
		"-X main.buildTime=$$build_time",
		"git rev-parse --short HEAD",
		"date -u +%Y-%m-%dT%H:%M:%SZ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Makefile 缺 %q（发布构建必须注入 version/commit/buildTime 三件套）", want)
		}
	}
}
