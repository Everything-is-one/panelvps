package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Everything-is-one/panelvps/internal/filemgr"
)

// 回收站：删除先进这里，误删可以原样放回。
//
// 保留期 7 天（启动时清理过期条目）；彻底删除必须显式传 confirm=DELETE。

func (s *Server) trashReady(w http.ResponseWriter) bool {
	if s.trash == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "回收站不可用：数据目录不可写")
		return false
	}
	return true
}

func (s *Server) handleTrashList(w http.ResponseWriter, r *http.Request) {
	if !s.trashReady(w) {
		return
	}
	items, err := s.trash.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取回收站失败："+err.Error())
		return
	}
	var total int64
	for _, it := range items {
		total += it.Size
	}
	ok(w, map[string]any{"items": items, "count": len(items), "bytes": total, "dir": s.trash.Dir()})
}

type trashIDsReq struct {
	IDs     []string `json:"ids"`
	Confirm string   `json:"confirm"`
}

func (s *Server) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	if !s.trashReady(w) || !s.filesReady(w) {
		return
	}
	var req trashIDsReq
	if err := decodeJSON(w, r, &req); err != nil || len(req.IDs) == 0 {
		fail(w, http.StatusBadRequest, codeBadRequest, "没有指定要恢复的条目")
		return
	}
	var errs []string
	for _, id := range req.IDs {
		item, err := s.trash.Restore(id, s.files.Root())
		if err != nil {
			if errors.Is(err, filemgr.ErrExists) {
				errs = append(errs, id+": 原位置已有同名文件，请先改名或移走后重试")
			} else {
				errs = append(errs, id+": "+err.Error())
			}
			continue
		}
		s.audit(r, "trash_restore", item.Original, "从回收站恢复")
	}
	if len(errs) > 0 {
		fail(w, http.StatusBadRequest, codeBadRequest, "部分恢复失败："+strings.Join(errs, "；"))
		return
	}
	ok(w, map[string]any{"restored": len(req.IDs)})
}

type trashDeleteReq struct {
	IDs     []string `json:"ids"`
	All     bool     `json:"all"`
	Confirm string   `json:"confirm"`
}

func (s *Server) handleTrashDelete(w http.ResponseWriter, r *http.Request) {
	if !s.trashReady(w) {
		return
	}
	var req trashDeleteReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式不正确")
		return
	}
	if req.Confirm != "DELETE" {
		fail(w, http.StatusBadRequest, codeBadRequest, "彻底删除需要二次确认（confirm=DELETE）")
		return
	}
	if !req.All && len(req.IDs) == 0 {
		fail(w, http.StatusBadRequest, codeBadRequest, "没有指定要清除的条目")
		return
	}
	if req.All {
		n, err := s.trash.Empty()
		if err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "清空回收站失败："+err.Error())
			return
		}
		s.audit(r, "trash_empty", "-", "清空回收站")
		ok(w, map[string]any{"purged": n})
		return
	}
	var errs []string
	n := 0
	for _, id := range req.IDs {
		if err := s.trash.Purge(id); err != nil {
			errs = append(errs, id+": "+err.Error())
			continue
		}
		n++
		s.audit(r, "trash_purge", id, "彻底删除")
	}
	if len(errs) > 0 {
		fail(w, http.StatusBadRequest, codeBadRequest, "部分清除失败："+strings.Join(errs, "；"))
		return
	}
	ok(w, map[string]any{"purged": n})
}
