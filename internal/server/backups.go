package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Everything-is-one/panelvps/internal/backup"
	"github.com/Everything-is-one/panelvps/internal/config"
	"github.com/Everything-is-one/panelvps/internal/version"
)

const backupKeep = 30

func (s *Server) backupsReady(w http.ResponseWriter) bool {
	if s.backups == nil {
		fail(w, http.StatusServiceUnavailable, codeInternalError, "备份模块不可用")
		return false
	}
	return true
}

func (s *Server) handleBackupList(w http.ResponseWriter, r *http.Request) {
	if !s.backupsReady(w) {
		return
	}
	files, err := s.backups.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取备份列表失败")
		return
	}
	ok(w, map[string]any{
		"items":    files,
		"dir":      s.backups.Dir,
		"keep":     backupKeep,
		"panel_db": s.cfg.DBPath,
	})
}

type backupReq struct {
	Note string `json:"note"`
}

// createBackup 生成一份含数据库一致性快照与配置文件的备份。
func (s *Server) createBackup(ctx context.Context, note string) (*backup.File, error) {
	if s.backups == nil {
		return nil, errors.New("备份模块不可用")
	}
	return s.backups.Create(ctx, note, func(ctx context.Context, dir string) error {
		if err := s.st.Snapshot(ctx, filepath.Join(dir, "panel.db")); err != nil {
			return err
		}
		if p := s.cfg.Path(); p != "" {
			if raw, err := os.ReadFile(p); err == nil {
				confDir := filepath.Join(dir, "conf")
				if err := os.MkdirAll(confDir, 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(confDir, "panel.yaml"), raw, 0o600); err != nil {
					return err
				}
			}
		}
		meta, _ := json.MarshalIndent(map[string]any{
			"version":    version.Version,
			"created_at": time.Now().Format(time.RFC3339),
			"db_path":    s.cfg.DBPath,
			"note":       note,
		}, "", "  ")
		return os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600)
	})
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	if !s.backupsReady(w) || s.jobs == nil {
		return
	}
	var req backupReq
	_ = decodeJSON(w, r, &req)

	job, err := s.jobs.Start("backup", "panel", func(ctx context.Context, log func(string)) error {
		log("正在生成数据库快照（VACUUM INTO）")
		f, err := s.createBackup(ctx, req.Note)
		if err != nil {
			return err
		}
		log("已生成 " + f.Name + "（" + humanBytes(f.Size) + "）")
		removed, _ := s.backups.Prune(backupKeep)
		if removed > 0 {
			log("按保留策略清理了 " + itoa(removed) + " 份旧备份")
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusTooManyRequests, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "backup_create", "panel", req.Note)
	ok(w, job)
}

func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	if !s.backupsReady(w) {
		return
	}
	name := r.PathValue("name")
	p, err := s.backups.Resolve(name)
	if err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fail(w, http.StatusNotFound, codeNotFound, "备份不存在")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "读取备份失败")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	if !s.backupsReady(w) {
		return
	}
	name := r.PathValue("name")
	if err := s.backups.Delete(name); err != nil {
		fail(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	s.audit(r, "backup_delete", name, "")
	ok(w, map[string]any{"ok": true})
}

// handleBackupRestore 恢复备份。
//
// 运行中的进程仍持有旧数据库句柄，因此必须「替换 + 重启」；
// 没有 systemd 时只做暂存，绝不制造新旧两个数据库同时可写的局面。
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if !s.backupsReady(w) {
		return
	}
	name := r.PathValue("name")
	var req confirmReq
	if err := decodeJSON(w, r, &req); err != nil || req.Confirm != "RESTORE" {
		fail(w, http.StatusBadRequest, codeBadRequest, "恢复需要二次确认（confirm=RESTORE）")
		return
	}

	// 1) 先给当前状态留一份安全备份
	if _, err := s.createBackup(r.Context(), "恢复 "+name+" 前的自动备份"); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "恢复前自动备份失败，已中止: "+err.Error())
		return
	}

	// 2) 展开到暂存目录
	staging, err := os.MkdirTemp(s.cfg.DataDir, ".restore-*")
	if err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "创建暂存目录失败")
		return
	}
	if err := s.backups.Extract(name, staging); err != nil {
		os.RemoveAll(staging)
		fail(w, http.StatusBadRequest, codeBadRequest, "展开备份失败: "+err.Error())
		return
	}
	dbSrc := filepath.Join(staging, "panel.db")
	if !backup.LooksLikeSQLite(dbSrc) {
		os.RemoveAll(staging)
		fail(w, http.StatusBadRequest, codeBadRequest, "备份中的 panel.db 不是有效的 SQLite 文件，已中止")
		return
	}

	if _, err := exec.LookPath("systemctl"); err != nil {
		ok(w, map[string]any{
			"staged":           true,
			"staging_dir":      staging,
			"restart_required": true,
			"message":          "当前环境没有 systemd，备份已展开到暂存目录。请手工替换 " + s.cfg.DBPath + " 并重启面板。",
		})
		s.audit(r, "backup_restore_staged", name, staging)
		return
	}

	// 3) 替换数据库与配置
	ts := time.Now().Format("20060102-150405")
	if err := os.MkdirAll(filepath.Dir(s.cfg.DBPath), 0o700); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "准备数据库目录失败")
		return
	}
	if _, err := os.Stat(s.cfg.DBPath); err == nil {
		if err := os.Rename(s.cfg.DBPath, s.cfg.DBPath+".pre-restore-"+ts); err != nil {
			fail(w, http.StatusInternalServerError, codeInternalError, "备份当前数据库失败: "+err.Error())
			return
		}
	}
	// 同目录 rename，避免跨文件系统复制
	if err := os.Rename(dbSrc, s.cfg.DBPath); err != nil {
		fail(w, http.StatusInternalServerError, codeInternalError, "替换数据库失败: "+err.Error())
		return
	}
	// WAL 残留会让新库读到旧数据，必须清掉
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(s.cfg.DBPath + suffix)
	}
	// 恢复完紧接着就是重启，所以配置必须先证明自己能解析。
	// 备份包里存的正是「当时那份配置」——如果它是坏的（0.4.8 之前改过
	// SSH 端口的机器就是这个状态），原样写回去会让面板重启后无限重启，
	// 而且数据库已经换过了，用户连回滚的参照都没了。
	confNote := ""
	if confSrc := filepath.Join(staging, "conf", "panel.yaml"); fileExists(confSrc) {
		if p := s.cfg.Path(); p != "" {
			if raw, err := os.ReadFile(confSrc); err == nil {
				fixed, notes, perr := config.Sanitize(raw)
				switch {
				case perr != nil:
					s.logger.Warn("备份内的配置文件无法解析，已跳过配置恢复，仅恢复数据库", "err", perr)
					confNote = "；备份里的配置文件无法解析，已跳过（沿用当前配置）"
				default:
					_ = os.WriteFile(p+".pre-restore-"+ts, fixed, 0o600)
					_ = os.WriteFile(p, fixed, 0o600)
					if len(notes) > 0 {
						confNote = "；备份里的配置文件已顺带修复"
					}
				}
			}
		}
	}
	os.RemoveAll(staging)
	s.audit(r, "backup_restore", name, "已替换数据库，等待重启")

	ok(w, map[string]any{
		"staged":           false,
		"restart_required": true,
		"message":          "数据库已替换，面板将在 2 秒后重启以加载新数据。" + confNote,
	})
	go func() {
		time.Sleep(2 * time.Second)
		if err := exec.Command("systemctl", "restart", "panelvps").Run(); err != nil {
			s.logger.Error("恢复后重启失败，请手动重启", "err", err)
		}
	}()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
