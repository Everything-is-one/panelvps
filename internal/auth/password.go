// Package auth 实现密码哈希、会话令牌与登录限流。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 参数。
//
// 故意选择 OWASP 推荐的低内存档（m=19MiB）而不是 64MiB：
// 低配机器可用内存有限，且面板与 Xray 共用同一台机器，
// 单个哈希运算占 64MB 会造成明显的瞬时抖动。
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrInvalidHash 表示数据库里的哈希格式不可识别。
var ErrInvalidHash = errors.New("密码哈希格式非法")

// HashPassword 生成 argon2id 哈希，格式：
// $argon2id$v=19$m=19456,t=2,p=1$<salt-b64>$<hash-b64>
//
// 这里只做「防滥用」的长度上限，具体策略由 ValidatePassword 负责——
// 否则哈希函数与密码策略会耦合，改策略要改两处。
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("密码不能为空")
	}
	if len(password) > 256 {
		return "", errors.New("密码过长")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword 校验密码。使用 subtle.ConstantTimeCompare 避免时序侧信道。
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	if version != argon2.Version {
		return false, fmt.Errorf("不支持的 argon2 版本: %d", version)
	}

	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}

	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NewSessionToken 生成 32 字节随机令牌，返回明文与落库用的哈希。
// 明文只存在于 Cookie，数据库里永远只有哈希。
func NewSessionToken() (token string, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("生成会话令牌失败: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken 对令牌做 SHA-256。令牌本身熵足够，无需再加盐。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RandomPassword 生成指定长度的随机密码，用于首次安装。
// 去掉了易混淆字符（0/O、1/l/I），方便用户手抄。
func RandomPassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#%^&*-_=+"
	if n < 8 {
		n = 8
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}
