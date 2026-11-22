// Package bindfail 判断一条「打开监听套接字失败」的命令错误，是否只是
// 端口层面的问题，而非它真正想校验的东西（配置语法、可执行性）。
//
// 存在的理由：nginx -t 是个混合校验 —— 它既检查配置语法，也真的会
// bind() 一遍每个 listen 指令，于是把「端口能不能开」这个与配置无关的
// 问题混进了校验结果。而面板跑它的时候，条件往往不对：
//
//   - 面板是 nginx 的父进程，继承服务单元的能力边界集。缺
//     CAP_NET_BIND_SERVICE 时，内核在 inet_bind 里**先**查端口权限就返回
//     EACCES(13)，连「有没有被占用」都还没轮到。用户看到的现象是
//     「Nginx 明明装好了，面板却判定安装失败并回滚」。
//   - 反过来，nginx 已经跑起来时，80 被它自己占着，这里必然 EADDRINUSE(98)。
//     于是「新建站点」这种日常操作也会在校验环节失败。
//
// 两者性质不同，所以这里不解成布尔值：权限问题与调用方是谁有关，占用问题
// 要看「是不是它自己占的」。语法错误、重复 listen、这两种之外的任何
// [emerg] 一律照旧报错。
package bindfail

import "strings"

// Kind 是 bind 失败的归类。
type Kind int

const (
	// NotBind 表示失败与 bind 无关（语法错误、命令不存在等），照常报错。
	NotBind Kind = iota
	// PermDenied 表示全部是 EACCES：调用方没有 CAP_NET_BIND_SERVICE。
	// 与配置无关，换一个有权限的调用方就能通过。
	PermDenied
	// AddrInUse 表示全部是 EADDRINUSE：端口已被别人占着。
	// 在 nginx 已经运行时这属于预期内（占着的就是它自己）。
	AddrInUse
	// MixedBind 表示既有权限问题又有占用问题。权限都过不去，占用那条
	// 不足以单独判断，按权限问题处理。
	MixedBind
)

// Classify 解析命令输出。
//
// 只有所有 [emerg] / [alert] 行都命中 bind() 且原因是这两种 errno 时，
// 才判定为 bind 类失败；混入任何一条其它原因就返回 NotBind —— 宁可让
// 调用方报错，也不要把真正的配置问题吞掉。
func Classify(out string) Kind {
	var marks []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "[emerg]") || strings.Contains(line, "[alert]") {
			marks = append(marks, line)
		}
	}
	if len(marks) == 0 {
		// 没有可判定的诊断行（例如命令本身不存在），不做猜测。
		return NotBind
	}

	perm, inUse := false, false
	for _, line := range marks {
		if !strings.Contains(line, "bind() to") {
			return NotBind
		}
		l := strings.ToLower(line)
		switch {
		case strings.Contains(l, "permission denied"):
			perm = true
		case strings.Contains(l, "address already in use"):
			inUse = true
		default:
			return NotBind
		}
	}
	switch {
	case perm && inUse:
		return MixedBind
	case perm:
		return PermDenied
	default:
		return AddrInUse
	}
}

// IsBindOnly 是 Classify 的布尔简写：只要是 bind 类失败即为 true。
func IsBindOnly(out string) bool { return Classify(out) != NotBind }
