package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/version"
)

// 这个文件是「密码明明重置了却登不上」的诊断器。
//
// 症状：菜单/CLI 提示重置成功，Web 端仍然 401；服务日志里可能还留着一条
// 首次启动时打印的「初始密码」。
//
// 根因通常是两边读的不是同一个 SQLite 文件：
//   - 服务由 systemd 拉起，工作目录 /opt/panelvps，data_dir 解析成 /opt/panelvps/data；
//   - 人在 /root 里敲 panelvps，同一个 ./data 解析成 /root/data，
//     CLI 见库空就好心地建了一份新的，重置写的正是这份「影子库」。
//
// 0.4.3 起相对路径按配置文件所在的安装前缀解析（见 config.baseDirFor），
// 正常使用不会再分叉。这个文件负责把「到底谁的库是哪个」直接摆出来，
// 让人不必靠猜。

// knownSubcommands 与 main 里的 switch 保持一致，用来区分「服务进程」和
// 「一次性 CLI 调用」。
var knownSubcommands = map[string]bool{
	"menu": true, "serve": true, "version": true, "info": true,
	"init": true, "reset-password": true, "relay": true,
}

// runningService 描述正在运行的 panelvps serve 进程。
type runningService struct {
	PID    int
	Args   []string
	Cwd    string
	Config string
	DBs    []string
}

// findRunningService 在 /proc 里找 panelvps serve。找不到返回 nil。
//
// 不做 root 权限检查：读不到别人的 /proc/<pid> 时只是少几行信息，
// 命令本身仍然可用。
func findRunningService() *runningService {
	if runtime.GOOS != "linux" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()

	var found []*runningService
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(args) == 0 || filepath.Base(args[0]) != "panelvps" {
			continue
		}
		// 只认 serve 子命令：menu / relay / reset-password 都是一次性的。
		// 逐个比对已知子命令而不是「第一个非选项参数」——`-config <path>`
		// 的值本身也不带减号，按后者判断会把配置路径当成子命令。
		isServe := true
		for _, a := range args[1:] {
			if knownSubcommands[a] {
				isServe = a == "serve"
				break
			}
		}
		if !isServe {
			continue
		}

		svc := &runningService{PID: pid, Args: args}
		if cwd, err := os.Readlink("/proc/" + e.Name() + "/cwd"); err == nil {
			svc.Cwd = cwd
		}
		for i, a := range args {
			if (a == "-config" || a == "--config") && i+1 < len(args) {
				svc.Config = args[i+1]
			} else if strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config=") {
				svc.Config = a[strings.Index(a, "=")+1:]
			}
		}
		svc.DBs = openDBs(pid)
		found = append(found, svc)
	}
	if len(found) == 0 {
		return nil
	}
	// 正常情况下只有一个；多个时取 pid 最小（先起来的那个）之外的第一个，
	// 只为了输出稳定，不承诺语义。
	sort.Slice(found, func(i, j int) bool { return found[i].PID < found[j].PID })
	return found[0]
}

// openDBs 从 /proc/<pid>/fd 反查进程打开着的 SQLite 文件。
//
// 这是最硬的证据：不管配置写成什么样、工作目录在哪，进程真正 mmap 的是哪个
// 文件一目了然。WAL / SHM 是附属文件，归并成主库路径。
func openDBs(pid int) []string {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		name := target
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if strings.HasSuffix(name, suffix) {
				name = strings.TrimSuffix(name, suffix)
			}
		}
		if !strings.HasSuffix(name, ".db") || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// dbFact 是一个候选数据库的体检结果。
type dbFact struct {
	Path    string
	Users   int
	Size    int64
	ModTime time.Time
	Err     error
}

func inspectDB(path string) dbFact {
	fact := dbFact{Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		fact.Err = err
		return fact
	}
	fact.Size = fi.Size()
	fact.ModTime = fi.ModTime()

	st, err := store.Open(path)
	if err != nil {
		fact.Err = err
		return fact
	}
	defer st.Close()
	n, err := st.CountUsers(context.Background())
	if err != nil {
		fact.Err = err
		return fact
	}
	fact.Users = n
	return fact
}

// candidateDBs 列出可能被误用的数据库：配置里的、安装目录的、当前目录的、
// 家目录的。只返回真实存在的文件。
func candidateDBs(cfg *config.Config) []string {
	var cands []string
	add := func(p string) {
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return
		}
		cands = append(cands, abs)
	}

	if cfg != nil {
		add(cfg.DBPath)
	}
	const installed = "/opt/panelvps/data/panel.db"
	add(installed)
	if cwd, err := os.Getwd(); err == nil {
		add(filepath.Join(cwd, "data", "panel.db"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, "data", "panel.db"))
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(cands))
	for _, p := range cands {
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// runInfo 打印运行时路径与账号概况。
//
// 除了「配置说是什么」，更重要的是「服务实际在用哪个库」——前者是意图，
// 后者才是事实。两者不一致时直接给出结论，省掉一整轮猜。
func runInfo(cfgPath, dbOverride string) error {
	svc := findRunningService()

	// 配置可能读不到（比如救急时只关心 --db），这时降级为只报能报的。
	cfg, cfgErr := config.Load(cfgPath)

	fmt.Printf("版本      : %s (%s)\n", version.Version, version.Commit)
	fmt.Printf("配置文件  : %s\n", cfgPath)
	if cfgErr != nil {
		fmt.Printf("            （读取失败: %v）\n", cfgErr)
	} else {
		fmt.Printf("监听地址  : %s%s\n", cfg.Listen, map[bool]string{true: "  [仅本机]", false: "  [已对外]"}[cfg.IsLoopback()])
		fmt.Printf("数据目录  : %s\n", cfg.DataDir)
	}
	dbPath := dbOverride
	switch {
	case dbOverride != "":
		fmt.Printf("数据库    : %s  [--db 指定]\n", dbOverride)
	case cfgErr == nil:
		fmt.Printf("数据库    : %s\n", cfg.DBPath)
		dbPath = cfg.DBPath
	}
	if cfgErr == nil {
		fmt.Printf("会话超时  : %s\n", cfg.SessionTTL)
		fmt.Printf("文件根目录: %s\n", cfg.FileRoot)
		fmt.Printf("终端功能  : %v\n", cfg.TerminalEnabled)
	}

	fmt.Println()
	printService(svc)

	fmt.Println()
	printDatabases(cfg, svc, dbPath)

	return nil
}

func printService(svc *runningService) {
	if runtime.GOOS != "linux" {
		return
	}
	fmt.Println("运行中的服务")
	if svc == nil {
		fmt.Println("  [注意] 没有找到 panelvps serve 进程")
		if hasSystemd() {
			fmt.Println("         可执行: systemctl status panelvps --no-pager")
		}
		return
	}
	fmt.Printf("  进程    : pid %d  %s\n", svc.PID, strings.Join(svc.Args, " "))
	fmt.Printf("  工作目录: %s\n", orDash(svc.Cwd))
	fmt.Printf("  配置参数: %s\n", orDash(svc.Config))
	if len(svc.DBs) == 0 {
		fmt.Println("  实际打开: （读不到 /proc/<pid>/fd，可能需要 root）")
		return
	}
	for _, db := range svc.DBs {
		fmt.Printf("  实际打开: %s\n", db)
	}
}

func printDatabases(cfg *config.Config, svc *runningService, resolved string) {
	cands := candidateDBs(cfg)
	if len(cands) == 0 && resolved == "" {
		return
	}

	// 服务的库 > 配置解析出的库：前者是事实。
	truth := ""
	if svc != nil && len(svc.DBs) > 0 {
		truth = svc.DBs[0]
	}
	if truth == "" {
		truth = resolved
	}
	if truth != "" {
		if abs, err := filepath.Abs(truth); err == nil {
			truth = abs
		}
	}

	fmt.Println("数据库体检")
	if truth == "" {
		fmt.Println("  未找到可检查的数据库")
		return
	}
	if _, err := os.Stat(truth); err != nil {
		fmt.Printf("  %s 尚未创建，首次 serve 时会初始化\n", truth)
		return
	}

	// 把「事实库」排在最前面，读的人第一眼看到的就是真正在用的那个。
	if len(cands) > 1 {
		moved := make([]string, 0, len(cands))
		for _, p := range cands {
			if p == truth {
				moved = append(moved, p)
			}
		}
		for _, p := range cands {
			if p != truth {
				moved = append(moved, p)
			}
		}
		cands = moved
	}

	for _, p := range cands {
		fact := inspectDB(p)
		tag := "[其他]"
		switch {
		case p == truth && svc != nil:
			tag = "[在用]"
		case p == truth:
			tag = "[当前]"
		case resolved != "" && p == resolved:
			tag = "[配置]"
		}
		fmt.Printf("  %s %s\n", tag, p)
		if fact.Err != nil {
			fmt.Printf("         %v\n", fact.Err)
			continue
		}
		fmt.Printf("         %d 个账号 · %.0f KB · 修改于 %s\n",
			fact.Users, float64(fact.Size)/1024, fact.ModTime.Format("2006-01-02 15:04"))
	}

	// 结论：只在有分歧时才说话，正常的一行也不多。
	fmt.Println()
	switch {
	case resolved != "" && svc != nil && len(svc.DBs) > 0 && filepath.Clean(resolved) != filepath.Clean(svc.DBs[0]):
		fmt.Println("  [注意] 配置解析出的库和服务实际打开的库不是同一个：")
		fmt.Printf("         配置: %s\n", resolved)
		fmt.Printf("         服务: %s\n", svc.DBs[0])
		fmt.Println("         密码存在后者里，前者是影子库。CLI 请加 -db 指向服务那个库。")
	case resolved != "" && svc == nil:
		fmt.Println("  [ok]  没有运行中的服务；reset-password 会改上面这个库。")
	case resolved != "":
		fmt.Println("  [ok]  配置解析出的库就是服务在用的库，重置密码会生效。")
	}

	// 影子库一旦存在就会一直误导人，点名提示。
	for _, p := range cands {
		if p == truth {
			continue
		}
		fmt.Printf("  [建议] %s 不是服务在用的库，可改名或删除，避免再次改错（先把内容备份）。\n", p)
	}
}

func hasSystemd() bool {
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}
