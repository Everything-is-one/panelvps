package config

import (
	"errors"
	"regexp"
	"strings"
)

// accessPathRe 限制访问后缀的字符集。
//
// 只允许 URL 里无需转义的字符：这个值会出现在 <meta>、fetch 路径与
// 纯文本说明里，多一个需要转义的字符就多一处出错的机会。
var accessPathRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{2,64}$`)

// NormalizeAccessPath 把用户输入整理成可以比较的形式。
//
// 容忍手滑输入 "/abc/"、" abc " 这类写法：统一去掉首尾斜杠与空白。
// 不做小写化——路径区分大小写，擅自改掉会让人按自己对的大小写敲不进来。
func NormalizeAccessPath(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "/")
	return v
}

// ValidateAccessPath 校验访问后缀。空字符串表示不启用。
func ValidateAccessPath(v string) error {
	v = NormalizeAccessPath(v)
	if v == "" {
		return nil
	}
	if !accessPathRe.MatchString(v) {
		return errors.New("访问后缀只能用字母、数字与 . _ ~ -，长度 2–64")
	}
	return nil
}
