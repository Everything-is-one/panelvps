package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Everything-is-one/panelvps/internal/logs"
)

func (s *Server) handleLogSources(w http.ResponseWriter, r *http.Request) {
	items, err := logs.Sources(s.cfg.DataDir)
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取日志来源失败")
		return
	}
	// journal 的落盘量是共用的，只能整站给一个数：界面上用它替代
	// 「这个来源多大」，也让清理按钮知道现在压到多少合适。
	ok(w, map[string]any{"items": items, "journal_bytes": logs.JournalUsage()})
}

// handleLogClean 清空一个来源。
//
// 这是不可逆操作，所以要求 confirm=CLEAN：与重启面板同一套约定，
// 让误点一次按钮不至于把日志抹掉。
func (s *Server) handleLogClean(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source   string `json:"source"`
		Confirm  string `json:"confirm"`
		TargetMB int    `json:"target_mb"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.Confirm != "CLEAN" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要提交 confirm=CLEAN 才能清理日志")
		return
	}
	if req.TargetMB < 0 || req.TargetMB > 10240 {
		fail(w, http.StatusBadRequest, codeBadRequest, "保留目标应在 0–10240 MB 之间")
		return
	}
	res, err := logs.Clean(req.Source, logs.Options{DataDir: s.cfg.DataDir}, int64(req.TargetMB)<<20)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "log_clean", res.Source,
		fmt.Sprintf("%s: %d -> %d 字节", res.Kind, res.Before, res.After))
	ok(w, res)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "panel"
	}
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			lines = n
		}
	}
	res, err := logs.Read(source, logs.Options{DataDir: s.cfg.DataDir, Lines: lines})
	if err != nil {
		if errors.Is(err, logs.ErrUnknownSource) {
			fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ok(w, res)
}
