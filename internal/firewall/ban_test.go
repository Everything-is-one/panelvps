package firewall

import (
	"testing"
	"time"
)

func TestIptBanCommentRoundTrip(t *testing.T) {
	expire := time.Now().Add(time.Hour).Unix()
	comment := iptBanComment(expire)
	if comment != "panelvps-ban:"+itoa(expire) {
		t.Fatalf("注释格式不对: %q", comment)
	}
	got, permanent, ok := parseIptBanComment(comment)
	if !ok || permanent || got != expire {
		t.Fatalf("解析结果不对: expire=%d permanent=%v ok=%v", got, permanent, ok)
	}
}

func TestParseIptBanCommentPermanent(t *testing.T) {
	_, permanent, ok := parseIptBanComment(iptBanComment(0))
	if !ok || !permanent {
		t.Fatalf("0 应解析为永久: permanent=%v ok=%v", permanent, ok)
	}
}

// 老版本写的是不带时间戳的注释。把它当永久处理：
// 宁可让用户手动解封，也不要在升级时把已封的 IP 一起放出来。
func TestParseIptBanCommentLegacy(t *testing.T) {
	_, permanent, ok := parseIptBanComment("panelvps-ban")
	if !ok || !permanent {
		t.Fatalf("老注释应按永久处理: permanent=%v ok=%v", permanent, ok)
	}
}

func TestParseIptBanCommentOther(t *testing.T) {
	for _, bad := range []string{"", "ssh safety", "panelvps-banx:1", "panelvps-ban:abc"} {
		if _, _, ok := parseIptBanComment(bad); ok {
			t.Errorf("%q 不应被识别成本模块的封禁注释", bad)
		}
	}
}

// iptables -S 的输出里注释是带引号的，commentExpiryIn 必须能取出来。
func TestCommentExpiryIn(t *testing.T) {
	line := `-A INPUT -s 1.2.3.4/32 -j DROP -m comment --comment "panelvps-ban:1730000000"`
	expire, permanent, ok := commentExpiryIn(line)
	if !ok || permanent || expire != 1730000000 {
		t.Fatalf("解析失败: expire=%d permanent=%v ok=%v", expire, permanent, ok)
	}

	line = `-A INPUT -s 5.6.7.8/32 -j DROP -m comment --comment "panelvps-ban:0"`
	_, permanent, ok = commentExpiryIn(line)
	if !ok || !permanent {
		t.Fatalf("永久规则解析失败: permanent=%v ok=%v", permanent, ok)
	}
}

func TestIptBanCommentExpiryIsInFuture(t *testing.T) {
	// 临时封禁必须给出未来的时刻，否则 SweepExpiredBans 会立刻把它删掉
	expire := time.Now().Add(60 * time.Minute).Unix()
	if expire <= time.Now().Unix() {
		t.Fatal("到期时间应该在将来")
	}
	if _, permanent, ok := parseIptBanComment(iptBanComment(expire)); !ok || permanent {
		t.Fatal("临时封禁不应被当成永久")
	}
}

// itoa 只是避免测试里再引一个 strconv，保持与实现同一个格式。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
