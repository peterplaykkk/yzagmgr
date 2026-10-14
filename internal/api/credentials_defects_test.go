// 托管凭据「尾随换行」存量防御的单测（第 1 轮评估缺陷 #3）。
//
// 面：解析（resolveCredRefToken）一律 TrimSpace + 拒绝控制字符；渲染（credentialMask /
// credentialViewOf）掩码不得带控制字符；值本身不删、不改盘上数据。
//
// 夹具直接落一份 schema 2 的「存量」凭据库：带尾随换行的明文 + 按旧口径算出的、
// 尾段含控制字符的落盘掩码——那是录入面校验收紧之前的真实形态。
package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

const legacyCredentialUID = "mt-0123456789abcdef0123456789abcdef"

// seedLegacyCredentialVault 落一份存量凭据库并打开（返回的 vault 已装载该条）。
func seedLegacyCredentialVault(t *testing.T, token, mask string) *tokenVault {
	t.Helper()
	env := &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	doc := managedTokensDoc{Schema: managedTokensSchema, Entries: map[string]managedTokenEntry{
		legacyCredentialUID: {
			Name:      "旧值",
			Provider:  "deepseek",
			UpdatedAt: "2026-09-20T00:00:00Z",
			Mask:      mask,
			Token:     token,
		},
	}}
	blob, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("编码存量凭据库失败: %v", err)
	}
	if err := os.WriteFile(env.ManagedTokensFile(), blob, 0o600); err != nil {
		t.Fatalf("写入存量凭据库失败: %v", err)
	}
	return newTokenVault(env)
}

// TestLegacyCredentialTrailingNewlineIsTrimmedAtResolution 带尾随换行的存量值经解析后
// 无控制字符，且盘上数据逐字节未动。
func TestLegacyCredentialTrailingNewlineIsTrimmedAtResolution(t *testing.T) {
	const rawToken = "sk-legacy-secret-1234567890abcdef\n"
	v := seedLegacyCredentialVault(t, rawToken, "sk-****def\n")
	srv := &Server{routerTokens: v}

	before, err := os.ReadFile(v.path)
	if err != nil {
		t.Fatalf("读存量库失败: %v", err)
	}

	got, err := srv.resolveCredRefToken(proxy.ArmTokenScheme + legacyCredentialUID)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != "sk-legacy-secret-1234567890abcdef" {
		t.Fatalf("解析结果 = %q，期望去掉尾随换行", got)
	}
	if hasControlRune(got) {
		t.Fatalf("解析结果仍含控制字符: %q", got)
	}

	after, err := os.ReadFile(v.path)
	if err != nil {
		t.Fatalf("回读存量库失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("解析不得改动盘上数据:\nbefore=%s\nafter =%s", before, after)
	}
	if !strings.Contains(string(after), "abcdef\\n") {
		t.Fatalf("盘上原值（含换行转义）应原样保留: %s", after)
	}
}

// TestLegacyCredentialInteriorControlCharIsRejected 修剪后仍含控制字符（值内部换行）
// ⇒ 明确拒绝（config_invalid），而不是把坏形态注进配置。
func TestLegacyCredentialInteriorControlCharIsRejected(t *testing.T) {
	v := seedLegacyCredentialVault(t, "sk-inter\nior-secret-0123456789", "")
	srv := &Server{routerTokens: v}

	_, err := srv.resolveCredRefToken(proxy.ArmTokenScheme + legacyCredentialUID)
	if err == nil {
		t.Fatal("值内部含控制字符应被拒绝")
	}
	if ae := apierr.From(err); ae.Code != apierr.CodeConfigInvalid {
		t.Fatalf("错误码 = %q，期望 config_invalid（指向轮换）", ae.Code)
	}
}

// TestLegacyMaskRenderingStripsControlChars 渲染面：落盘的存量掩码若带控制字符
// 整值替换；新口径掩码先 TrimSpace 再计算，输出不含控制字符。
func TestLegacyMaskRenderingStripsControlChars(t *testing.T) {
	v := seedLegacyCredentialVault(t, "sk-legacy-secret-1234567890abcdef\n", "sk-****def\n")
	mc, ok := v.View(legacyCredentialUID)
	if !ok {
		t.Fatal("存量记录缺失")
	}
	view := credentialViewOf(mc)
	if hasControlRune(view.Mask) {
		t.Fatalf("渲染掩码含控制字符: %q", view.Mask)
	}
	if view.Mask != envcheck.MaskedValue {
		t.Fatalf("带控制字符的存量掩码应整值替换为 %q，实际 %q", envcheck.MaskedValue, view.Mask)
	}

	// 新口径：尾随换行先按展示口径修剪，前 3 + 后 4 照常给指纹，不带控制字符。
	got := credentialMask("sk-legacy-secret-1234567890abcdef\n")
	if got != "sk-****cdef" {
		t.Fatalf("credentialMask = %q，期望 sk-****cdef", got)
	}
	if hasControlRune(got) {
		t.Fatalf("credentialMask 输出含控制字符: %q", got)
	}
	// 内部含控制字符的值形态不可信 ⇒ 整值替换。
	if got := credentialMask("sk-internal\nsecret-0123456789"); got != envcheck.MaskedValue {
		t.Fatalf("内部含控制字符应整值替换，实际 %q", got)
	}
	// 纯空白（含换行）等同空值。
	if got := credentialMask(" \n\t "); got != "" {
		t.Fatalf("纯空白应返回空串，实际 %q", got)
	}
}
