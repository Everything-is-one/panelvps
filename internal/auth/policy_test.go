package auth

import (
	"strings"
	"testing"
)

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name     string
		password string
		username string
		wantErr  string // 为空表示应当通过
	}{
		{"正常强密码", "Str0ng-Passw0rd-2026", "admin", ""},
		{"长口令", "correct horse battery staple", "admin", ""},
		{"太短", "Ab3-x8Qz", "admin", "至少 12 位"},
		{"常见弱密码", "qwerty123456", "admin", "常见弱密码表"},
		{"拼接式弱密码", "password1234", "admin", "常见词拼接"},
		{"含用户名", "AdminPass-2026", "admin", "包含用户名"},
		{"单一字符", "aaaaaaaaaaaa", "admin", "过于简单"},
		{"纯数字序列", "123456789012", "admin", "两类字符"},
		{"纯小写长口令", "mypassphraseonly", "admin", "两类字符"},
		{"超长", strings.Repeat("Ab3!", 70), "admin", "不能超过"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password, tc.username)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错包含 %q，实际通过", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息不匹配，期望包含 %q，实际 %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestHashAndVerify(t *testing.T) {
	const pwd = "Str0ng-Passw0rd-2026"
	hash, err := HashPassword(pwd)
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("哈希格式不符: %s", hash)
	}

	ok, err := VerifyPassword(pwd, hash)
	if err != nil || !ok {
		t.Fatalf("正确密码校验失败: ok=%v err=%v", ok, err)
	}

	ok, err = VerifyPassword("wrong-password-123", hash)
	if err != nil {
		t.Fatalf("错误密码不应报格式错: %v", err)
	}
	if ok {
		t.Fatal("错误密码被判定为正确")
	}

	if _, err := VerifyPassword(pwd, "not-a-hash"); err == nil {
		t.Fatal("非法哈希应当返回错误")
	}
}

func TestSessionToken(t *testing.T) {
	token, hash, err := NewSessionToken()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	if len(token) < 40 {
		t.Fatalf("令牌过短: %d", len(token))
	}
	if HashToken(token) != hash {
		t.Fatal("令牌哈希不一致")
	}
	token2, hash2, _ := NewSessionToken()
	if token == token2 || hash == hash2 {
		t.Fatal("两次生成的令牌重复")
	}
}

func TestLoginLimiterBlocks(t *testing.T) {
	l := NewLoginLimiter(5, 0, 0) // 直接用零窗口，专注计数逻辑
	for i := 0; i < 4; i++ {
		if l.Fail("1.2.3.4") {
			t.Fatalf("第 %d 次不应触发封禁", i+1)
		}
	}
	if !l.Fail("1.2.3.4") {
		t.Fatal("第 5 次应触发封禁")
	}
	l.Reset("1.2.3.4")
	if l.Blocked("1.2.3.4") > 0 {
		t.Fatal("重置后不应仍处于封禁状态")
	}
}
