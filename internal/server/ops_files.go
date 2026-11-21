package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Everything-is-one/panelvps/internal/filemgr"
)

// filesReady 统一处理「文件模块不可用」。
func (s *Server) filesReady(w http.ResponseWriter) bool {
	if s.files == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError,
			"文件管理不可用：请检查配置里的 file_root 是否存在且可读")
		return false
	}
	return true
}

func fileErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, filemgr.ErrOutsideRoot):
		fail(w, http.StatusForbidden, codeBadRequest, "路径超出允许范围")
	case errors.Is(err, filemgr.ErrTooLarge):
		fail(w, http.StatusRequestEntityTooLarge, codeBadRequest, "文件超过大小限制")
	case errors.Is(err, filemgr.ErrExists):
		fail(w, http.StatusConflict, codeBadRequest, "目标已存在")
	case errors.Is(err, os.ErrNotExist):
		fail(w, http.StatusNotFound, codeNotFound, "文件不存在")
	case errors.Is(err, os.ErrPermission):
		fail(w, http.StatusForbidden, codeBadRequest, "没有权限访问该路径")
	default:
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
	}
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	p := r.URL.Query().Get("path")
	hidden := r.URL.Query().Get("hidden") == "1"
	items, err := s.files.List(p, hidden)
	if err != nil {
		fileErr(w, err)
		return
	}
	clean := strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "/")
	if clean == "." {
		clean = ""
	}
	parent := ""
	if clean != "" {
		parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(clean)))
		if parent == "." {
			parent = ""
		}
	}
	ok(w, map[string]any{
		"root":    s.files.Root(),
		"path":    clean,
		"parent":  parent,
		"entries": items,
	})
}

func (s *Server) handleFileStat(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	e, err := s.files.Stat(r.URL.Query().Get("path"))
	if err != nil {
		fileErr(w, err)
		return
	}
	ok(w, e)
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	p := r.URL.Query().Get("path")
	raw, err := s.files.ReadAll(p)
	if err != nil {
		fileErr(w, err)
		return
	}
	if len(raw) == 0 {
		ok(w, map[string]any{"path": p, "content": "", "size": 0, "binary": false})
		return
	}
	// 截断到前 2KB 判断是否为文本：包含 NUL 或非法 UTF-8 即视为二进制
	probe := raw
	if len(probe) > 2048 {
		probe = probe[:2048]
	}
	binary := bytesHaveNUL(probe) || !utf8.Valid(probe)
	resp := map[string]any{
		"path":   p,
		"size":   len(raw),
		"binary": binary,
	}
	if !binary {
		resp["content"] = string(raw)
	}
	ok(w, resp)
}

func bytesHaveNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

type fileWriteReq struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode"`
}

func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	var req fileWriteReq
	if err := decodeJSON(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "请求格式错误")
		return
	}
	if req.Path == "" {
		fail(w, http.StatusBadRequest, codeBadRequest, "缺少文件路径")
		return
	}
	mode := os.FileMode(0o644)
	if req.Mode == "755" {
		mode = 0o755
	}
	if err := s.files.WriteFile(req.Path, []byte(req.Content), mode); err != nil {
		fileErr(w, err)
		return
	}
	s.audit(r, "file_write", req.Path, "写入 "+strconv.Itoa(len(req.Content))+" 字节")
	ok(w, map[string]any{"size": len(req.Content)})
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	p := r.URL.Query().Get("path")
	f, fi, err := s.files.Open(p)
	if err != nil {
		fileErr(w, err)
		return
	}
	defer f.Close()
	name := filepath.Base(fi.Name())
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlEncode(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

func urlEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xf])
	}
	return b.String()
}

// handleFileBatchDownload 把勾选的多个文件/目录打成一个 tar.gz 直接下载。
// 用 GET 而不是 POST：浏览器只能对 GET 地址做「另存为」，
// 前端把每个 path 作为重复的 query 参数带上来即可。
func (s *Server) handleFileBatchDownload(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	paths := r.URL.Query()["path"]
	if len(paths) == 0 {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要至少选择一个文件")
		return
	}
	// 先校验再写响应头：一旦开始写 tar 流，中途出错就只能给一个截断的文件
	if err := s.files.CheckPack(paths); err != nil {
		fileErr(w, err)
		return
	}
	name := "panelvps-" + time.Now().Format("20060102-150405") + ".tar.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlEncode(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.files.WriteArchive(w, paths); err != nil {
		s.logger.Error("打包下载中断", "err", err, "count", len(paths))
		return
	}
	s.audit(r, "file_batch_download", name, strings.Join(paths, ","))
}

// handleFileUpload 接收 multipart 上传，逐文件流式落盘。
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	dir := r.URL.Query().Get("path")
	const maxUpload = 512 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)

	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要 multipart/form-data 上传")
		return
	}

	type saved struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	var out []saved
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fail(w, http.StatusBadRequest, codeBadRequest, "读取上传内容失败: "+err.Error())
			return
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		if name == "" || name == "." || name == ".." {
			part.Close()
			continue
		}
		target := name
		if dir != "" {
			target = strings.TrimSuffix(dir, "/") + "/" + name
		}
		n, err := s.files.WriteStream(target, part, 0o644)
		part.Close()
		if err != nil {
			fileErr(w, err)
			return
		}
		out = append(out, saved{Name: name, Path: target, Size: n})
		s.audit(r, "file_upload", target, strconv.FormatInt(n, 10)+" 字节")
	}
	if len(out) == 0 {
		fail(w, http.StatusBadRequest, codeBadRequest, "没有收到任何文件")
		return
	}
	ok(w, map[string]any{"files": out})
}

type filePathReq struct {
	Path string `json:"path"`
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	var req filePathReq
	if err := decodeJSON(w, r, &req); err != nil || req.Path == "" {
		fail(w, http.StatusBadRequest, codeBadRequest, "缺少目录路径")
		return
	}
	if err := s.files.Mkdir(req.Path); err != nil {
		fileErr(w, err)
		return
	}
	s.audit(r, "file_mkdir", req.Path, "")
	ok(w, map[string]any{"ok": true})
}

type fileRenameReq struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
}

func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	var req fileRenameReq
	if err := decodeJSON(w, r, &req); err != nil || req.From == "" || req.To == "" {
		fail(w, http.StatusBadRequest, codeBadRequest, "缺少源路径或目标路径")
		return
	}
	if err := s.files.Rename(req.From, req.To, req.Overwrite); err != nil {
		fileErr(w, err)
		return
	}
	s.audit(r, "file_rename", req.From, "→ "+req.To)
	ok(w, map[string]any{"ok": true})
}

func (s *Server) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	var req fileRenameReq
	if err := decodeJSON(w, r, &req); err != nil || req.From == "" || req.To == "" {
		fail(w, http.StatusBadRequest, codeBadRequest, "缺少源路径或目标路径")
		return
	}
	if err := s.files.Copy(req.From, req.To, req.Overwrite); err != nil {
		fileErr(w, err)
		return
	}
	s.audit(r, "file_copy", req.From, "→ "+req.To)
	ok(w, map[string]any{"ok": true})
}

type fileDeleteReq struct {
	Paths     []string `json:"paths"`
	Confirm   string   `json:"confirm"`
	Permanent bool     `json:"permanent"`
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	var req fileDeleteReq
	if err := decodeJSON(w, r, &req); err != nil || len(req.Paths) == 0 {
		fail(w, http.StatusBadRequest, codeBadRequest, "没有指定要删除的路径")
		return
	}
	if req.Confirm != "DELETE" {
		fail(w, http.StatusBadRequest, codeBadRequest, "删除需要二次确认（confirm=DELETE）")
		return
	}
	if !req.Permanent && s.trash == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError,
			"回收站不可用（数据目录写不进去）。确认要永久删除时，请勾选「彻底删除」再试")
		return
	}

	var errs []string
	trashed, deleted := 0, 0
	for _, p := range req.Paths {
		abs, err := s.files.Resolve(p)
		if err != nil {
			errs = append(errs, p+": "+err.Error())
			continue
		}
		// 系统目录、挂载点、面板自身数据目录一律拦在服务端
		if s.guard != nil {
			if err := s.guard.Check(abs); err != nil {
				errs = append(errs, p+": "+err.Error())
				continue
			}
		}
		if req.Permanent {
			if err := s.files.Remove(p); err != nil {
				errs = append(errs, p+": "+err.Error())
				continue
			}
			deleted++
			s.audit(r, "file_delete", p, "彻底删除")
			continue
		}
		if _, err := s.trash.Put(abs, p); err != nil {
			errs = append(errs, p+": "+err.Error())
			continue
		}
		trashed++
		s.audit(r, "file_trash", p, "移入回收站")
	}
	if len(errs) > 0 {
		fail(w, http.StatusBadRequest, codeBadRequest, "部分删除失败："+strings.Join(errs, "；"))
		return
	}
	ok(w, map[string]any{"trashed": trashed, "deleted": deleted})
}

type archiveReq struct {
	Paths   []string `json:"paths"`
	Dest    string   `json:"dest"`
	Archive string   `json:"archive"`
	Dir     string   `json:"dir"`
}

func (s *Server) handleFileArchive(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	var req archiveReq
	if err := decodeJSON(w, r, &req); err != nil || len(req.Paths) == 0 || req.Dest == "" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要选择文件并指定压缩包路径")
		return
	}
	if !strings.HasSuffix(strings.ToLower(req.Dest), ".tar.gz") {
		fail(w, http.StatusBadRequest, codeBadRequest, "压缩包名需以 .tar.gz 结尾")
		return
	}
	if err := s.files.Archive(req.Paths, req.Dest); err != nil {
		fileErr(w, err)
		return
	}
	s.audit(r, "file_archive", req.Dest, strings.Join(req.Paths, ","))
	ok(w, map[string]any{"ok": true})
}

func (s *Server) handleFileExtract(w http.ResponseWriter, r *http.Request) {
	if !s.filesReady(w) {
		return
	}
	var req archiveReq
	if err := decodeJSON(w, r, &req); err != nil || req.Archive == "" || req.Dir == "" {
		fail(w, http.StatusBadRequest, codeBadRequest, "需要指定压缩包与目标目录")
		return
	}
	if err := s.files.Extract(req.Archive, req.Dir); err != nil {
		fileErr(w, err)
		return
	}
	s.audit(r, "file_extract", req.Archive, "→ "+req.Dir)
	ok(w, map[string]any{"ok": true})
}
