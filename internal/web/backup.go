package web

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chinamobile-monitor/internal/mobile"
	"chinamobile-monitor/internal/store"
)

// 备份 zip 结构：
//
//	backup.json        元信息（应用/版本/时间）
//	store.json         账号列表、设置、面板用户、查询历史
//	accounts/<手机号>/user-data/...   Chromium 登录态（导入后无需重新登录）

const backupMaxSize = 300 << 20 // 上传大小上限 300MB

// apiBackupExport GET /api/backup/export
// 浏览器直接下载链接（cookie 自动携带），无 CSRF 风险（GET 无副作用）
func (s *Server) apiBackupExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.apiFail(w, "方法不允许")
		return
	}
	if !s.st.HasUser() || s.getSession(r) == nil {
		s.apiFail(w, "未登录")
		return
	}
	_ = s.st.Save() // 确保最新数据落盘

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=chinamobile-backup-%s.zip", time.Now().Format("20060102-150405")))

	zw := zip.NewWriter(w)
	defer zw.Close()

	dir := s.dataDir()
	// 元信息
	if f, err := zw.Create("backup.json"); err == nil {
		meta, _ := json.Marshal(map[string]interface{}{
			"app":        "chinamobile-monitor",
			"backup_ver": 1,
			"created_at": time.Now().Format(time.RFC3339),
		})
		_, _ = f.Write(meta)
	}

	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// 排除日志、临时目录、导入安全备份
		if rel == "logs" || strings.HasPrefix(rel, "logs/") ||
			strings.HasPrefix(rel, ".") || strings.HasPrefix(rel, "pre-import-") {
			return nil
		}
		zf, err := zw.Create(rel)
		if err != nil {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		_, _ = io.Copy(zf, f)
		return nil
	})

	s.log.Info("备份已导出（IP %s）", clientIP(r))
}

// apiBackupImport POST /api/backup/import（multipart：file 字段）
func (s *Server) apiBackupImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if err := r.ParseMultipartForm(backupMaxSize); err != nil {
		s.apiFail(w, "上传文件解析失败（大小不超过 300MB）")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		s.apiFail(w, "未找到上传文件")
		return
	}
	defer file.Close()

	// 保存到临时文件（zip 需要随机访问）
	tmp, err := os.CreateTemp(s.dataDir(), ".import-*.zip")
	if err != nil {
		s.apiFail(w, "创建临时文件失败")
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		s.apiFail(w, "读取上传文件失败")
		return
	}
	tmp.Close()

	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		s.apiFail(w, "不是有效的备份文件（zip）")
		return
	}
	defer zr.Close()

	// 校验：必须包含 store.json 且为合法 JSON
	var storeEntry *zip.File
	for _, f := range zr.File {
		if filepath.ToSlash(filepath.Clean(f.Name)) == "store.json" {
			storeEntry = f
			break
		}
	}
	if storeEntry == nil {
		s.apiFail(w, "备份文件中缺少 store.json，请确认是本系统导出的备份")
		return
	}
	if rc, err := storeEntry.Open(); err == nil {
		var probe map[string]interface{}
		err := json.NewDecoder(rc).Decode(&probe)
		rc.Close()
		if err != nil {
			s.apiFail(w, "备份中的 store.json 无法解析")
			return
		}
	} else {
		s.apiFail(w, "备份中的 store.json 无法读取")
		return
	}

	// 停掉进行中的登录流程（释放浏览器 profile 文件锁）
	s.cancelActiveFlow()
	// 清理所有残留浏览器进程（异常退出留下的，锁住 user-data 目录）
	mobile.KillStaleBrowsers(s.dataDir())

	dir := s.dataDir()

	// 先解压到临时目录，全部成功后再替换（失败不影响现有数据）
	tmpDir := filepath.Join(dir, ".import-tmp")
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		s.apiFail(w, "创建临时目录失败")
		return
	}
	defer os.RemoveAll(tmpDir)

	importedAccounts := 0
	seenPhones := map[string]bool{}
	for _, f := range zr.File {
		name, ok := safeZipName(f.Name)
		if !ok {
			s.apiFail(w, "备份包含非法路径: "+f.Name)
			return
		}
		if name == "backup.json" {
			continue
		}
		if strings.HasPrefix(name, "accounts/") {
			rest := strings.TrimPrefix(name, "accounts/")
			if phone := strings.SplitN(rest, "/", 2)[0]; phone != "" {
				seenPhones[phone] = true
			}
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(filepath.Join(tmpDir, filepath.FromSlash(name)), 0o755)
			continue
		}
		dst := filepath.Join(tmpDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			s.apiFail(w, "解压失败: "+err.Error())
			return
		}
		rc, err := f.Open()
		if err != nil {
			s.apiFail(w, "解压失败: "+err.Error())
			return
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			s.apiFail(w, "解压失败: "+err.Error())
			return
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			s.apiFail(w, "解压失败: "+err.Error())
			return
		}
	}
	if len(seenPhones) > 0 {
		importedAccounts = len(seenPhones)
	}

	// 导入前安全备份现有数据
	pre, err := s.snapshotTo(filepath.Join(dir, fmt.Sprintf("pre-import-%s.zip", time.Now().Format("20060102-150405"))))
	if err != nil {
		s.apiFail(w, "导入前安全备份失败: "+err.Error())
		return
	}
	_ = pre

	// 替换：旧数据 → 新数据
	if err := os.RemoveAll(filepath.Join(dir, "accounts")); err != nil {
		s.apiFail(w, "替换账号目录失败（可能有浏览器进程占用，请稍后重试）: "+err.Error())
		return
	}
	if err := moveAll(filepath.Join(tmpDir, "accounts"), filepath.Join(dir, "accounts")); err != nil {
		s.apiFail(w, "移动账号目录失败: "+err.Error())
		return
	}
	if src := filepath.Join(tmpDir, "store.json"); pathExists(src) {
		b, err := os.ReadFile(src)
		if err != nil || os.WriteFile(filepath.Join(dir, "store.json"), b, 0o600) != nil {
			s.apiFail(w, "写入 store.json 失败")
			return
		}
	}

	// 重载配置（原地更新）+ 同步登录态标志
	if err := s.st.Reload(); err != nil {
		s.apiFail(w, "导入数据已写入但重载失败，请重启服务: "+err.Error())
		return
	}
	for _, a := range s.st.ListAccounts() {
		hasDir := pathExists(store.UserDataDir(dir, a.Phone))
		s.st.UpdateAccount(a.Phone, func(x *store.Account) { x.HasLoginState = hasDir })
	}

	// 清空全部会话，强制重新登录（导入的管理员凭据可能与当前不同）
	s.sessMu.Lock()
	s.sessions = map[string]*session{}
	s.sessMu.Unlock()

	s.log.Info("备份导入成功（账号 %d 个）", importedAccounts)
	s.apiOK(w, map[string]interface{}{"accounts": importedAccounts})
}

// cancelActiveFlow 取消进行中的登录流程并等待浏览器退出（释放 profile 文件锁）
func (s *Server) cancelActiveFlow() {
	s.flows.CancelAndWait()
}

// safeZipName 防 zip slip：返回归一化相对路径，非法路径返回 false
func safeZipName(name string) (string, bool) {
	name = strings.TrimPrefix(name, "/")
	name = filepath.ToSlash(filepath.Clean(name))
	if name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
		return "", false
	}
	if strings.ContainsAny(name, ":") || strings.HasPrefix(name, `\`) {
		return "", false
	}
	return name, true
}

// snapshotTo 把当前 store.json + accounts 打包到指定 zip（导入前安全备份）
func (s *Server) snapshotTo(dst string) (string, error) {
	_ = s.st.Save()
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	dir := s.dataDir()
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "logs" || strings.HasPrefix(rel, "logs/") ||
			strings.HasPrefix(rel, ".") || strings.HasPrefix(rel, "pre-import-") {
			return nil
		}
		zf, err := zw.Create(rel)
		if err != nil {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer src.Close()
		_, _ = io.Copy(zf, src)
		return nil
	})
	if err := zw.Close(); err != nil {
		return "", err
	}
	return dst, nil
}

// moveAll 移动整个目录（跨目录 rename 失败时逐文件复制）
func moveAll(src, dst string) error {
	if !pathExists(src) {
		return nil
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// rename 失败（跨设备等）→ 逐项复制
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
