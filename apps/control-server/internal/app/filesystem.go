package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Filesystem 统一抽象，本地和远程各实现一个。
type Filesystem interface {
	// 读取操作（无需 workspace lease）
	ReadDir(ctx context.Context, path string) ([]FileEntry, error)
	ReadFile(ctx context.Context, path string) (*FileContent, error)
	Stat(ctx context.Context, path string) (FileInfo, error)
	Search(ctx context.Context, rootPath, query string) ([]FileEntry, error)

	// 写入操作（需 workspace lease，由 handler 层检查）
	WriteFile(ctx context.Context, path string, content []byte, expectedVersion string, createOnly bool) error
	Mkdir(ctx context.Context, path string) error
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
}

// FileEntry 目录列表中的条目。
type FileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"modTime,omitempty"`
}

// FileInfo 文件元信息。
type FileInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	ModTime  string `json:"modTime"`
	Mode     string `json:"mode"`     // 文件权限，如 "0644"
	IsText   bool   `json:"isText"`   // 是否为文本文件
	MimeType string `json:"mimeType"` // MIME 类型，如 "image/png"
}

// FileContent 文件内容响应。
type FileContent struct {
	Content  string   `json:"content"`            // 文本内容（文本文件）
	Encoding string   `json:"encoding,omitempty"` // 编码方式，"base64" 表示二进制
	Version  string   `json:"version"`
	Stat     FileInfo `json:"stat"`
}

const maxFileSize = 10 * 1024 * 1024 // 10MB

var errFileVersionConflict = errors.New("文件已被修改，请重新加载后再保存")
var errFileAlreadyExists = errors.New("文件已存在")

// ─── LocalFilesystem ────────────────────────────────────────────────────────

// LocalFilesystem 通过本地文件系统实现 Filesystem 接口。
type LocalFilesystem struct {
	allowedRoot string // 向后兼容的单一 root
	projectPath string // 项目根路径
}

func (fs *LocalFilesystem) ReadDir(ctx context.Context, path string) ([]FileEntry, error) {
	absPath, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, err
	}
	result := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		result = append(result, FileEntry{
			Name:    entry.Name(),
			Path:    fs.relativePath(absPath, entry.Name()),
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result, nil
}

func (fs *LocalFilesystem) ReadFile(ctx context.Context, path string) (*FileContent, error) {
	absPath, err := fs.resolvePath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New("路径是目录，不是文件")
	}
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("文件过大（%d 字节），最大支持 10MB", info.Size())
	}
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("文件过大，最大支持 10MB")
	}
	fi := fs.makeFileInfo(absPath, info, data[:min(len(data), 512)])
	if fi.IsText {
		return &FileContent{Content: string(data), Version: contentVersion(data), Stat: fi}, nil
	}
	return &FileContent{Content: base64.StdEncoding.EncodeToString(data), Encoding: "base64", Version: contentVersion(data), Stat: fi}, nil
}

func (fs *LocalFilesystem) Stat(ctx context.Context, path string) (FileInfo, error) {
	absPath, err := fs.resolvePath(path)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return FileInfo{}, err
	}
	// 对于文本检测，只读前 512 字节
	var sample []byte
	if !info.IsDir() && info.Size() > 0 {
		f, err := os.Open(absPath)
		if err == nil {
			sample = make([]byte, 512)
			n, _ := f.Read(sample)
			sample = sample[:n]
			f.Close()
		}
	}
	return fs.makeFileInfo(absPath, info, sample), nil
}

func (fs *LocalFilesystem) Search(ctx context.Context, rootPath, query string) ([]FileEntry, error) {
	absRoot, err := fs.resolvePath(rootPath)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(query)
	var result []FileEntry
	limit := 100
	var limitErr = errors.New("search limit reached")
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(result) >= limit {
			return limitErr
		}
		// 检查 context 取消
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// 跳过隐藏目录和常见忽略目录
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "__pycache__") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.Contains(strings.ToLower(d.Name()), query) {
			info, _ := d.Info()
			size := int64(0)
			modTime := ""
			if info != nil {
				size = info.Size()
				modTime = info.ModTime().Format(time.RFC3339)
			}
			result = append(result, FileEntry{
				Name:    d.Name(),
				Path:    fs.relativePath(filepath.Dir(path), d.Name()),
				IsDir:   false,
				Size:    size,
				ModTime: modTime,
			})
		}
		return nil
	})
	if err != nil && err != limitErr {
		return nil, err
	}
	return result, nil
}

func (fs *LocalFilesystem) WriteFile(ctx context.Context, path string, content []byte, expectedVersion string, createOnly bool) error {
	absPath, err := fs.resolveMutationPath(path)
	if err != nil {
		return err
	}
	if err := fs.verifyVersion(ctx, path, expectedVersion); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Lstat(absPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("不能通过符号链接写入文件")
		}
		if createOnly {
			return errFileAlreadyExists
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// 原子写入：先写临时文件再 rename
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(dir, ".auto-fs-write-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(content); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	// 保留已有文件的权限；新文件使用常规 0644 权限。
	if err := os.Chmod(tmpPath, mode); err != nil {
		os.Remove(tmpPath)
		return err
	}
	// 在最终替换前再次确认版本，缩小外部进程修改文件的竞争窗口。
	if err := fs.verifyVersion(ctx, path, expectedVersion); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if info, err := os.Lstat(absPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		os.Remove(tmpPath)
		return errors.New("不能通过符号链接写入文件")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		os.Remove(tmpPath)
		return err
	}
	if createOnly {
		if err := os.Link(tmpPath, absPath); err != nil {
			os.Remove(tmpPath)
			if errors.Is(err, os.ErrExist) {
				return errFileAlreadyExists
			}
			return err
		}
		return os.Remove(tmpPath)
	}
	if err := os.Rename(tmpPath, absPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func (fs *LocalFilesystem) verifyVersion(ctx context.Context, path, expectedVersion string) error {
	if expectedVersion == "" {
		return nil
	}
	current, err := fs.ReadFile(ctx, path)
	if err != nil || current.Version != expectedVersion {
		return errFileVersionConflict
	}
	return nil
}

func (fs *LocalFilesystem) Mkdir(ctx context.Context, path string) error {
	absPath, err := fs.resolvePath(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(absPath, 0o755)
}

func (fs *LocalFilesystem) Remove(ctx context.Context, path string) error {
	absPath, err := fs.resolveMutationPath(path)
	if err != nil {
		return err
	}
	if fs.isProjectRoot(absPath) {
		return errors.New("不能删除项目根目录")
	}
	return os.RemoveAll(absPath)
}

func (fs *LocalFilesystem) Rename(ctx context.Context, oldPath, newPath string) error {
	absOld, err := fs.resolveMutationPath(oldPath)
	if err != nil {
		return err
	}
	absNew, err := fs.resolveMutationPath(newPath)
	if err != nil {
		return err
	}
	if fs.isProjectRoot(absOld) {
		return errors.New("不能重命名项目根目录")
	}
	if absOld == absNew {
		return nil
	}
	if _, err := os.Lstat(absNew); err == nil {
		return errFileAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(absOld, absNew)
}

// resolveMutationPath resolves and validates every path component except the
// final one. Mutating the final component must operate on a symlink itself,
// rather than silently following it to a different filesystem object.
func (fs *LocalFilesystem) resolveMutationPath(path string) (string, error) {
	// WSL UNC 项目路径：走字符串前缀解析（写法操作不 follow symlink，见 resolvePathUNC）。
	if isWSLUncPath(fs.projectPath) {
		norm, err := fs.resolvePathUNC(path)
		if err != nil {
			return "", err
		}
		return norm, nil
	}
	if path == "" || path == "/" {
		projectRoot, err := filepath.EvalSymlinks(fs.projectPath)
		if err != nil {
			return "", err
		}
		return projectRoot, nil
	}

	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(fs.projectPath, candidate)
	}
	candidate = filepath.Clean(candidate)
	parent, name := filepath.Dir(candidate), filepath.Base(candidate)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("父目录不存在：%w", err)
	}
	projectRoot, err := filepath.EvalSymlinks(fs.projectPath)
	if err != nil {
		return "", err
	}
	if !pathWithin(projectRoot, resolvedParent) {
		return "", errors.New("路径超出项目范围")
	}
	return filepath.Join(resolvedParent, name), nil
}

func (fs *LocalFilesystem) isProjectRoot(path string) bool {
	// WSL UNC：直接与规范项目根比较字符串。
	if isWSLUncPath(fs.projectPath) {
		return wslUncNormalize(path) == wslUncNormalize(fs.projectPath)
	}
	projectRoot, err := filepath.EvalSymlinks(fs.projectPath)
	return err == nil && path == projectRoot
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// resolvePath 将相对路径解析为绝对路径并校验安全性。
func (fs *LocalFilesystem) resolvePath(path string) (string, error) {
	if path == "" || path == "/" {
		return fs.projectPath, nil
	}
	// WSL UNC 项目路径（\\wsl$...）：filepath.EvalSymlinks 对 9P UNC 不可靠（Windows API
	// 报 "system cannot find"），改用字符串前缀校验，否则 WSL 项目任何子路径都读写不了。
	if isWSLUncPath(fs.projectPath) {
		return fs.resolvePathUNC(path)
	}
	// 如果已经是绝对路径，校验是否在 projectPath 下
	if filepath.IsAbs(path) {
		absolute, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		// 必须在 projectPath 内（而非仅 allowedRoot）
		projectRoot, err := filepath.EvalSymlinks(fs.projectPath)
		if err != nil {
			return "", err
		}
		relative, err := filepath.Rel(projectRoot, absolute)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return "", errors.New("路径超出项目范围")
		}
		return absolute, nil
	}
	// 相对路径：基于项目根路径解析
	absPath := filepath.Join(fs.projectPath, path)
	absolute, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// 路径可能不存在（如创建新文件），此时检查父目录
		parentAbs := filepath.Dir(absPath)
		parentResolved, err := filepath.EvalSymlinks(parentAbs)
		if err != nil {
			return "", fmt.Errorf("父目录不存在：%w", err)
		}
		// 父目录必须在 projectPath 内
		projectRoot, perr := filepath.EvalSymlinks(fs.projectPath)
		if perr != nil {
			return "", perr
		}
		parentRel, prel := filepath.Rel(projectRoot, parentResolved)
		if prel != nil || parentRel == ".." || strings.HasPrefix(parentRel, ".."+string(os.PathSeparator)) {
			return "", errors.New("路径超出项目范围")
		}
		return filepath.Join(parentResolved, filepath.Base(absPath)), nil
	}
	// 必须在 projectPath 内（相对路径不允许逃逸到其他 root）
	projectRoot, err := filepath.EvalSymlinks(fs.projectPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(projectRoot, absolute)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("路径超出项目范围")
	}
	return absolute, nil
}

// resolvePathUNC 用字符串前缀校验解析 WSL UNC 项目内的路径（不触发 EvalSymlinks）。
// 相对路径安全拼接；绝对 UNC 路径提取相对部分后同样经 wslJoinUnc 的 filepath.Clean
// 净化（合并 . / ..），避免 .. 目录穿越逃逸出项目范围。
func (fs *LocalFilesystem) resolvePathUNC(path string) (string, error) {
	if path == "" || path == "/" {
		return wslUncNormalize(fs.projectPath), nil
	}
	if filepath.IsAbs(path) || isWSLUncPath(path) {
		norm := wslUncNormalize(path)
		if norm == "" {
			return "", errors.New("路径超出项目范围")
		}
		if !wslPathWithin(fs.projectPath, norm) {
			return "", errors.New("路径超出项目范围")
		}
		// 提取相对部分（含 .. 段），交 wslJoinUnc 净化，防止 UNC 9P 层解析 .. 造成穿越。
		projNorm := wslUncNormalize(fs.projectPath)
		relPart := strings.TrimPrefix(norm[len(projNorm):], `\`)
		if relPart == "" {
			return projNorm, nil
		}
		joined, ok := wslJoinUnc(projNorm, relPart)
		if !ok || !wslPathWithin(projNorm, joined) {
			return "", errors.New("路径超出项目范围")
		}
		return joined, nil
	}
	// 相对路径
	joined, ok := wslJoinUnc(fs.projectPath, path)
	if !ok {
		return "", errors.New("路径超出项目范围")
	}
	return joined, nil
}

// relativePath 返回相对于项目根路径的相对路径。
func (fs *LocalFilesystem) relativePath(absDir, name string) string {
	full := filepath.Join(absDir, name)
	rel, err := filepath.Rel(fs.projectPath, full)
	if err != nil {
		return full
	}
	return rel
}

func (fs *LocalFilesystem) makeFileInfo(absPath string, info os.FileInfo, sample []byte) FileInfo {
	mimeType := detectMimeType(absPath, sample)
	isText := isTextFile(filepath.Base(absPath), sample)
	mode := fmt.Sprintf("%04o", info.Mode().Perm())
	return FileInfo{
		Name:     info.Name(),
		Path:     fs.relativePath(filepath.Dir(absPath), info.Name()),
		IsDir:    info.IsDir(),
		Size:     info.Size(),
		ModTime:  info.ModTime().Format(time.RFC3339),
		Mode:     mode,
		IsText:   isText,
		MimeType: mimeType,
	}
}

// ─── SFTPFilesystem ─────────────────────────────────────────────────────────

// SFTPFilesystem 通过 SFTP 实现远程文件系统接口。
type SFTPFilesystem struct {
	client   *sshClient
	rootPath string // 远程路径沙箱
}

func (fs *SFTPFilesystem) ReadDir(ctx context.Context, path string) ([]FileEntry, error) {
	absPath, err := fs.resolvePath(ctx, path)
	if err != nil {
		return nil, err
	}
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := sftpClient.ReadDir(absPath)
	if err != nil {
		return nil, err
	}
	result := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		absEntryPath := pathpkg.Join(absPath, entry.Name())
		result = append(result, FileEntry{
			Name:    entry.Name(),
			Path:    fs.relativePath(absEntryPath),
			IsDir:   entry.IsDir(),
			Size:    entry.Size(),
			ModTime: entry.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result, nil
}

func (fs *SFTPFilesystem) ReadFile(ctx context.Context, path string) (*FileContent, error) {
	absPath, err := fs.resolvePath(ctx, path)
	if err != nil {
		return nil, err
	}
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return nil, err
	}
	info, err := sftpClient.Stat(absPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New("路径是目录，不是文件")
	}
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("文件过大（%d 字节），最大支持 10MB", info.Size())
	}
	f, err := sftpClient.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("文件过大，最大支持 10MB")
	}
	fi := fs.makeFileInfo(absPath, info, data[:min(len(data), 512)])
	if fi.IsText {
		return &FileContent{Content: string(data), Version: contentVersion(data), Stat: fi}, nil
	}
	return &FileContent{Content: base64.StdEncoding.EncodeToString(data), Encoding: "base64", Version: contentVersion(data), Stat: fi}, nil
}

func (fs *SFTPFilesystem) Stat(ctx context.Context, path string) (FileInfo, error) {
	absPath, err := fs.resolvePath(ctx, path)
	if err != nil {
		return FileInfo{}, err
	}
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := sftpClient.Stat(absPath)
	if err != nil {
		return FileInfo{}, err
	}
	// 对于文本检测，只读前 512 字节
	var sample []byte
	if !info.IsDir() && info.Size() > 0 {
		f, err := sftpClient.Open(absPath)
		if err == nil {
			sample = make([]byte, 512)
			n, _ := f.Read(sample)
			sample = sample[:n]
			f.Close()
		}
	}
	return fs.makeFileInfo(absPath, info, sample), nil
}

func (fs *SFTPFilesystem) Search(ctx context.Context, rootPath, query string) ([]FileEntry, error) {
	absRoot, err := fs.resolvePath(ctx, rootPath)
	if err != nil {
		return nil, err
	}
	// 远程搜索：使用 find 命令
	// query 在单引号内，只需转义单引号，不能用 shellQuote
	escapedQuery := strings.ReplaceAll(query, "'", `'\''`)
	cmd := fmt.Sprintf(
		"find %s -type d \\( -name '.*' -o -name node_modules -o -name .git -o -name vendor -o -name __pycache__ \\) -prune -o -type f -iname '*%s*' -print | head -100",
		shellQuote(absRoot),
		escapedQuery,
	)
	output, err := fs.client.execCommand(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("远程搜索失败：%w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	result := make([]FileEntry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 验证路径在 rootPath 内
		if !remotePathWithinRoot(fs.rootPath, line) {
			continue
		}
		name := pathpkg.Base(line)
		result = append(result, FileEntry{
			Name:  name,
			Path:  fs.relativePath(line),
			IsDir: false,
		})
	}
	return result, nil
}

func (fs *SFTPFilesystem) WriteFile(ctx context.Context, path string, content []byte, expectedVersion string, createOnly bool) error {
	absPath, err := fs.resolvePath(ctx, path)
	if err != nil {
		return err
	}
	if err := fs.verifyVersion(ctx, path, expectedVersion); err != nil {
		return err
	}
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return err
	}
	// 确保父目录存在
	dir := pathpkg.Dir(absPath)
	if dir != "" && dir != "/" {
		if err := sftpClient.MkdirAll(dir); err != nil {
			return fmt.Errorf("创建父目录失败：%w", err)
		}
	}
	mode := os.FileMode(0o644)
	if info, err := sftpClient.Stat(absPath); err == nil {
		if createOnly {
			return errFileAlreadyExists
		}
		if info.IsDir() {
			return errors.New("路径是目录，不是文件")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if createOnly {
		f, err := sftpClient.OpenFile(absPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return errFileAlreadyExists
			}
			return err
		}
		written, writeErr := f.Write(content)
		if writeErr == nil && written != len(content) {
			writeErr = io.ErrShortWrite
		}
		if closeErr := f.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr != nil {
			_ = sftpClient.Remove(absPath)
			return writeErr
		}
		return sftpClient.Chmod(absPath, mode)
	}
	tmpPath := pathpkg.Join(dir, ".auto-fs-write-"+uuid.NewString())
	f, err := sftpClient.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	cleanup := func() { _ = sftpClient.Remove(tmpPath) }
	written, err := f.Write(content)
	if err == nil && written != len(content) {
		err = io.ErrShortWrite
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return err
	}
	if err := sftpClient.Chmod(tmpPath, mode); err != nil {
		cleanup()
		return err
	}
	// 写入临时文件期间若内容被外部进程更新，不能替换当前文件。
	if err := fs.verifyVersion(ctx, path, expectedVersion); err != nil {
		cleanup()
		return err
	}
	if err := sftpClient.PosixRename(tmpPath, absPath); err != nil {
		if err = sftpClient.Rename(tmpPath, absPath); err != nil {
			// 一些 SFTP v2 服务不允许 Rename 覆盖已有目标。先将旧文件
			// 移到同目录备份，再替换；替换失败时尽力恢复旧文件。
			backupPath := pathpkg.Join(dir, ".auto-fs-backup-"+uuid.NewString())
			if backupErr := sftpClient.Rename(absPath, backupPath); backupErr != nil {
				cleanup()
				return fmt.Errorf("原子替换远程文件失败：%w", err)
			}
			if replaceErr := sftpClient.Rename(tmpPath, absPath); replaceErr != nil {
				_ = sftpClient.Rename(backupPath, absPath)
				cleanup()
				return fmt.Errorf("替换远程文件失败：%w", replaceErr)
			}
			_ = sftpClient.Remove(backupPath)
		}
	}
	return nil
}

func (fs *SFTPFilesystem) verifyVersion(ctx context.Context, path, expectedVersion string) error {
	if expectedVersion == "" {
		return nil
	}
	current, err := fs.ReadFile(ctx, path)
	if err != nil || current.Version != expectedVersion {
		return errFileVersionConflict
	}
	return nil
}

func (fs *SFTPFilesystem) Mkdir(ctx context.Context, path string) error {
	absPath, err := fs.resolvePath(ctx, path)
	if err != nil {
		return err
	}
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return err
	}
	return sftpClient.MkdirAll(absPath)
}

func (fs *SFTPFilesystem) Remove(ctx context.Context, path string) error {
	absPath, err := fs.resolvePath(ctx, path)
	if err != nil {
		return err
	}
	// 禁止删除根目录
	rootPath, err := fs.canonicalRoot(ctx)
	if err != nil {
		return err
	}
	if absPath == rootPath {
		return errors.New("不能删除项目根目录")
	}
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return err
	}
	// 删除文件或目录
	info, err := sftpClient.Stat(absPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		// 尝试删除空目录，如果非空则用 rm -rf
		if err := sftpClient.RemoveDirectory(absPath); err != nil {
			// 非空目录，用远程命令删除
			_, cmdErr := fs.client.execCommand(ctx, "rm -rf "+shellQuote(absPath))
			if cmdErr != nil {
				return fmt.Errorf("删除目录失败：%w", cmdErr)
			}
		}
		return nil
	}
	return sftpClient.Remove(absPath)
}

func (fs *SFTPFilesystem) Rename(ctx context.Context, oldPath, newPath string) error {
	absOld, err := fs.resolvePath(ctx, oldPath)
	if err != nil {
		return err
	}
	absNew, err := fs.resolvePath(ctx, newPath)
	if err != nil {
		return err
	}
	if absOld == absNew {
		return nil
	}
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return err
	}
	if _, err := sftpClient.Lstat(absNew); err == nil {
		return errFileAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return sftpClient.Rename(absOld, absNew)
}

// resolvePath canonicalizes remote paths through SFTP and rejects paths which
// resolve outside the filesystem root. This prevents symlinks from escaping
// the project boundary.
func (fs *SFTPFilesystem) resolvePath(ctx context.Context, path string) (string, error) {
	if path == "" || path == "/" {
		path = fs.rootPath
	}
	absPath := path
	if !strings.HasPrefix(absPath, "/") {
		absPath = pathpkg.Join(fs.rootPath, path)
	}
	absPath = pathpkg.Clean(absPath)
	if !remotePathWithinRoot(fs.rootPath, absPath) {
		return "", errors.New("路径超出允许范围")
	}
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return "", err
	}
	root, err := fs.canonicalRoot(ctx)
	if err != nil {
		return "", err
	}
	if info, err := sftpClient.Lstat(absPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("不支持通过符号链接访问文件")
	}
	if resolved, err := sftpClient.RealPath(absPath); err == nil {
		if !remotePathWithinRoot(root, resolved) {
			return "", errors.New("路径超出允许范围")
		}
		return resolved, nil
	}
	parent, err := sftpClient.RealPath(pathpkg.Dir(absPath))
	if err != nil {
		return "", fmt.Errorf("父目录不存在：%w", err)
	}
	candidate := pathpkg.Join(parent, pathpkg.Base(absPath))
	if !remotePathWithinRoot(root, candidate) {
		return "", errors.New("路径超出允许范围")
	}
	return candidate, nil
}

func (fs *SFTPFilesystem) canonicalRoot(ctx context.Context) (string, error) {
	sftpClient, err := fs.client.getSFTPClient(ctx)
	if err != nil {
		return "", err
	}
	root, err := sftpClient.RealPath(fs.rootPath)
	if err != nil {
		return "", fmt.Errorf("解析远程根路径失败：%w", err)
	}
	return root, nil
}

func contentVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// relativePath 返回相对于 rootPath 的相对路径。
func (fs *SFTPFilesystem) relativePath(absPath string) string {
	root := pathpkg.Clean(fs.rootPath)
	path := pathpkg.Clean(absPath)
	if root == "/" {
		// 根路径为 "/" 时，去掉开头的 "/"
		if strings.HasPrefix(path, "/") {
			return path[1:]
		}
		return path
	}
	if !strings.HasPrefix(path, root+"/") && path != root {
		return absPath // 超出 rootPath，返回原始路径
	}
	if path == root {
		return ""
	}
	rel := strings.TrimPrefix(path, root+"/")
	if rel == "" {
		return ""
	}
	return rel
}

func (fs *SFTPFilesystem) makeFileInfo(absPath string, info os.FileInfo, sample []byte) FileInfo {
	mimeType := detectMimeType(absPath, sample)
	isText := isTextFile(pathpkg.Base(absPath), sample)
	mode := fmt.Sprintf("%04o", info.Mode().Perm())
	return FileInfo{
		Name:     info.Name(),
		Path:     fs.relativePath(absPath),
		IsDir:    info.IsDir(),
		Size:     info.Size(),
		ModTime:  info.ModTime().Format(time.RFC3339),
		Mode:     mode,
		IsText:   isText,
		MimeType: mimeType,
	}
}

// ─── 文本/二进制检测 ────────────────────────────────────────────────────────

// binaryExts 已知二进制扩展名
var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".ico": true, ".pdf": true,
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true,
	".7z": true, ".rar": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".otf": true,
	".mp3": true, ".mp4": true, ".wav": true, ".avi": true, ".mkv": true,
	".mov": true, ".flv": true, ".wmv": true,
	".sqlite": true, ".db": true,
	".class": true, ".o": true, ".pyc": true, ".pyd": true,
	".wasm": true, ".swf": true,
}

// text 已知文本扩展名
var textExts = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".mjs": true, ".cjs": true,
	".css": true, ".scss": true, ".less": true, ".sass": true,
	".html": true, ".htm": true, ".xml": true, ".svg": true,
	".json": true, ".json5": true, ".jsonc": true,
	".md": true, ".mdx": true, ".rst": true, ".txt": true,
	".go": true, ".py": true, ".rs": true, ".rb": true,
	".java": true, ".kt": true, ".scala": true, ".clj": true,
	".c": true, ".cpp": true, ".h": true, ".hpp": true,
	".cs": true, ".fs": true,
	".toml": true, ".yaml": true, ".yml": true, ".ini": true, ".cfg": true,
	".sh": true, ".bash": true, ".zsh": true, ".fish": true,
	".sql": true, ".graphql": true, ".gql": true,
	".env": true, ".gitignore": true, ".dockerignore": true,
	".dockerfile": true,
	".lock":       true, ".map": true,
	".conf": true, ".properties": true,
	".vue": true, ".svelte": true,
	".dart": true, ".swift": true,
	".lua": true, ".r": true,
	".proto": true, ".thrift": true,
	".cmake": true, ".makefile": true,
	".log": true, ".diff": true, ".patch": true,
}

// isTextFile 判断文件是否为文本文件。
// 通过扩展名映射 + 前 512 字节检测 NULL 字节。
func isTextFile(name string, content []byte) bool {
	ext := strings.ToLower(filepath.Ext(name))
	// 特殊处理无扩展名的常见文件名
	baseName := strings.ToLower(name)
	if baseName == "makefile" || baseName == "dockerfile" || baseName == "rakefile" ||
		baseName == "gemfile" || baseName == "procfile" || baseName == "vagrantfile" ||
		baseName == ".gitignore" || baseName == ".env" || baseName == ".editorconfig" ||
		baseName == "license" || baseName == "readme" || baseName == "changelog" {
		return true
	}
	if binaryExts[ext] {
		return false
	}
	if textExts[ext] {
		return true
	}
	// 未知扩展名：检测前 512 字节
	sample := content
	if len(sample) > 512 {
		sample = sample[:512]
	}
	return !bytes.ContainsRune(sample, 0)
}

// detectMimeType 根据扩展名检测 MIME 类型。
func detectMimeType(path string, sample []byte) string {
	ext := strings.ToLower(filepath.Ext(path))
	mimeMap := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".svg":  "image/svg+xml",
		".webp": "image/webp",
		".ico":  "image/x-icon",
		".pdf":  "application/pdf",
		".zip":  "application/zip",
		".json": "application/json",
		".xml":  "application/xml",
		".html": "text/html",
		".css":  "text/css",
		".js":   "application/javascript",
		".ts":   "application/typescript",
		".md":   "text/markdown",
	}
	if mt, ok := mimeMap[ext]; ok {
		return mt
	}
	return ""
}
