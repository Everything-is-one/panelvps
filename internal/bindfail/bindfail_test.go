package bindfail

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want Kind
	}{
		{
			name: "EACCES：调用方缺 CAP_NET_BIND_SERVICE（装 Nginx 的现场）",
			out: "nginx: the configuration file /etc/nginx/nginx.conf syntax is ok\n" +
				"nginx: [emerg] bind() to 0.0.0.0:80 failed (13: Permission denied)\n" +
				"nginx: configuration file /etc/nginx/nginx.conf test failed",
			want: PermDenied,
		},
		{
			name: "EADDRINUSE：nginx 自己已经占着 80",
			out:  "nginx: [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)",
			want: AddrInUse,
		},
		{
			name: "多个端口都是权限问题",
			out: "nginx: [emerg] bind() to 0.0.0.0:80 failed (13: Permission denied)\n" +
				"nginx: [emerg] bind() to [::]:80 failed (13: Permission denied)",
			want: PermDenied,
		},
		{
			name: "权限与占用并存：按权限处理",
			out: "nginx: [emerg] bind() to 0.0.0.0:80 failed (13: Permission denied)\n" +
				"nginx: [emerg] bind() to 0.0.0.0:443 failed (98: Address already in use)",
			want: MixedBind,
		},
		{
			name: "真正的语法错误",
			out:  "nginx: [emerg] unknown directive \"serverr\" in /etc/nginx/sites-enabled/a.conf:3",
			want: NotBind,
		},
		{
			name: "语法错误与端口错误并存：不能放行",
			out: "nginx: [emerg] bind() to 0.0.0.0:80 failed (13: Permission denied)\n" +
				"nginx: [emerg] unknown directive \"proxy_pas\" in /etc/nginx/sites-enabled/a.conf:9",
			want: NotBind,
		},
		{
			name: "地址本身不可用（EADDRNOTAVAIL）不是这两种",
			out:  "nginx: [emerg] bind() to 10.0.0.9:80 failed (99: Cannot assign requested address)",
			want: NotBind,
		},
		{
			name: "没有诊断行",
			out:  "",
			want: NotBind,
		},
		{
			name: "命令不存在，只有 shell 的错误",
			out:  "exit status 127",
			want: NotBind,
		},
	}
	for _, c := range cases {
		if got := Classify(c.out); got != c.want {
			t.Errorf("%s: 期望 %v，得到 %v", c.name, c.want, got)
		}
		if got := IsBindOnly(c.out); got != (c.want != NotBind) {
			t.Errorf("%s: IsBindOnly 期望 %v，得到 %v", c.name, c.want != NotBind, got)
		}
	}
}
