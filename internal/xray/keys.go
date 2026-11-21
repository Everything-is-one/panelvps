package xray

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// NewUUID 生成 UUIDv4，用于 VLESS / VMess。
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// NewRealityKeypair 生成 X25519 密钥对，返回值可直接填入 Xray 配置。
//
// 私钥进服务端配置，公钥进客户端分享链接；两者都是 43 字符的 base64url。
func NewRealityKeypair() (privateKey, publicKey string, err error) {
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return "", "", err
	}
	// X25519 会做 clamp，这里显式处理一遍保证与 xray x25519 输出一致
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(priv[:]), enc.EncodeToString(pub), nil
}

// PublicFromPrivate 由私钥推导公钥，用于用户只填了私钥的场景。
func PublicFromPrivate(privateKey string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil {
		return "", fmt.Errorf("私钥不是合法的 base64url: %w", err)
	}
	if len(raw) != 32 {
		return "", errors.New("私钥长度必须是 32 字节")
	}
	pub, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(pub), nil
}

// NewShortID 生成 REALITY 的 shortId（8 字节十六进制，长度必须是偶数）。
func NewShortID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NewPassword 生成 URL 安全的随机密码。
func NewPassword(n int) (string, error) {
	if n < 8 {
		n = 8
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

// ssKeyLen 返回 Shadowsocks-2022 各加密方式要求的密钥字节数。
func ssKeyLen(method string) (int, bool) {
	switch method {
	case "2022-blake3-aes-128-gcm":
		return 16, true
	case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		return 32, true
	default:
		return 0, false
	}
}

// IsSS2022 判断是否为 2022 系列（这些方法要求固定长度的 base64 密钥）。
func IsSS2022(method string) bool {
	_, ok := ssKeyLen(method)
	return ok
}

// SSKeyLen 返回该加密方式要求的密钥字节数。
func SSKeyLen(method string) int {
	n, _ := ssKeyLen(method)
	return n
}

// IsUUID 校验 UUID 文本格式。
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	return err == nil && len(raw) == 16
}

// IsSSKeyValid 校验 Shadowsocks-2022 的 base64 密钥长度。
func IsSSKeyValid(method, key string) bool {
	n, ok := ssKeyLen(method)
	if !ok {
		return key != ""
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(key)
	}
	return err == nil && len(raw) == n
}

// NewSSPassword 按加密方式生成合规密钥。
func NewSSPassword(method string) (string, error) {
	n, ok := ssKeyLen(method)
	if !ok {
		return "", fmt.Errorf("不支持的 Shadowsocks 加密方式 %q", method)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
