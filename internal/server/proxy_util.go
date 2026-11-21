package server

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Everything-is-one/panelvps/internal/xray"
)

// marshalOptions 把选项序列化为入库 JSON。
func marshalOptions(o xray.Options) (string, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// mergeOptions 做「部分更新」合并：请求里没给的字段沿用旧值。
//
// 这对 private_key 尤其重要——私钥不在前端展示，
// 如果按提交值覆盖，用户改一次备注就会把 REALITY 私钥清空。
func mergeOptions(oldV, newV xray.Options) xray.Options {
	out := oldV
	if newV.Dest != "" {
		out.Dest = newV.Dest
	}
	if len(newV.ServerNames) > 0 {
		out.ServerNames = newV.ServerNames
	}
	if len(newV.ShortIDs) > 0 {
		out.ShortIDs = newV.ShortIDs
	}
	if newV.PrivateKey != "" {
		out.PrivateKey = newV.PrivateKey
		out.PublicKey = "" // 私钥变了，公钥必须重算
	}
	if newV.PublicKey != "" {
		out.PublicKey = newV.PublicKey
	}
	if newV.CertFile != "" {
		out.CertFile = newV.CertFile
	}
	if newV.KeyFile != "" {
		out.KeyFile = newV.KeyFile
	}
	if len(newV.Alpn) > 0 {
		out.Alpn = newV.Alpn
	}
	if newV.WSPath != "" {
		out.WSPath = newV.WSPath
	}
	if newV.GRPCService != "" {
		out.GRPCService = newV.GRPCService
	}
	if newV.Method != "" {
		out.Method = newV.Method
	}
	return out
}

// sqlNullTime 把时间转成可空列值；零值表示 NULL。
func sqlNullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}
