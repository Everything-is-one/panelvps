package filemgr

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemDirs 是面板层直接拒绝删除的关键目录（精确匹配）。
//
// 只挡目录本身：/etc/nginx/conf.d/x.conf 这类子路径照常可以删，
// 用户日常维护配置不会被打断；会被误点成灾难的是「把整个目录删掉」。
var systemDirs = []string{
	"/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib32", "/lib64", "/libx32",
	"/lost+found", "/media", "/mnt", "/opt", "/proc", "/root", "/run", "/sbin",
	"/srv", "/sys", "/tmp", "/usr", "/var", "/var/lib", "/var/log", "/var/spool",
}

// Guard 拦截会伤到系统或面板自身的删除目标。
type Guard struct {
	extra []string
}

// NewGuard 创建删除守卫。extra 一般是面板的数据目录与配置目录——
// 把它们删了面板就起不来，而且面板跑在 root 下，拦不住就真删了。
func NewGuard(extra ...string) *Guard {
	clean := make([]string, 0, len(extra))
	for _, p := range extra {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			clean = append(clean, filepath.Clean(abs))
		}
	}
	return &Guard{extra: clean}
}

// Check 判断 abs 是否允许删除，不允许时返回原因。
func (g *Guard) Check(abs string) error {
	clean := filepath.Clean(abs)
	if clean == string(filepath.Separator) {
		return errors.New("拒绝删除文件系统根目录")
	}
	for _, p := range systemDirs {
		if clean == p {
			return fmt.Errorf("%s 是系统关键目录，面板拒绝删除；确实要删请用 SSH 操作", p)
		}
	}
	// 挂载点单列：rm -rf /mnt/data 会把挂在这儿的整块盘清空，
	// 这比删一个普通目录危险得多，普通用户很难意识到。
	if mp, ok := isMountPoint(clean); ok {
		return fmt.Errorf("%s 是挂载点（%s），删除会清空该分区上的全部数据，已拦截", clean, mp)
	}
	for _, p := range g.extra {
		if clean == p {
			return fmt.Errorf("%s 是面板自身的数据或配置目录，删除后面板将无法启动，已拦截", clean)
		}
	}
	return nil
}

// isMountPoint 读 /proc/mounts 判断是否为挂载点。非 Linux 上恒为 false。
func isMountPoint(p string) (string, bool) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		mp := unescapeMount(fields[1])
		if mp == p {
			return mp, true
		}
	}
	return "", false
}

// unescapeMount 还原 /proc/mounts 里的 \040 这类八进制转义。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			ok := true
			for j := 1; j <= 3; j++ {
				c := s[i+j]
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
