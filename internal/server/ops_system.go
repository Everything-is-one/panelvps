package server

import (
	"net/http"
	"os/exec"
	"strconv"
	"time"

	"github.com/Everything-is-one/panelvps/internal/metrics"
	"github.com/Everything-is-one/panelvps/internal/store"
	"github.com/Everything-is-one/panelvps/internal/sysinfo"
	"github.com/Everything-is-one/panelvps/internal/version"
)

// handleSystemInfo 返回主机概况与面板运行参数。
func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{
		"os":       sysinfo.Info(),
		"cgroup":   sysinfo.CgroupVersion(),
		"timezone": systemTimezone(),
		"metrics":  s.metrics.Collect(),
		"panel": map[string]any{
			"version":          version.Version,
			"listen":           s.cfg.Listen,
			"loopback":         s.cfg.IsLoopback(),
			"data_dir":         s.cfg.DataDir,
			"db_path":          s.cfg.DBPath,
			"file_root":        s.cfg.FileRoot,
			"sites_root":       s.cfg.SitesRoot,
			"backup_dir":       s.cfg.BackupDir,
			"terminal_enabled": s.cfg.TerminalEnabled,
			"files_available":  s.files != nil,
			"update_repo":      s.cfg.UpdateRepo,
		},
	})
}

func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	procs, err := sysinfo.Processes(limit)
	if err != nil {
		ok(w, map[string]any{"supported": false, "reason": err.Error(), "items": []any{}})
		return
	}
	ok(w, map[string]any{"supported": true, "items": procs})
}

// handleSystemTop 给概览页的 CPU / 内存 / 磁盘卡片提供 hover 排行。
// 单独开接口而不是塞进 /system/stat：CPU 排行要做两次采样，代价比
// 一次普通指标采集高一个数量级，只在鼠标真的放上去时才值得付。
func (s *Server) handleSystemTop(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	limit := 5
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 20 {
			limit = n
		}
	}
	switch kind {
	case "cpu", "mem":
		items, err := s.metrics.TopProcesses(kind, limit)
		if err != nil {
			ok(w, map[string]any{"supported": false, "reason": err.Error(), "items": []any{}})
			return
		}
		ok(w, map[string]any{"supported": true, "kind": kind, "items": items})
	case "disk":
		items, err := metrics.TopMounts(limit)
		if err != nil {
			ok(w, map[string]any{"supported": false, "reason": err.Error(), "items": []any{}})
			return
		}
		ok(w, map[string]any{"supported": true, "kind": kind, "items": items})
	default:
		fail(w, http.StatusBadRequest, codeBadRequest, "kind 只能是 cpu / mem / disk")
	}
}

// handleDiskUsage 是磁盘占用分析的入口。
//
// 一次扫描可能要遍历几十万个文件，远超一个 HTTP 请求该等的时间，
// 所以这里只做两件事：没有缓存就开始扫（立刻返回 running），
// 有缓存就返回结果。前端拿 running 时按 700ms 轮询同一地址。
func (s *Server) handleDiskUsage(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	refresh := r.URL.Query().Get("refresh") == "1"
	if s.disk == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "磁盘分析不可用")
		return
	}
	st, err := s.disk.Start(path, refresh)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ok(w, st)
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	items, err := sysinfo.Services()
	if err != nil {
		ok(w, map[string]any{"supported": false, "reason": err.Error(), "items": []any{}})
		return
	}
	ok(w, map[string]any{"supported": true, "items": items})
}

func (s *Server) handleServiceAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	action := r.PathValue("action")
	out, err := sysinfo.ServiceAction(name, action)
	s.audit(r, "service_"+action, name, store.TrimDetail(out))
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"output": out})
}

func (s *Server) handlePorts(w http.ResponseWriter, r *http.Request) {
	items, err := sysinfo.ListeningPorts()
	if err != nil {
		ok(w, map[string]any{"supported": false, "reason": err.Error(), "items": []any{}})
		return
	}
	ifaces, _ := sysinfo.Interfaces()
	ok(w, map[string]any{"supported": true, "items": items, "interfaces": ifaces})
}

type confirmReq struct {
	Confirm string `json:"confirm"`
}

// handleRestart 重启面板自身。先写响应再触发，避免把请求本身杀掉。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	var req confirmReq
	if err := decodeJSON(w, r, &req); err != nil || req.Confirm != "RESTART" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要提交 confirm=RESTART 才能重启面板")
		return
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest,
			"当前环境没有 systemd，无法自重启；请手动重启进程")
		return
	}
	s.audit(r, "panel_restart", "panel", "用户触发重启")
	ok(w, map[string]any{"restarting": true})
	s.restartPanelSoon(1200 * time.Millisecond)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		ok(w, map[string]any{"items": []any{}})
		return
	}
	ok(w, map[string]any{"items": s.jobs.List()})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		fail(w, http.StatusNotFound, codeNotFound, "任务不存在")
		return
	}
	j, ok2 := s.jobs.Get(r.PathValue("id"))
	if !ok2 {
		fail(w, http.StatusNotFound, codeNotFound, "任务不存在")
		return
	}
	ok(w, j)
}
