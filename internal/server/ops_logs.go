package server

import (
	"errors"
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
	ok(w, map[string]any{"items": items})
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
