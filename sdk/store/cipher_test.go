package store

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ── 令牌加密件（核心包公共件，纯标准库）─────────────────────────────────────
//
// 这里测的是 TokenCipher 本身的封/开契约：密钥纪律、密文布局、nonce 随机、
// 方案维度与明文拒绝。**驱动无关**——三个驱动子包的 Open 只是把它接进 SQL 路径
// （端到端纪律另有驱动子包与 internal/sqlcore 的测试）。

// testKey 是确定性的 32 字节测试密钥（只用于测试；真实部署从 Secret/环境注入）。
func testKey() []byte {
	b := make([]byte, KeyLen)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestCipherKeyDiscipline(t *testing.T) {
	// 无密钥 + 未显式 AllowPlaintext ⇒ 构造即错（默认拒绝明文），错误可读且不含密钥。
	_, err := NewTokenCipher(Options{})
	if !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("缺密钥应报 ErrKeyRequired，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "AllowPlaintext") {
		t.Fatalf("错误应指出显式开关，实际 %v", err)
	}
	if strings.Contains(err.Error(), string(testKey())) {
		t.Fatal("错误串不得含密钥内容")
	}

	// 长度不对 ⇒ ErrKeyLength（只报长度）。
	for _, n := range []int{1, 16, 31, 33} {
		key := make([]byte, n)
		_, err := NewTokenCipher(Options{TokenKey: key})
		if !errors.Is(err, ErrKeyLength) {
			t.Fatalf("密钥 %d 字节应报 ErrKeyLength，实际 %v", n, err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d 字节", n)) {
			t.Fatalf("错误应只报长度，实际 %v", err)
		}
	}

	// 密钥 + AllowPlaintext 同时给出 ⇒ 歧义配置，拒绝。
	if _, err := NewTokenCipher(Options{TokenKey: testKey(), AllowPlaintext: true}); !errors.Is(err, ErrConfig) {
		t.Fatalf("互斥配置应报 ErrConfig，实际 %v", err)
	}
	// 合法密钥 ⇒ 加密模式（Plaintext()=false）。
	tc, err := NewTokenCipher(Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("32 字节密钥应可构造，实际 %v", err)
	}
	if tc.Plaintext() {
		t.Fatal("给了密钥就不是明文模式")
	}
	// 显式明文模式 ⇒ Plaintext()=true（构造期警示日志由驱动侧 open 记）。
	plainCipher, err := NewTokenCipher(Options{AllowPlaintext: true})
	if err != nil {
		t.Fatalf("显式 AllowPlaintext 应可构造，实际 %v", err)
	}
	if !plainCipher.Plaintext() {
		t.Fatal("显式 AllowPlaintext 应进入明文模式")
	}
}

func TestCipherSealOpenRoundTrip(t *testing.T) {
	const plain = "sk-live-abc123-DO-NOT-LEAK"
	tc, err := NewTokenCipher(Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("NewTokenCipher 失败：%v", err)
	}

	blob, scheme := tc.Seal(plain)
	if scheme != SchemeAESGCM {
		t.Fatalf("scheme 应为 %s，实际 %s", SchemeAESGCM, scheme)
	}
	if bytes.Contains(blob, []byte(plain)) || string(blob) == plain {
		t.Fatal("密文列里出现了明文")
	}
	// 布局：nonce(12) || ct || tag(16)。
	if len(blob) != 12+len(plain)+16 {
		t.Fatalf("密文布局应为 nonce(12)+ct+tag(16)=%d，实际 %d", 12+len(plain)+16, len(blob))
	}
	got, err := tc.Open("ref", blob, scheme)
	if err != nil || got != plain {
		t.Fatalf("Open = %q, %v", got, err)
	}
	// 同值两次封：nonce 随机 ⇒ 密文不同（不可用密文相等反推「令牌没变」）。
	blob2, _ := tc.Seal(plain)
	if bytes.Equal(blob, blob2) {
		t.Fatal("同值两次封的密文相同：nonce 不是随机的")
	}
	if got2, err := tc.Open("ref", blob2, scheme); err != nil || got2 != plain {
		t.Fatalf("第二次密文解回应为明文，实际 %q, %v", got2, err)
	}

	// 换密钥读旧密文 ⇒ ErrCipher（不是「返回垃圾」也不是 panic）；错误串不含明文。
	other := make([]byte, KeyLen)
	for i := range other {
		other[i] = 0xAA
	}
	otherCipher, err := NewTokenCipher(Options{TokenKey: other})
	if err != nil {
		t.Fatalf("NewTokenCipher(新密钥) 失败：%v", err)
	}
	_, err = otherCipher.Open("ref", blob, scheme)
	if !errors.Is(err, ErrCipher) {
		t.Fatalf("换密钥读旧密文应报 ErrCipher，实际 %v", err)
	}
	if strings.Contains(err.Error(), plain) {
		t.Fatalf("错误串不得含明文令牌，实际 %v", err)
	}

	// 密文长度不足 ⇒ ErrCipher（不返回部分明文）。
	if _, err := tc.Open("ref", blob[:20], scheme); !errors.Is(err, ErrCipher) {
		t.Fatalf("截断密文应报 ErrCipher，实际 %v", err)
	}
	// 未知方案 ⇒ ErrCipher。
	if _, err := tc.Open("ref", blob, "rot13"); !errors.Is(err, ErrCipher) {
		t.Fatalf("未知方案应报 ErrCipher，实际 %v", err)
	}
}

func TestCipherPlaintextModeIsExplicit(t *testing.T) {
	tc, err := NewTokenCipher(Options{AllowPlaintext: true})
	if err != nil {
		t.Fatalf("NewTokenCipher 失败：%v", err)
	}
	blob, scheme := tc.Seal("plain-token")
	if scheme != SchemePlaintext || string(blob) != "plain-token" {
		t.Fatalf("明文模式应落明文并标 scheme=plaintext，实际 scheme=%s blob=%q", scheme, blob)
	}
	if got, err := tc.Open("ref", blob, scheme); err != nil || got != "plain-token" {
		t.Fatalf("明文模式应可读：%q, %v", got, err)
	}

	// 加密模式读明文行 ⇒ 拒绝（读写两侧都 fail-closed）。
	strict, err := NewTokenCipher(Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("NewTokenCipher(加密) 失败：%v", err)
	}
	if _, err := strict.Open("ref", blob, SchemePlaintext); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("未开明文开关时读明文行应报 ErrPlaintextRefused，实际 %v", err)
	}
	// 明文模式读加密行 ⇒ 缺密钥，报 ErrKeyRequired（不是静默回垃圾）。
	if _, err := tc.Open("ref", blob, SchemeAESGCM); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("明文模式读密文行应报 ErrKeyRequired，实际 %v", err)
	}
}
