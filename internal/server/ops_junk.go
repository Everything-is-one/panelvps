package server

import (
	"fmt"
	"net/http"
	"strings"
)

// handleJunkScan 扫描磁盘垃圾。
//
// 与磁盘占用分析同一套节奏：一次扫描要遍历若干目录树（/root/.cache 可能有
// 几十万个小文件），HTTP 请求只负责开始与看进度，前端拿 running 就轮询。
func (s *Server) handleJunkScan(w http.ResponseWriter, r *http.Request) {
	if s.junkScan == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "清理模块不可用")
		return
	}
	ok(w, s.junkScan.Start(r.URL.Query().Get("refresh") == "1"))
}

// handleJunkClean 按类别键清理。
//
// 这是不可逆操作，与日志清理、重启面板同一套约定：要求 confirm=CLEAN。
// 请求里只有类别键，没有任何路径——路径由 internal/junk 的白名单决定。
func (s *Server) handleJunkClean(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys    []string `json:"keys"`
		Confirm string   `json:"confirm"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.Confirm != "CLEAN" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要提交 confirm=CLEAN 才能清理")
		return
	}
	if s.junkScan == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "清理模块不可用")
		return
	}
	rep, err := s.junkScan.Clean(req.Keys)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "junk_clean", strings.Join(req.Keys, ","),
		fmt.Sprintf("释放 %d 字节，%d 个文件", rep.Freed, rep.Files))
	ok(w, rep)
}
