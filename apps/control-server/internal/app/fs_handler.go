package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

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
		r.Get("/download", s.fsDownloadFile)
		r.Post("/download-ticket", s.fsCreateDownloadTicket)
		r.Get("/search", s.fsSearch)
		r.Get("/sqlite/tables", s.fsSQLiteTables)
		r.Get("/sqlite/schema", s.fsSQLiteSchema)
		r.Get("/sqlite/rows", s.fsSQLiteRows)

		// 写入操作（需 workspace lease）
		r.Put("/write", s.fsWriteFile)
		r.Post("/mkdir", s.fsMkdir)
		r.Delete("/remove", s.fsRemove)
		r.Post("/rename", s.fsRename)
	})
}

const downloadTicketTTL = time.Minute

type downloadTicketPayload struct {
	ProjectID      string `json:"projectId"`
	WorkspaceID    string `json:"workspaceId"`
	ConversationID string `json:"conversationId,omitempty"`
	Path           string `json:"path"`
	ExpiresAt      int64  `json:"expiresAt"`
}

// validDownloadTicket permits a short-lived, path-bound download navigation.
// It is intentionally restricted to GET downloads because browser navigations
// cannot attach the desktop session header.
func (s *Server) validDownloadTicket(r *http.Request) bool {
	payload, ok := s.downloadTicketPayload(r)
	return ok && payload.WorkspaceID != ""
}

func (s *Server) downloadTicketPayload(r *http.Request) (downloadTicketPayload, bool) {
	const prefix = "/api/projects/"
	const suffix = "/fs/download"
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		return downloadTicketPayload{}, false
	}
	projectID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
	if projectID == "" || strings.Contains(projectID, "/") {
		return downloadTicketPayload{}, false
	}
	parts := strings.Split(r.URL.Query().Get("ticket"), ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return downloadTicketPayload{}, false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return downloadTicketPayload{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return downloadTicketPayload{}, false
	}
	mac := hmac.New(sha256.New, []byte(s.config.SessionToken))
	_, _ = mac.Write(payloadBytes)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return downloadTicketPayload{}, false
	}
	var payload downloadTicketPayload
	if json.Unmarshal(payloadBytes, &payload) != nil {
		return downloadTicketPayload{}, false
	}
	valid := payload.ExpiresAt >= time.Now().UTC().Unix() && payload.ProjectID == projectID && payload.ConversationID == r.URL.Query().Get("conversationId") && payload.Path == r.URL.Query().Get("path")
	return payload, valid
}

func (s *Server) issueDownloadTicket(projectID, workspaceID, conversationID, path string) (string, error) {
	payloadBytes, err := json.Marshal(downloadTicketPayload{ProjectID: projectID, WorkspaceID: workspaceID, ConversationID: conversationID, Path: path, ExpiresAt: time.Now().UTC().Add(downloadTicketTTL).Unix()})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(s.config.SessionToken))
	_, _ = mac.Write(payloadBytes)
	return base64.RawURLEncoding.EncodeToString(payloadBytes) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// getFilesystem 根据项目的 runner 类型返回对应的 Filesystem 实现。
func (s *Server) getFilesystem(r *http.Request) (Filesystem, error) {
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		return nil, err
	}
	return s.filesystemForWorkspace(r.Context(), workspace)
}

func (s *Server) filesystemForWorkspace(ctx context.Context, workspace resolvedConversationWorkspace) (Filesystem, error) {
	project := workspace.Project

	if isLocalRunnerID(project.Runner) || project.Runner == "wsl-local" {
		// wsl-local 项目路径为 UNC（\\wsl$\...），Go 的 os 直读，复用 LocalFilesystem。
		return &LocalFilesystem{
			allowedRoot: s.config.AllowedRoot,
			projectPath: workspace.Workspace.Path,
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
		rootPath: workspace.Workspace.Path,
	}
	rootPath, err := fs.canonicalRoot(ctx)
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
	stream, info, err := fs.OpenRead(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer stream.Close()
	// 仅对图片类型响应原始内容
	if !strings.HasPrefix(info.MimeType, "image/") {
		writeError(w, http.StatusUnsupportedMediaType, errors.New("仅支持图片类型的原始内容访问"))
		return
	}
	w.Header().Set("Content-Type", info.MimeType)
	w.Header().Set("Cache-Control", "no-cache")
	if info.Size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}
	// SVG 可包含 <script>，强制下载而非内联显示
	if info.MimeType == "image/svg+xml" {
		w.Header().Set("Content-Disposition", contentDispositionFilename(path))
	}
	if _, err := io.Copy(w, stream); err != nil {
		return
	}
}

func (s *Server) fsDownloadFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 参数必填"))
		return
	}
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	if r.URL.Query().Get("ticket") != "" {
		payload, valid := s.downloadTicketPayload(r)
		if !valid || payload.WorkspaceID != workspace.Workspace.ID {
			writeError(w, http.StatusUnauthorized, errors.New("download ticket does not match the active workspace"))
			return
		}
	}
	fs, err := s.filesystemForWorkspace(r.Context(), workspace)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	stream, info, err := fs.OpenRead(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer stream.Close()
	contentType := info.MimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDispositionFilename(path))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if info.Size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}
	if _, err := io.Copy(w, stream); err != nil {
		return
	}
}

func (s *Server) fsCreateDownloadTicket(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path parameter is required"))
		return
	}
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	fs, err := s.filesystemForWorkspace(r.Context(), workspace)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	info, err := fs.Stat(r.Context(), input.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if info.IsDir {
		writeError(w, http.StatusBadRequest, errors.New("path is a directory"))
		return
	}
	ticket, err := s.issueDownloadTicket(chi.URLParam(r, "projectID"), workspace.Workspace.ID, workspace.ConversationID, input.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	query := "path=" + url.QueryEscape(input.Path) + "&ticket=" + url.QueryEscape(ticket)
	if workspace.ConversationID != "" {
		query += "&conversationId=" + url.QueryEscape(workspace.ConversationID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": "/api/projects/" + chi.URLParam(r, "projectID") + "/fs/download?" + query})
}

func contentDispositionFilename(path string) string {
	safeName := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, filepath.Base(path))
	if safeName == "" || safeName == "." {
		safeName = "download"
	}
	return "attachment; filename=" + safeName
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
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	// 写入操作需要实际目录级 workspace lease。
	release, ok := s.acquireWorkspace(workspaceLeaseKey(workspace.Project, workspace.Workspace), "fs:"+uuid.NewString())
	if !ok {
		writeError(w, http.StatusConflict, s.workspaceOccupiedError(workspaceLeaseKey(workspace.Project, workspace.Workspace)))
		return
	}
	defer release()

	fs, err := s.filesystemForWorkspace(r.Context(), workspace)
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
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	release, ok := s.acquireWorkspace(workspaceLeaseKey(workspace.Project, workspace.Workspace), "fs:"+uuid.NewString())
	if !ok {
		writeError(w, http.StatusConflict, s.workspaceOccupiedError(workspaceLeaseKey(workspace.Project, workspace.Workspace)))
		return
	}
	defer release()

	fs, err := s.filesystemForWorkspace(r.Context(), workspace)
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
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	release, ok := s.acquireWorkspace(workspaceLeaseKey(workspace.Project, workspace.Workspace), "fs:"+uuid.NewString())
	if !ok {
		writeError(w, http.StatusConflict, s.workspaceOccupiedError(workspaceLeaseKey(workspace.Project, workspace.Workspace)))
		return
	}
	defer release()

	fs, err := s.filesystemForWorkspace(r.Context(), workspace)
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
	workspace, err := s.resolveRequestWorkspaceFromRequest(r)
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	release, ok := s.acquireWorkspace(workspaceLeaseKey(workspace.Project, workspace.Workspace), "fs:"+uuid.NewString())
	if !ok {
		writeError(w, http.StatusConflict, s.workspaceOccupiedError(workspaceLeaseKey(workspace.Project, workspace.Workspace)))
		return
	}
	defer release()

	fs, err := s.filesystemForWorkspace(r.Context(), workspace)
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
