package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// 令牌列的加密形态（cipher_scheme 列的取值集合）。落库时逐行记 scheme，于是
// 「哪几行是明文」在库里可审计（psql 直接可查），也不必靠列长度猜。
const (
	// SchemeAESGCM 是默认形态：AES-256-GCM 密文（列本身是二进制类型）。
	SchemeAESGCM = "aes-256-gcm"
	// SchemePlaintext 只在显式 AllowPlaintext 时落库（仅限本地开发）。
	SchemePlaintext = "plaintext"
)

// KeyLen 是 AES-256 的密钥长度（字节）。
const KeyLen = 32

// TokenCipher 负责令牌列的封/开。aead 为 nil 表示**明文模式**（显式 AllowPlaintext）；
// 两种模式互斥，构造期已判定。
//
// 本类型是**三个驱动子包共用**的加密件（放在核心包是为了让 `sdk/store` 只依赖标准库，
// 驱动的 SQL 实现经 internal/sqlcore 取用）。零值不可用：必须经 NewTokenCipher 构造。
//
// 密文布局：`nonce(12B) || GCM(明文) || tag(16B)`（cipher.AEAD.Seal 的标准输出，
// nonce 前置）。**没有**第二层编码：列本身是二进制类型（BLOB/BYTEA/MEDIUMBLOB），
// psql 回读即为 `\x…`，一眼可辨「不是明文」。
type TokenCipher struct {
	aead      cipher.AEAD
	plaintext bool
}

// NewTokenCipher 按 Options 构造封/开器（三驱动共用）。
//
// fail-closed 顺序（也决定了对无 server 的驱动仍是确定性错误）：
//   - TokenKey 与 AllowPlaintext 同时给出 ⇒ 歧义配置，拒绝（不猜谁是本意）；
//   - 无 TokenKey 且未显式 AllowPlaintext ⇒ **构造即错**（默认拒绝明文）；
//   - TokenKey 长度 ≠ 32 ⇒ 构造即错（错误串只含长度，绝不含密钥内容）。
func NewTokenCipher(opts Options) (TokenCipher, error) {
	switch {
	case len(opts.TokenKey) > 0 && opts.AllowPlaintext:
		return TokenCipher{}, fmt.Errorf("%w：TokenKey 与 AllowPlaintext 同时给出（互斥）：给了密钥就一律加密，要明文就别给密钥", ErrConfig)
	case len(opts.TokenKey) == 0:
		if !opts.AllowPlaintext {
			return TokenCipher{}, fmt.Errorf("%w：未提供 32 字节 TokenKey。默认拒绝明文令牌落库；"+
				"确需明文（仅限本地开发）请显式设置 Options.AllowPlaintext=true", ErrKeyRequired)
		}
		return TokenCipher{plaintext: true}, nil
	case len(opts.TokenKey) != KeyLen:
		return TokenCipher{}, fmt.Errorf("%w：AES-256-GCM 需要 %d 字节密钥，实际 %d 字节（密钥内容不落错误串）",
			ErrKeyLength, KeyLen, len(opts.TokenKey))
	}
	// 长度已由上面的分支收敛到 KeyLen：下面两处构造**不可达失败**，保留检查是防御性的
	// （覆盖报告里这两行登记为不可达：aes.NewCipher 只按长度拒绝，cipher.NewGCM 只按
	// 块长拒绝，而 AES 块长恒 16）。
	block, err := aes.NewCipher(opts.TokenKey)
	if err != nil {
		// 长度已校验，这里只可能在实现层出问题；错误串仍只带原因不带密钥。
		return TokenCipher{}, fmt.Errorf("%w：初始化 AES 失败：%v", ErrKeyLength, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return TokenCipher{}, fmt.Errorf("%w：初始化 GCM 失败：%v", ErrCipher, err)
	}
	return TokenCipher{aead: aead}, nil
}

// Plaintext 报告本实例是否处于明文模式（存储实现据此记构造期警示日志）。
func (c TokenCipher) Plaintext() bool { return c.plaintext }

// Seal 把明文封成落库字节 + 方案名。每次调用都取**新随机 nonce**，于是同一个明文
// 两次落库的密文不同（不可用密文相等反推「令牌没变」）。
func (c TokenCipher) Seal(plaintext string) (blob []byte, scheme string) {
	if c.aead == nil {
		return []byte(plaintext), SchemePlaintext
	}
	nonce := make([]byte, c.aead.NonceSize())
	// Go 1.24+ 的 crypto/rand.Read 不返回错误（熵源不可用时内部 panic，不会退化）。
	_, _ = rand.Read(nonce)
	return c.aead.Seal(nonce, nonce, []byte(plaintext), nil), SchemeAESGCM
}

// Open 把落库字节解回明文。错误串只含凭据引用与原因，**绝不**含明文或密钥；
// 解密失败按不可用处理（不返回部分明文）。
func (c TokenCipher) Open(credRef string, blob []byte, scheme string) (string, error) {
	switch scheme {
	case SchemeAESGCM:
		if c.aead == nil {
			return "", fmt.Errorf("%w：令牌 %q 以 %s 落库，但当前 Store 未提供 TokenKey", ErrKeyRequired, credRef, SchemeAESGCM)
		}
		ns := c.aead.NonceSize()
		if len(blob) < ns+c.aead.Overhead() {
			return "", fmt.Errorf("%w：令牌 %q 的密文长度非法（%d 字节，至少 %d）", ErrCipher, credRef, len(blob), ns+c.aead.Overhead())
		}
		plain, err := c.aead.Open(nil, blob[:ns], blob[ns:], nil)
		if err != nil {
			return "", fmt.Errorf("%w：令牌 %q 解密失败（密钥不匹配或密文被改）", ErrCipher, credRef)
		}
		return string(plain), nil
	case SchemePlaintext:
		if !c.plaintext {
			return "", fmt.Errorf("%w：令牌 %q 在库里是明文（cipher_scheme=%s），当前 Store 未显式启用 "+
				"AllowPlaintext，拒绝读取", ErrPlaintextRefused, credRef, SchemePlaintext)
		}
		return string(blob), nil
	default:
		return "", fmt.Errorf("%w：令牌 %q 的存储方案未知：%q", ErrCipher, credRef, scheme)
	}
}
