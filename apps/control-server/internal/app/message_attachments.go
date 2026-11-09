package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strings"
)

// MessageAttachment stores a workspace-relative path, never a client supplied
// absolute path or base64 payload. Files remain in the conversation workspace.
type MessageAttachment struct {
	Path      string `json:"path"`
	MediaType string `json:"mediaType,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

type AgentAttachment struct {
	Path      string
	MediaType string
	Data      string // base64, only for image content blocks; never persisted in events
}

const maxMessageAttachments = 8
const maxAttachmentBytes = 10 << 20
const maxNativeImageBytes = 5 << 20

func nativeImageMediaType(media string) bool {
	switch media {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func attachmentJSON(attachments []MessageAttachment) string {
	if len(attachments) == 0 {
		return "[]"
	}
	data, _ := json.Marshal(attachments)
	return string(data)
}

func decodeAttachments(raw string) []MessageAttachment {
	var attachments []MessageAttachment
	_ = json.Unmarshal([]byte(raw), &attachments)
	return attachments
}

func attachmentReferences(attachments []MessageAttachment) string {
	var refs []string
	for _, attachment := range attachments {
		p := attachment.Path
		if strings.ContainsAny(p, " \t\"`") {
			encoded, _ := json.Marshal(p)
			p = string(encoded)
		}
		refs = append(refs, "@"+p)
	}
	return strings.Join(refs, "\n")
}

// Client MIME and size are only hints. Retries compare the ordered, normalized
// paths so a deleted/modified file cannot change the result of an accepted send.
func sameAttachmentPaths(a, b []MessageAttachment) bool {
	paths := func(items []MessageAttachment) []string {
		var result []string
		seen := map[string]bool{}
		for _, item := range items {
			p, err := validateAttachmentPath(item.Path)
			if err != nil {
				return []string{"invalid:" + item.Path}
			}
			if !seen[p] {
				result = append(result, p)
				seen[p] = true
			}
		}
		return result
	}
	x, y := paths(a), paths(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func validateAttachmentPath(raw string) (string, error) {
	p := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
	if p == "" || len(p) > 1024 || strings.ContainsAny(p, "\x00\r\n:") || strings.HasPrefix(p, "/") {
		return "", errors.New("附件必须使用工作区内的相对路径")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errors.New("附件路径不能越出工作区")
		}
	}
	p = path.Clean(p)
	if p == "." {
		return "", errors.New("附件必须是文件")
	}
	return p, nil
}

// Windows batch entrypoints reparse argv with cmd.exe. Do not pass attachment
// names containing expansion/control syntax through those shims.
func validateAttachmentShimArgs(binary string, attachments []AgentAttachment) error {
	name := strings.ToLower(binary)
	if !strings.HasSuffix(name, ".cmd") && !strings.HasSuffix(name, ".bat") {
		return nil
	}
	for _, attachment := range attachments {
		if strings.ContainsAny(attachment.Path, "\"%&|<>^!\r\n") {
			return errors.New("当前 CLI 使用 Windows 批处理入口，附件名称含不支持的字符；请重命名后发送")
		}
	}
	return nil
}

func (s *Server) validateMessageAttachments(ctx context.Context, workspace resolvedConversationWorkspace, input []MessageAttachment) ([]MessageAttachment, error) {
	if len(input) > maxMessageAttachments {
		return nil, errors.New("每条消息最多添加 8 个附件")
	}
	if len(input) == 0 {
		return nil, nil
	}
	fs, err := s.filesystemForWorkspace(ctx, workspace)
	if err != nil {
		return nil, err
	}
	var result []MessageAttachment
	seen := map[string]bool{}
	var total int64
	for _, attachment := range input {
		p, err := validateAttachmentPath(attachment.Path)
		if err != nil {
			return nil, err
		}
		if seen[p] {
			continue
		}
		info, err := fs.Stat(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("无法读取附件 %s：%w", p, err)
		}
		if !info.Regular || info.Size < 0 || info.Size > maxAttachmentBytes {
			return nil, errors.New("附件必须是文件且单个不超过 10 MiB")
		}
		total += info.Size
		if total > 20<<20 {
			return nil, errors.New("每条消息的附件总计不能超过 20 MiB")
		}
		seen[p] = true
		result = append(result, MessageAttachment{Path: p, MediaType: info.MimeType, Size: info.Size})
	}
	return result, nil
}

// Resolve again at execution time: queued files may have been removed or changed.
// Native image blocks use the target filesystem, so SSH images never get read
// from an identically named path on the control server.
func (s *Server) runAttachments(ctx context.Context, runID, agentID, runnerID, root string) ([]AgentAttachment, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `select attachments from messages where run_id=? and role='user' order by created_at,id limit 1`, runID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	stored := decodeAttachments(raw)
	if len(stored) == 0 {
		return nil, nil
	}
	fs, err := s.filesystemForWorkspace(ctx, resolvedConversationWorkspace{Project: Project{Runner: runnerID}, Workspace: ConversationWorkspace{Path: root}})
	if err != nil {
		return nil, err
	}
	var result []AgentAttachment
	var total int64
	for _, attachment := range stored {
		if _, err := validateAttachmentPath(attachment.Path); err != nil {
			return nil, err
		}
		reader, info, err := fs.OpenRead(ctx, attachment.Path)
		if err != nil {
			return nil, fmt.Errorf("附件已不可用 %s：%w", attachment.Path, err)
		}
		if !info.Regular || info.Size < 0 || info.Size > maxAttachmentBytes {
			reader.Close()
			return nil, errors.New("附件不是文件或已超过大小限制")
		}
		total += info.Size
		if total > 20<<20 {
			reader.Close()
			return nil, errors.New("每条消息的附件总计不能超过 20 MiB")
		}
		item := AgentAttachment{Path: attachment.Path, MediaType: info.MimeType}
		if nativeImageMediaType(info.MimeType) && (agentID == "claude-code" || agentID == "codebuddy") && info.Size <= maxNativeImageBytes {
			data, readErr := io.ReadAll(io.LimitReader(reader, maxAttachmentBytes+1))
			reader.Close()
			if readErr != nil {
				return nil, readErr
			}
			if len(data) > maxAttachmentBytes {
				return nil, errors.New("图片附件过大")
			}
			media := http.DetectContentType(data)
			if nativeImageMediaType(media) && len(data) <= maxNativeImageBytes {
				item.MediaType, item.Data = media, base64.StdEncoding.EncodeToString(data)
			}
		} else {
			reader.Close()
		}
		// Relative paths work in local, WSL and SSH CLI working directories.
		item.Path = filepath.ToSlash(item.Path)
		if strings.HasPrefix(item.Path, "-") {
			item.Path = "./" + item.Path
		}
		result = append(result, item)
	}
	return result, nil
}

func agentContent(prompt string, attachments []AgentAttachment) []any {
	content := []any{map[string]any{"type": "text", "text": prompt}}
	for _, attachment := range attachments {
		if attachment.Data != "" {
			content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": attachment.MediaType, "data": attachment.Data}})
		}
	}
	return content
}
