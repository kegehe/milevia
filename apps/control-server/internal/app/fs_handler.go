package app

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// registerFSRoutes 添加文件系统 API 端点到路由。
func (s *Server) registerFSRoutes(r chi.Router) {
	r.Route("/api/projects/{projectID}/fs", func(r chi.Router) {
		// 读取操作（无需 workspace lease）
		r.Get("/tree", s.fsReadDir)
		r.Get("/read", s.fsReadFile)
		r.Get("/stat", s.fsStat)
		r.Get("/raw", s.fsRawFile)
		r.Get("/search", s.fsSearch)

		// 写入操作（需 workspace lease）
		r.Put("/write", s.fsWriteFile)
		r.Post("/mkdir", s.fsMkdir)
		r.Delete("/remove", s.fsRemove)
		r.Post("/rename", s.fsRename)
	})
}

// getFilesystem 根据项目的 runner 类型返回对应的 Filesystem 实现。
func (s *Server) getFilesystem(r *http.Request) (Filesystem, error) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		return nil, fmt.Errorf("项目不存在：%w", err)
	}

	if isLocalRunnerID(project.Runner) {
		return &LocalFilesystem{
			allowedRoot: s.config.AllowedRoot,
			projectPath: project.Path,
		}, nil
	}

	// SSH runner
	runner, ok := s.runnerRegistry.get(project.Runner)
	if !ok {
		return nil, &runnerOfflineError{RunnerID: project.Runner}
	}
	sshR, ok := runner.(*sshRunner)
	if !ok {
		return nil, errors.New("runner 不是 SSH 类型")
	}
	fs := &SFTPFilesystem{
		client:   sshR.client,
		rootPath: project.Path,
	}
	rootPath, err := fs.canonicalRoot(r.Context())
	if err != nil {
		return nil, err
	}
	fs.rootPath = rootPath
	return fs, nil
}

// runnerOfflineError 表示 SSH runner 离线。
type runnerOfflineError struct {
	RunnerID string
}

func (e *runnerOfflineError) Error() string {
	return "runner 离线"
}

// ─── 读取操作 ───────────────────────────────────────────────────────────────

func (s *Server) fsReadDir(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	entries, err := fs.ReadDir(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if entries == nil {
		entries = []FileEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) fsReadFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 参数必填"))
		return
	}
	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	content, err := fs.ReadFile(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, content)
}

func (s *Server) fsStat(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 参数必填"))
		return
	}
	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	info, err := fs.Stat(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) fsRawFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 参数必填"))
		return
	}
	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	content, err := fs.ReadFile(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// 仅对图片类型响应原始内容
	if !strings.HasPrefix(content.Stat.MimeType, "image/") {
		writeError(w, http.StatusUnsupportedMediaType, errors.New("仅支持图片类型的原始内容访问"))
		return
	}
	var data []byte
	if content.Encoding == "base64" {
		data, err = base64.StdEncoding.DecodeString(content.Content)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		data = []byte(content.Content)
	}
	w.Header().Set("Content-Type", content.Stat.MimeType)
	w.Header().Set("Cache-Control", "no-cache")
	// SVG 可包含 <script>，强制下载而非内联显示
	if content.Stat.MimeType == "image/svg+xml" {
		// RFC 5987: 清理文件名防止 header 注入
		safeName := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
				return r
			}
			return '_'
		}, filepath.Base(path))
		w.Header().Set("Content-Disposition", "attachment; filename="+safeName)
	}
	w.Write(data)
}

func (s *Server) fsSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, errors.New("query 参数必填"))
		return
	}
	path := r.URL.Query().Get("path")
	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	entries, err := fs.Search(r.Context(), path, query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if entries == nil {
		entries = []FileEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// ─── 写入操作 ───────────────────────────────────────────────────────────────

func (s *Server) fsWriteFile(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	// 写入操作需要 workspace lease
	release, ok := s.acquireProjectWorkspace(projectID, "fs:"+uuid.NewString())
	if !ok {
		writeError(w, http.StatusConflict, errors.New("project workspace is occupied"))
		return
	}
	defer release()

	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}

	var req struct {
		Path            string `json:"path"`
		Content         string `json:"content"`
		ExpectedVersion string `json:"expectedVersion"`
		CreateOnly      bool   `json:"createOnly"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 必填"))
		return
	}
	if len(req.Content) > maxFileSize {
		writeError(w, http.StatusBadRequest, fmt.Errorf("内容过大（%d 字节），最大支持 10MB", len(req.Content)))
		return
	}
	if err := fs.WriteFile(r.Context(), req.Path, []byte(req.Content), req.ExpectedVersion, req.CreateOnly); err != nil {
		if errors.Is(err, errFileVersionConflict) || errors.Is(err, errFileAlreadyExists) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": contentVersion([]byte(req.Content))})
}

func (s *Server) fsMkdir(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	release, ok := s.acquireProjectWorkspace(projectID, "fs:"+uuid.NewString())
	if !ok {
		writeError(w, http.StatusConflict, errors.New("project workspace is occupied"))
		return
	}
	defer release()

	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 必填"))
		return
	}

	if err := fs.Mkdir(r.Context(), req.Path); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) fsRemove(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	release, ok := s.acquireProjectWorkspace(projectID, "fs:"+uuid.NewString())
	if !ok {
		writeError(w, http.StatusConflict, errors.New("project workspace is occupied"))
		return
	}
	defer release()

	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 参数必填"))
		return
	}

	if err := fs.Remove(r.Context(), path); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) fsRename(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	release, ok := s.acquireProjectWorkspace(projectID, "fs:"+uuid.NewString())
	if !ok {
		writeError(w, http.StatusConflict, errors.New("project workspace is occupied"))
		return
	}
	defer release()

	fs, err := s.getFilesystem(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}

	var req struct {
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.OldPath == "" || req.NewPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("oldPath 和 newPath 必填"))
		return
	}

	// 安全检查：路径组件不能包含 ..
	for _, p := range []string{req.OldPath, req.NewPath} {
		for _, part := range strings.Split(p, "/") {
			if part == ".." {
				writeError(w, http.StatusBadRequest, errors.New("路径不能包含 .."))
				return
			}
		}
	}
	// 确保新路径在同一目录下（防止移动到项目外）
	oldDir := filepath.Dir(req.OldPath)
	newDir := filepath.Dir(req.NewPath)
	if oldDir != newDir {
		writeError(w, http.StatusBadRequest, errors.New("只能在同一目录下重命名"))
		return
	}

	if err := fs.Rename(r.Context(), req.OldPath, req.NewPath); err != nil {
		if errors.Is(err, errFileAlreadyExists) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ─── 辅助方法 ───────────────────────────────────────────────────────────────

// writeFSError 处理 Filesystem 相关错误，区分 runner 离线和其他错误。
func (s *Server) writeFSError(w http.ResponseWriter, err error) {
	if offline, ok := err.(*runnerOfflineError); ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":    "runner_offline",
			"runnerId": offline.RunnerID,
		})
		return
	}
	writeError(w, http.StatusBadRequest, err)
}
