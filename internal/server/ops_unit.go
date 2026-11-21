package server

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Everything-is-one/panelvps/deploy"
)

const unitName = "panelvps.service"

// unitStatus 描述磁盘上的服务单元与二进制内置模板是否一致。
type unitStatus struct {
	Path      string `json:"path"`
	Present   bool   `json:"present"`
	Match     bool   `json:"match"`
	Writable  bool   `json:"writable"`
	Reason    string `json:"reason,omitempty"`
	CheckedAt string `json:"checked_at"`
}

// unitPath 取 systemd 实际加载的单元文件路径。
func unitPath() string {
	if out, err := exec.Command("systemctl", "show", unitName, "-p", "FragmentPath", "--value").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	return "/etc/systemd/system/" + unitName
}

// normalizeUnit 抹平换行与行尾空白的差异，只比语义。
func normalizeUnit(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func inspectUnit() unitStatus {
	return inspectUnitAt(unitPath())
}

// inspectUnitAt 把路径作为参数，方便测试。
func inspectUnitAt(p string) unitStatus {
	st := unitStatus{Path: p, CheckedAt: time.Now().Format(time.RFC3339)}
	raw, err := os.ReadFile(p)
	if err == nil {
		st.Present = true
		st.Match = normalizeUnit(string(raw)) == normalizeUnit(deploy.ServiceUnit)
	} else if !os.IsNotExist(err) {
		st.Reason = "读取失败：" + err.Error()
	}

	if st.Present {
		// 用 O_WRONLY 打开不会截断，只探测写权限；root 下普通文件都能过，
		// 只读挂载才会返回 EROFS。
		if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			_ = f.Close()
			st.Writable = true
		}
	} else if f, err := os.CreateTemp(filepath.Dir(p), ".pv-unit-probe-*"); err == nil {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		st.Writable = true
	}

	switch {
	case !st.Present && st.Reason == "":
		st.Reason = "磁盘上没有这个单元文件，面板可能是在前台手工跑的"
	case st.Present && !st.Match && st.Reason == "":
		st.Reason = "与当前版本内置的单元不一致（旧版本升级后常见）"
	}
	return st
}

func (s *Server) handleSystemUnit(w http.ResponseWriter, r *http.Request) {
	ok(w, inspectUnit())
}

// handleSystemUnitSync 用内置模板覆盖磁盘上的单元。
//
// 只写文件 + daemon-reload，不重启自己：重启会把当前请求一起掐掉，
// 由前端提示用户点「重启面板」。
func (s *Server) handleSystemUnitSync(w http.ResponseWriter, r *http.Request) {
	st := inspectUnit()
	if st.Present && st.Match {
		ok(w, map[string]any{"changed": false, "restart_required": false, "status": st})
		return
	}
	if !st.Writable {
		fail(w, http.StatusForbidden, codeBadRequest,
			"服务单元不可写："+st.Path+"。面板运行在 systemd 沙箱里，只能写 ReadWritePaths 放行的目录；"+
				"请在 SSH 或商家控制台里重跑安装脚本，不要用面板自带的 Web 终端（它继承同样的只读视图）")
		return
	}

	backup := ""
	if st.Present {
		raw, err := os.ReadFile(st.Path)
		if err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "读取旧单元失败："+err.Error())
			return
		}
		backup = st.Path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, raw, 0o644); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "备份旧单元失败："+err.Error())
			return
		}
	}
	if err := os.WriteFile(st.Path, []byte(deploy.ServiceUnit), 0o644); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "写入服务单元失败："+err.Error())
		return
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError,
			"单元已写入，但 daemon-reload 失败："+strings.TrimSpace(string(out))+"；请在 SSH 里手工执行 systemctl daemon-reload")
		return
	}
	s.audit(r, "unit_sync", st.Path, "同步服务单元")
	ok(w, map[string]any{
		"changed":          true,
		"backup":           backup,
		"restart_required": true,
		"status":           inspectUnit(),
	})
}
