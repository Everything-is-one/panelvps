package auth

import (
	"errors"
	"strings"
	"unicode"
)

// 常见弱密码（小写比对）。M0 内置一个精简表，
// 完整版（top 10000）在 M2 随设置页一起做成可更新数据文件。
var commonPasswords = map[string]struct{}{
	"12345678": {}, "123456789": {}, "1234567890": {}, "12345678910": {},
	"password": {}, "password1": {}, "password123": {}, "passw0rd": {},
	"qwertyuiop": {}, "qwerty123": {}, "qwerty123456": {}, "1qaz2wsx": {},
	"abc123456": {}, "abcd1234": {}, "a123456789": {}, "admin123": {},
	"admin123456": {}, "administrator": {}, "root123456": {}, "letmein123": {},
	"iloveyou": {}, "iloveyou123": {}, "woaini1314": {}, "woaini520": {},
	"5201314520": {}, "1314520520": {}, "aa123456789": {}, "welcome123": {},
	"monkey123": {}, "dragon123": {}, "sunshine123": {}, "princess123": {},
	"football123": {}, "baseball123": {}, "superman123": {}, "trustno1": {},
	"changeme": {}, "changeme123": {}, "default123": {}, "test123456": {},
	"guest123456": {}, "master123": {}, "shadow123": {}, "michael123": {},
	"jennifer123": {}, "computer123": {}, "internet123": {}, "samsung123": {},
	"panelvps": {}, "panelvps123": {}, "vps123456": {},
	"xray123456": {}, "server123": {}, "linux123456": {}, "debian123": {},
	// 下面是「词根」：用于识别 password1234 / admin2026 这类拼接式弱密码
	"admin": {}, "root": {}, "qwerty": {}, "abc": {}, "test": {},
	"guest": {}, "master": {}, "welcome": {}, "letmein": {},
}

// ValidatePassword 执行密码策略校验。
//
// 规则（对应 PRD S-B01）：
//  1. 长度 12–128 —— 上限是为了防止超长输入拖慢 argon2
//  2. 不在常见弱密码表中
//  3. 不包含用户名（忽略大小写）
//  4. 至少包含两类字符（小写/大写/数字/符号）
//
// 刻意不强制「必须含特殊字符」：那会逼用户写 P@ssw0rd! 这类反而更弱的密码。
func ValidatePassword(password, username string) error {
	n := len([]rune(password))
	if n < 12 {
		return errors.New("密码至少 12 位")
	}
	if n > 128 {
		return errors.New("密码不能超过 128 位")
	}

	lower := strings.ToLower(password)
	if _, bad := commonPasswords[lower]; bad {
		return errors.New("该密码在常见弱密码表中，请换一个")
	}
	// 归一化后再比一次：把数字与符号剥掉，识别 password1234、
	// Admin@2026 这类「常见词 + 数字」的拼接式弱密码。
	if base := lettersOnly(lower); len(base) >= 4 {
		if _, bad := commonPasswords[base]; bad {
			return errors.New("密码由常见词拼接而成，请换一个")
		}
	}
	if username != "" && strings.Contains(lower, strings.ToLower(username)) {
		return errors.New("密码不能包含用户名")
	}
	if isSingleRepeated(password) || isSequential(password) {
		return errors.New("密码过于简单（全相同字符或连续序列）")
	}

	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, r := range password {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasSymbol = true
		}
	}
	classes := 0
	for _, ok := range []bool{hasLower, hasUpper, hasDigit, hasSymbol} {
		if ok {
			classes++
		}
	}
	if classes < 2 {
		return errors.New("密码需要至少包含两类字符（大小写/数字/符号）")
	}
	return nil
}

// lettersOnly 只保留字母并转小写，用于识别拼接式弱密码。
func lettersOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func isSingleRepeated(s string) bool {
	rs := []rune(s)
	if len(rs) < 2 {
		return false
	}
	for _, r := range rs[1:] {
		if r != rs[0] {
			return false
		}
	}
	return true
}

// isSequential 识别 123456789012 与 abcdefghijkl 这类整串递增/递减。
func isSequential(s string) bool {
	rs := []rune(s)
	if len(rs) < 3 {
		return false
	}
	up, down := true, true
	for i := 1; i < len(rs); i++ {
		if rs[i] != rs[i-1]+1 {
			up = false
		}
		if rs[i] != rs[i-1]-1 {
			down = false
		}
		if !up && !down {
			return false
		}
	}
	return up || down
}
