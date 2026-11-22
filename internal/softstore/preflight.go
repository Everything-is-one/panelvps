package softstore

import (
	"fmt"
	"os"
	"path"
	"strings"
)

// preflight 在动手之前，把计划会写入的目录挨个探一遍。
//
// 起因是一次真实的安装失败：Xray 装到第 3 步 mkdir /usr/local/share/xray
// 报 read-only file system，随后回滚又撞在同一面墙上。根因在 systemd：
// 面板跑在 ProtectSystem=full 沙箱里，/usr 与 /etc 默认只读，只有
// ReadWritePaths 白名单中的路径可写；而 systemd 对白名单里**不存在**的
// 路径是静默跳过的 —— 目录没建，bind mount 就不会发生，面板再往里
// mkdir 自然是只读。
//
// 白名单写父目录（/usr/local/share）可以从根上避免这件事，但清单以后
// 还可能往别处写。这里先探一遍：能建就顺手建出来（等于自愈），不能建
// 就在下载 16MB 安装包之前失败，并直接告诉用户怎么修。
func preflight(plan []Action, log func(string)) error {
	targets := planTargets(plan)
	if len(targets) == 0 {
		return nil
	}
	log("检查安装目录：" + strings.Join(targets, "、"))
	for _, dir := range targets {
		if err := ensureWritable(dir); err != nil {
			return fmt.Errorf("目录 %s 不可写（%v）。原因通常是这两条之一："+
				"① 服务单元还是旧版本，没放开该目录——到「设置 → 服务单元」用内置版本覆盖后重启面板；"+
				"② 目录在面板启动时不存在，systemd 会静默跳过 ReadWritePaths 里的路径——"+
				"在 SSH 或商家控制台执行 mkdir -p %s 再 systemctl restart panelvps。"+
				"（别用面板自带的 Web 终端：它是面板的子进程，继承同样的只读视图。）",
				dir, err, dir)
		}
	}
	return nil
}

// planTargets 列出计划会写入的目录，去重并保持出现顺序。
func planTargets(plan []Action) []string {
	seen := make(map[string]bool, len(plan))
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		p = path.Clean(p)
		if p == "." || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, a := range plan {
		switch {
		case a.Download != nil:
			add(path.Dir(a.Download.Dest))
		case a.Extract != nil:
			add(a.Extract.Dest)
		case a.WriteFile != nil:
			add(path.Dir(a.WriteFile.Path))
		case a.Path != nil:
			// 只探要创建的目录；absent 是删除，不在这一步制造它
			if a.Path.State != "absent" {
				add(a.Path.Path)
			}
		}
	}
	return out
}

// ensureWritable 确认目录存在且真的能落一个文件。
//
// 只看 MkdirAll 的返回值不够：对「已存在但只读」的目录，MkdirAll 也返回
// nil（备份模块踩过同一个坑）。所以创建完再写一个临时文件探一下。
func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pv-write-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}
