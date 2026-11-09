package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"
)

const supervisionSnapshotPolicy = "source-manifest-v3"
const supervisionSnapshotFileLimit = 4 << 20
const supervisionSnapshotTotalLimit = 64 << 20
const supervisionSnapshotEntryLimit = 10000
const supervisionRulesLimit = 256 << 10

var errSupervisionSnapshotChanged = errors.New("采集期间工作区或项目规则发生变化")

type supervisionSnapshotEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
	Gap    string `json:"gap,omitempty"`
}

type supervisionSourceRule struct {
	Source  string `json:"source"`
	Scope   string `json:"scope"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

type supervisionWorkspaceManifest struct {
	Policy            string                     `json:"policy"`
	TargetOS          string                     `json:"targetOs,omitempty"`
	Directory         string                     `json:"directory"`
	DirectoryIdentity string                     `json:"directoryIdentity"`
	Entries           []supervisionSnapshotEntry `json:"entries"`
}

type supervisionCollectedContext struct {
	RevisionID string
	Manifest   supervisionWorkspaceManifest
	Rules      []supervisionSourceRule
	RulesHash  string
	Blobs      map[string][]byte
}

func supervisionSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// 不沿用 shadow Git 的忽略清单：被 gitignore 忽略的源文件仍须纳入。
// 排除项在清单中保留，计划不能把其中的路径当作已覆盖输入。
func supervisionSnapshotExclusion(name string, directory bool) string {
	base := strings.ToLower(path.Base(name))
	if directory {
		switch base {
		case ".git", ".svn", "node_modules", ".venv", "venv", "__pycache__", ".cache", ".next", "dist", "build", "target", "coverage", ".ssh", ".aws", ".azure":
			return "excluded_directory"
		}
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == ".npmrc" || base == ".pypirc" || base == ".netrc" || base == "id_rsa" || base == "id_ed25519" || base == "auth.json" || strings.Contains(base, "credentials") || strings.HasPrefix(base, "secrets.") {
		return "sensitive_path"
	}
	switch path.Ext(base) {
	case ".pem", ".key", ".pfx", ".p12":
		return "sensitive_path"
	}
	return ""
}

// 构建/测试可重建产物不参与源码版本；依赖目录仍由验证环境指纹另行核对。
// 排除策略固定在 manifest policy 中，不能由 Agent 自定义以隐藏源码变化。
func supervisionGeneratedPath(name string, directory bool) bool {
	base := strings.ToLower(path.Base(name))
	if directory {
		switch base {
		case "__pycache__", ".cache", ".pytest_cache", ".ruff_cache", ".mypy_cache", ".next", "dist", "build", "target", "coverage", "test-results", "playwright-report":
			return true
		}
	}
	return strings.HasSuffix(base, ".tsbuildinfo") || strings.HasSuffix(base, ".pyc") || base == ".coverage"
}

func supervisionIsRule(name string) bool {
	base := path.Base(name)
	return strings.EqualFold(base, "AGENTS.md") || strings.EqualFold(base, "CLAUDE.md")
}

func collectSupervisionWorkspace(ctx context.Context, admission WorkspaceAdmission, storageRoot string) (supervisionCollectedContext, error) {
	// 双重完整采集比较内容与行为元数据，不用 mtime 证明内容相同。
	for attempt := 0; attempt < 2; attempt++ {
		first, err := scanSupervisionWorkspace(ctx, admission, storageRoot)
		if err != nil {
			return first, err
		}
		second, err := scanSupervisionWorkspace(ctx, admission, storageRoot)
		if err != nil {
			return second, err
		}
		if first.RevisionID == second.RevisionID && first.RulesHash == second.RulesHash {
			return second, nil
		}
	}
	return supervisionCollectedContext{}, errSupervisionSnapshotChanged
}

func scanSupervisionWorkspace(ctx context.Context, admission WorkspaceAdmission, storageRoot string) (supervisionCollectedContext, error) {
	out := supervisionCollectedContext{Manifest: supervisionWorkspaceManifest{Policy: supervisionSnapshotPolicy, TargetOS: runtime.GOOS, Directory: admission.CanonicalPath, DirectoryIdentity: admission.DirectoryIdentity, Entries: []supervisionSnapshotEntry{}}, Rules: []supervisionSourceRule{}, Blobs: map[string][]byte{}}
	if err := verifyWorkspaceAdmissionPath(admission); err != nil {
		return out, err
	}
	root, err := os.OpenRoot(admission.OriginalPath)
	if err != nil {
		return out, err
	}
	defer root.Close()
	storage, err := filepath.Abs(storageRoot)
	if err != nil {
		return out, err
	}
	// 应用数据目录不允许成为被采集项目本身，避免把数据库/凭据或快照再次复制。
	storage, err = canonicalWorkspacePath(storage)
	if err != nil {
		return out, err
	}
	if storage == admission.CanonicalPath || isPathWithin(storage, admission.CanonicalPath) {
		return out, errors.New("项目目录位于应用数据目录内，无法建立隔离的托管基线")
	}
	var total int
	var walk func(string) error
	walk = func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(out.Manifest.Entries) >= supervisionSnapshotEntryLimit {
			return errors.New("托管基线超过 10000 个文件/目录项")
		}
		info, err := root.Lstat(filepath.FromSlash(name))
		if err != nil {
			return err // 不把无法枚举的树当作空树。
		}
		entry := supervisionSnapshotEntry{Path: name, Mode: uint32(info.Mode().Perm())}
		if supervisionGeneratedPath(name, info.IsDir()) {
			return nil
		}
		if supervisionIsRule(name) && (!info.Mode().IsRegular() || info.Size() > supervisionRulesLimit) {
			return errors.New("项目规则无法安全读取或超限，不能静默忽略")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.Kind, entry.Gap = "symlink", "link_input_not_resolved"
			entry.Target, err = root.Readlink(filepath.FromSlash(name))
			if err != nil {
				return err
			}
			out.Manifest.Entries = append(out.Manifest.Entries, entry)
			return nil // 不读取链接目标，尤其不能读取项目外文件。
		}
		if name != "." {
			absolute := filepath.Join(admission.CanonicalPath, filepath.FromSlash(name))
			if absolute == storage || isPathWithin(storage, absolute) {
				entry.Kind, entry.Gap = "excluded", "application_storage"
			} else if exclusion := supervisionSnapshotExclusion(name, info.IsDir()); exclusion != "" {
				entry.Kind, entry.Gap = "excluded", exclusion
			}
			if entry.Kind == "excluded" {
				out.Manifest.Entries = append(out.Manifest.Entries, entry)
				return nil
			}
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			entry.Kind, entry.Gap = "unsupported", "special_file"
			out.Manifest.Entries = append(out.Manifest.Entries, entry)
			return nil
		}
		if info.Mode().IsRegular() && info.Size() > supervisionSnapshotFileLimit {
			entry.Kind, entry.Size, entry.Gap = "file", info.Size(), "file_size_limit"
			out.Manifest.Entries = append(out.Manifest.Entries, entry)
			return nil
		}
		file, err := root.Open(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) || opened.Mode() != info.Mode() {
			return errSupervisionSnapshotChanged
		}
		if info.IsDir() {
			entry.Kind = "directory"
			out.Manifest.Entries = append(out.Manifest.Entries, entry)
			var children []string
			for {
				batch, readErr := file.ReadDir(256)
				for _, item := range batch {
					children = append(children, item.Name())
				}
				if len(children) > supervisionSnapshotEntryLimit {
					return errors.New("托管基线目录项超限")
				}
				if errors.Is(readErr, io.EOF) {
					break
				}
				if readErr != nil {
					return readErr
				}
			}
			sort.Strings(children)
			for _, child := range children {
				if err := walk(path.Join(name, child)); err != nil {
					return err
				}
			}
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(file, supervisionSnapshotFileLimit+1))
		if err != nil {
			return err
		}
		if len(data) > supervisionSnapshotFileLimit || int64(len(data)) != info.Size() {
			return errSupervisionSnapshotChanged
		}
		total += len(data)
		if total > supervisionSnapshotTotalLimit {
			return errors.New("托管基线输入超过 64 MiB")
		}
		entry.Kind, entry.Size, entry.SHA256 = "file", int64(len(data)), supervisionSHA256(data)
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			entry.Gap = "binary_content_not_retained"
		} else if codexAPIKeyPattern.Match(data) || codexBearerPattern.Match(data) || bytes.Contains(data, []byte("PRIVATE KEY-----")) {
			entry.Gap = "sensitive_content_not_retained"
		} else {
			out.Blobs[entry.SHA256] = data
			if supervisionIsRule(name) {
				out.Rules = append(out.Rules, supervisionSourceRule{Source: name, Scope: path.Dir(name), SHA256: entry.SHA256, Content: string(data)})
			}
		}
		out.Manifest.Entries = append(out.Manifest.Entries, entry)
		if supervisionIsRule(name) && entry.Gap != "" {
			return errors.New("项目规则存在无法保留的内容，不能静默忽略")
		}
		return nil
	}
	if err := walk("."); err != nil {
		return out, err
	}
	// 项目外祖先规则只作为显式规则来源读取，不扫描祖先目录的其他内容。
	var ancestors []string
	for parent := filepath.Dir(admission.CanonicalPath); ; parent = filepath.Dir(parent) {
		ancestors = append(ancestors, parent)
		if filepath.Dir(parent) == parent {
			break
		}
	}
	var inherited []supervisionSourceRule
	for i := len(ancestors) - 1; i >= 0; i-- {
		parentRoot, err := os.OpenRoot(ancestors[i])
		if err != nil {
			return out, err
		}
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			rule, err := readSupervisionAncestorRule(ctx, parentRoot, ancestors[i], name)
			if err != nil {
				parentRoot.Close()
				return out, err
			}
			if rule != nil {
				inherited = append(inherited, *rule)
			}
		}
		parentRoot.Close()
	}
	out.Rules = append(inherited, out.Rules...)
	rulesJSON, _ := json.Marshal(out.Rules)
	if len(rulesJSON) > supervisionRulesLimit {
		return out, errors.New("项目规则超过 256 KiB")
	}
	if err := verifyWorkspaceAdmissionPath(admission); err != nil {
		return out, err
	}
	manifestJSON, _ := json.Marshal(out.Manifest)
	out.RevisionID, out.RulesHash = supervisionSHA256(manifestJSON), supervisionSHA256(rulesJSON)
	return out, nil
}

func readSupervisionAncestorRule(ctx context.Context, root *os.Root, directory, name string) (*supervisionSourceRule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > supervisionRulesLimit {
		return nil, errors.New("祖先项目规则无法安全读取或超限")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errSupervisionSnapshotChanged
	}
	data, err := io.ReadAll(io.LimitReader(file, supervisionRulesLimit+1))
	if err != nil || len(data) > supervisionRulesLimit || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 || redactAgentText(string(data)) != string(data) {
		return nil, errors.New("祖先项目规则包含不可保留内容或超限")
	}
	return &supervisionSourceRule{Source: filepath.Join(directory, name), Scope: ".", SHA256: supervisionSHA256(data), Content: string(data)}, nil
}
