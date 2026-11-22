// Package geoip 把公网 IP 解析成国家/地区，用于「外网攻击记录」的归属地列。
//
// 为什么不是内置库：一份覆盖全球的 GeoLite2 国家库压缩后也有几 MB，
// 编进二进制会让「14MB 单文件」这个卖点立刻失效；而大多数用户只想在
// 攻击列表里一眼看出「这批 IP 来自哪」。所以这里走在线查询 + 内存缓存，
// 失败就留空——归属地是锦上添花，不能因为它让安全页报错。
//
// 只发送攻击来源 IP（本身已是公开信息），不发送任何面板数据。
package geoip

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Info 是一个 IP 的归属地。
type Info struct {
	Country string `json:"country"` // 中文名，取不到时回落到国家代码
	Code    string `json:"code"`    // ISO 3166-1 alpha-2
}

type cacheEntry struct {
	info    Info
	expires time.Time
}

// Resolver 带缓存的国家查询器。并发安全。
type Resolver struct {
	mu    sync.Mutex
	cache map[string]cacheEntry
	cli   *http.Client

	// now 便于测试替换
	now func() time.Time
}

const (
	hitTTL  = 12 * time.Hour
	missTTL = 10 * time.Minute
	// maxLookup 单次请求最多解析多少个 IP：列表一屏也就几十条，
	// 留一点余量，同时避免有人拿它当批量查询接口用。
	maxLookup = 200
)

// New 构造一个查询器。
func New() *Resolver {
	return &Resolver{
		cache: make(map[string]cacheEntry),
		cli:   &http.Client{Timeout: 6 * time.Second},
		now:   time.Now,
	}
}

// Lookup 批量解析。返回的 map 只包含有结果的 IP；
// 网络不可用时返回空 map，不返回错误——调用方不需要为它写失败分支。
func (r *Resolver) Lookup(ctx context.Context, ips []string) map[string]Info {
	out := make(map[string]Info, len(ips))
	var need []string
	now := r.now()

	r.mu.Lock()
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		parsed := net.ParseIP(strings.TrimSpace(ip))
		if parsed == nil {
			// 解析不了就整条跳过：既不查，也不显示成「内网」
			continue
		}
		if isPrivate(parsed) {
			out[ip] = Info{Country: "内网"}
			continue
		}
		if e, ok := r.cache[ip]; ok && now.Before(e.expires) {
			if e.info.Country != "" {
				out[ip] = e.info
			}
			continue
		}
		need = append(need, ip)
	}
	r.mu.Unlock()

	if len(need) == 0 {
		return out
	}
	if len(need) > maxLookup {
		need = need[:maxLookup]
	}

	got := r.fetch(ctx, need)
	r.mu.Lock()
	for _, ip := range need {
		info, ok := got[ip]
		ttl := missTTL
		if ok && info.Country != "" {
			ttl = hitTTL
			out[ip] = info
		}
		r.cache[ip] = cacheEntry{info: info, expires: now.Add(ttl)}
	}
	// 顺手清掉过期条目，避免长期运行时 map 只增不减
	if len(r.cache) > 4096 {
		for ip, e := range r.cache {
			if now.After(e.expires) {
				delete(r.cache, ip)
			}
		}
	}
	r.mu.Unlock()
	return out
}

// fetch 依次尝试两个免费接口，任一成功就返回。
//
// 第一个给中文国名，第二个只给国家代码（国内访问第一个可能超时）。
func (r *Resolver) fetch(ctx context.Context, ips []string) map[string]Info {
	if got := r.fetchIPAPI(ctx, ips); len(got) > 0 {
		return got
	}
	return r.fetchCountryIs(ctx, ips)
}

func (r *Resolver) fetchIPAPI(ctx context.Context, ips []string) map[string]Info {
	body, err := json.Marshal(ips)
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://ip-api.com/batch?fields=status,country,countryCode,query&lang=zh-CN",
		bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.cli.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var rows []struct {
		Status      string `json:"status"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		Query       string `json:"query"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil
	}
	out := make(map[string]Info, len(rows))
	for _, row := range rows {
		if row.Status != "success" || row.Query == "" {
			continue
		}
		out[row.Query] = Info{Country: row.Country, Code: strings.ToUpper(row.CountryCode)}
	}
	return out
}

func (r *Resolver) fetchCountryIs(ctx context.Context, ips []string) map[string]Info {
	out := make(map[string]Info, len(ips))
	for _, ip := range ips {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.country.is/"+ip, nil)
		if err != nil {
			continue
		}
		resp, err := r.cli.Do(req)
		if err != nil {
			continue
		}
		var row struct {
			Country string `json:"country"`
			IP      string `json:"ip"`
		}
		err = json.NewDecoder(resp.Body).Decode(&row)
		resp.Body.Close()
		if err != nil || row.Country == "" {
			continue
		}
		code := strings.ToUpper(row.Country)
		name := CountryNames[code]
		if name == "" {
			name = code
		}
		out[ip] = Info{Country: name, Code: code}
		// 第二个接口是逐个查的，列表一长会拖住页面，限制一下条数
		if len(out) >= 20 {
			break
		}
	}
	return out
}

// isPrivate 判断内网 / 保留地址：这些查归属地没有意义，还会白花花耗一次请求。
func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// CountryNames 是常见国家/地区的中文名，供不支持语言的接口兜底。
// 不追求全量：查不到的显示两位国家代码，同样能辨认。
var CountryNames = map[string]string{
	"US": "美国", "CN": "中国", "HK": "中国香港", "TW": "中国台湾", "MO": "中国澳门",
	"RU": "俄罗斯", "NL": "荷兰", "DE": "德国", "FR": "法国", "GB": "英国",
	"BR": "巴西", "IN": "印度", "VN": "越南", "SG": "新加坡", "JP": "日本",
	"KR": "韩国", "TH": "泰国", "ID": "印尼", "MY": "马来西亚", "PH": "菲律宾",
	"AU": "澳大利亚", "CA": "加拿大", "UA": "乌克兰", "RO": "罗马尼亚",
	"BG": "保加利亚", "PL": "波兰", "SE": "瑞典", "CH": "瑞士", "AT": "奥地利",
	"IT": "意大利", "ES": "西班牙", "PT": "葡萄牙", "TR": "土耳其", "IR": "伊朗",
	"IL": "以色列", "AE": "阿联酋", "SA": "沙特", "EG": "埃及", "ZA": "南非",
	"NG": "尼日利亚", "KE": "肯尼亚", "MX": "墨西哥", "AR": "阿根廷", "CL": "智利",
	"CO": "哥伦比亚", "PE": "秘鲁", "VE": "委内瑞拉", "PA": "巴拿马", "CR": "哥斯达黎加",
	"MD": "摩尔多瓦", "LT": "立陶宛", "LV": "拉脱维亚", "EE": "爱沙尼亚",
	"FI": "芬兰", "NO": "挪威", "DK": "丹麦", "CZ": "捷克", "SK": "斯洛伐克",
	"HU": "匈牙利", "GR": "希腊", "RS": "塞尔维亚", "HR": "克罗地亚", "SI": "斯洛文尼亚",
	"IE": "爱尔兰", "BE": "比利时", "LU": "卢森堡", "IS": "冰岛", "NZ": "新西兰",
	"PK": "巴基斯坦", "BD": "孟加拉", "LK": "斯里兰卡", "NP": "尼泊尔", "MM": "缅甸",
	"KH": "柬埔寨", "LA": "老挝", "MN": "蒙古", "KZ": "哈萨克斯坦", "UZ": "乌兹别克斯坦",
	"AM": "亚美尼亚", "GE": "格鲁吉亚", "AZ": "阿塞拜疆", "BY": "白俄罗斯",
	"CY": "塞浦路斯", "MT": "马耳他", "AL": "阿尔巴尼亚", "MK": "北马其顿",
	"BA": "波黑", "ME": "黑山", "XK": "科索沃", "GI": "直布罗陀", "SC": "塞舌尔",
	"MU": "毛里求斯", "MG": "马达加斯加", "TZ": "坦桑尼亚", "UG": "乌干达",
	"GH": "加纳", "CI": "科特迪瓦", "SN": "塞内加尔", "MA": "摩洛哥", "TN": "突尼斯",
	"DZ": "阿尔及利亚", "LY": "利比亚", "SD": "苏丹", "ET": "埃塞俄比亚",
	"UY": "乌拉圭", "PY": "巴拉圭", "BO": "玻利维亚", "EC": "厄瓜多尔", "GT": "危地马拉",
	"DO": "多米尼加", "CU": "古巴", "TT": "特立尼达和多巴哥", "BB": "巴巴多斯",
	"PR": "波多黎各", "BZ": "伯利兹", "HN": "洪都拉斯", "SV": "萨尔瓦多", "NI": "尼加拉瓜",
}
