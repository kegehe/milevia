package app

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Node.js 托管运行时的取用与落地。
//
// 形态是**托管工具链**（docs/42 §7.1）：把官方分发包解压到平台自己的目录，
// 用它的 node / npm 工作。三个理由：
//
//  1. 不要 sudo、不要 UAC（apt/winget/MSI 全都要提权，而 SSH 远端多数没有免密 sudo）；
//  2. 不污染系统：删掉一个目录就完全回到原状；
//  3. 不和用户已有的 Node 打架：不动 PATH、不动 `npm prefix -g`。
//
// 另外两条纪律写在这里，因为它们是"能不能在远端放开"的前提：
//   - **必须校验 SHA256**，不匹配即中止并删除已下载文件；没有"跳过校验"的开关。
//   - **不执行任何安装脚本**，解压即用 —— 这与 `curl | bash` 在供应链风险上不是一个量级。

// nodeRuntimeMirrorEnv 是镜像源开关。默认官方源。
//
// 为什么要有它：下载走的是**目标环境自己的网络**，受限网络下官方源常不可达。
const nodeRuntimeMirrorEnv = "AUTO_NODE_MIRROR"

const defaultNodeRuntimeBaseURL = "https://nodejs.org/dist"

// nodeRuntimeIndexTTL 是版本索引的缓存时长。索引一天最多变几次，缓存能省掉
// 每次打开页面都去拉一次 200+ 条的 JSON。
const nodeRuntimeIndexTTL = 30 * time.Minute

// nodeRuntimeBaseURL 给出当前配置的下载源。
func nodeRuntimeBaseURL() string {
	if mirror := strings.TrimSpace(os.Getenv(nodeRuntimeMirrorEnv)); mirror != "" {
		return strings.TrimRight(mirror, "/")
	}
	return defaultNodeRuntimeBaseURL
}

// nodeRuntimeManager 负责取版本清单、取校验和、下载与解压。
type nodeRuntimeManager struct {
	client *http.Client

	mu          sync.Mutex
	index       []nodeVersionEntry
	indexAt     time.Time
	checksums   map[string]map[string]string // version → 文件名 → sha256
	checksumsAt map[string]time.Time
}

func newNodeRuntimeManager() *nodeRuntimeManager {
	return &nodeRuntimeManager{
		client:      &http.Client{Timeout: 10 * time.Minute},
		checksums:   map[string]map[string]string{},
		checksumsAt: map[string]time.Time{},
	}
}

// fetchNodeVersionIndex 取 dist/index.json（带缓存）。
func (m *nodeRuntimeManager) fetchNodeVersionIndex(ctx context.Context) ([]nodeVersionEntry, error) {
	if m == nil {
		return nil, fmt.Errorf("运行时管理器不可用")
	}
	m.mu.Lock()
	if len(m.index) > 0 && time.Since(m.indexAt) < nodeRuntimeIndexTTL {
		cached := m.index
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()

	raw, err := m.getText(ctx, nodeRuntimeBaseURL()+"/index.json")
	if err != nil {
		return nil, err
	}
	entries, err := parseNodeVersionIndex([]byte(raw))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.index, m.indexAt = entries, time.Now()
	m.mu.Unlock()
	return entries, nil
}

// fetchNodeChecksums 取某个版本的 SHASUMS256.txt（带缓存）。
//
// 校验和与文件名都从这一份里取：它同时解决了"官方文件名怎么拼"和"下载完对不对"
// 两个问题，不必维护第二份命名规则。
func (m *nodeRuntimeManager) fetchNodeChecksums(ctx context.Context, version string) (map[string]string, error) {
	if m == nil {
		return nil, fmt.Errorf("运行时管理器不可用")
	}
	m.mu.Lock()
	if cached, ok := m.checksums[version]; ok && time.Since(m.checksumsAt[version]) < nodeRuntimeIndexTTL {
		m.mu.Unlock()
		return cached, nil
	}
	m.mu.Unlock()

	raw, err := m.getText(ctx, nodeRuntimeBaseURL()+"/v"+version+"/SHASUMS256.txt")
	if err != nil {
		return nil, err
	}
	parsed, err := parseNodeChecksums(raw)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.checksums[version] = parsed
	m.checksumsAt[version] = time.Now()
	m.mu.Unlock()
	return parsed, nil
}

// resolveDistribution 选出目标平台的分发包。
func (m *nodeRuntimeManager) resolveDistribution(ctx context.Context, versionSelector string, platform NodePlatformKey) (runtimeDistribution, error) {
	entries, err := m.fetchNodeVersionIndex(ctx)
	if err != nil {
		return runtimeDistribution{}, err
	}
	entry, err := pickNodeVersion(entries, versionSelector)
	if err != nil {
		return runtimeDistribution{}, err
	}
	checksums, err := m.fetchNodeChecksums(ctx, entry.nodeVersion())
	if err != nil {
		return runtimeDistribution{}, err
	}
	return selectNodeDistribution(checksums, entry.nodeVersion(), platform, nodeRuntimeBaseURL())
}

func (m *nodeRuntimeManager) getText(ctx context.Context, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := m.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("访问 %s 失败：%w（受限网络可配置 %s 指向镜像源）", url, err, nodeRuntimeMirrorEnv)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("访问 %s 返回 %d", url, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// downloadVerified 下载分发包并**必须**通过校验，返回落盘路径。
//
// 边写边算 sha256，全部下完再比对：这样可以一边下一边丢，不必把整个包读进内存。
// 校验失败即删除已下载文件并报错 —— 留着一个不完整的包在盘上，下次可能被误用。
func (m *nodeRuntimeManager) downloadVerified(ctx context.Context, dist runtimeDistribution, destPath string, onProgress func(int64, int64)) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, dist.URL, nil)
	if err != nil {
		return err
	}
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("下载 %s 失败：%w（受限网络可配置 %s 指向镜像源）", dist.URL, err, nodeRuntimeMirrorEnv)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 %s 返回 %d", dist.URL, response.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	writer := io.MultiWriter(file, hasher)

	var written int64
	buffer := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			os.Remove(destPath)
			return err
		}
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			if _, err := writer.Write(buffer[:read]); err != nil {
				file.Close()
				os.Remove(destPath)
				return err
			}
			written += int64(read)
			if onProgress != nil {
				onProgress(written, response.ContentLength)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			file.Close()
			os.Remove(destPath)
			return fmt.Errorf("读取下载内容失败：%w", readErr)
		}
	}
	if err := file.Close(); err != nil {
		os.Remove(destPath)
		return err
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actual, dist.SHA256) {
		os.Remove(destPath)
		return fmt.Errorf("下载的分发包校验和不匹配（官方 %s，实际 %s），已删除该文件", dist.SHA256, actual)
	}
	return nil
}

// extractArchive 解压分发包到 destDir，并剥掉官方的顶层目录。
//
// 官方包的内部结构是 `node-vX-<platform>/...`，我们只要里面那一层的**内容**，
// 因为 destDir 本身已经是"这个版本的家"。
//
// 两条安全约束（不可省）：
//   - 拒绝任何逃出 destDir 的条目（zip-slip / tar-slip），包括绝对路径与 `..`；
//   - 符号链接只在**解析后仍留在 destDir 内**时创建，否则跳过。
//
// Node 的 Linux 包里 `bin/npm`、`bin/npx`、`bin/corepack` 都是符号链接，
// 所以符号链接必须处理 —— 跳过它们会得到一个"没有 npm 的 Node"。
func extractArchive(archivePath string, format nodeArchiveFormat, destDir string) error {
	switch format {
	case nodeArchiveZip:
		return extractZipArchive(archivePath, destDir)
	case nodeArchiveTarGz:
		return extractTarGzArchive(archivePath, destDir)
	}
	return fmt.Errorf("不支持的压缩格式 %q", format)
}

// stripArchiveRoot 去掉官方包的顶层目录，并拒绝逃逸路径。
//
// 第二个返回值：该条目是否可用（不安全时为 false，调用方跳过并计数）。
func stripArchiveRoot(name string, isDir bool) (string, bool) {
	cleaned := strings.ReplaceAll(name, "\\", "/")
	cleaned = strings.TrimPrefix(cleaned, "./")
	// 目录条目常带尾部斜杠；去掉它，让返回值是规范形状（否则 `bin/` 与 `bin`
	// 会算出同一个目标路径却返回不同字符串，判据写起来容易出错）。
	cleaned = strings.TrimSuffix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return "", false
	}
	// 绝对路径与盘符一律拒绝。
	if strings.HasPrefix(cleaned, "/") || (len(cleaned) > 1 && cleaned[1] == ':') {
		return "", false
	}
	parts := strings.Split(cleaned, "/")
	if len(parts) <= 1 && !isDir {
		// 顶层就是一个文件（正常包不会这样），无法剥离顶层目录。
		return "", false
	}
	// 去掉第一段（顶层目录）。
	relative := strings.Join(parts[1:], "/")
	if relative == "" {
		// 就是顶层目录本身：调用方不需要建它，destDir 已经是它。
		return "", false
	}
	// 逐段检查，任何 `..` 都拒绝（不依赖 filepath.Clean 之后再比 —— 那种做法在
	// 符号链接参与时并不可靠）。
	for _, part := range strings.Split(relative, "/") {
		if part == ".." {
			return "", false
		}
	}
	return relative, true
}

func extractZipArchive(archivePath, destDir string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("打开 zip 失败：%w", err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		relative, ok := stripArchiveRoot(entry.Name, entry.FileInfo().IsDir())
		if !ok {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(relative))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// zip 里可能带 unix 权限位；没有时按可读可执行处理（Node 的包里全是可执行/脚本）。
		mode := entry.Mode()
		if mode == 0 {
			mode = 0o755
		}
		if err := writeZipEntry(entry, target, mode); err != nil {
			return err
		}
	}
	return nil
}

func writeZipEntry(entry *zip.File, target string, mode os.FileMode) error {
	source, err := entry.Open()
	if err != nil {
		return fmt.Errorf("读取 zip 条目失败：%w", err)
	}
	defer source.Close()
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(destination, source); err != nil {
		destination.Close()
		return fmt.Errorf("写出 %s 失败：%w", target, err)
	}
	return destination.Close()
}

func extractTarGzArchive(archivePath, destDir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	uncompressed, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("解压 gzip 失败：%w", err)
	}
	defer uncompressed.Close()

	reader := tar.NewReader(uncompressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取 tar 失败：%w", err)
		}
		isDir := header.Typeflag == tar.TypeDir
		relative, ok := stripArchiveRoot(header.Name, isDir)
		if !ok {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(relative))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(destination, reader); err != nil {
				destination.Close()
				return fmt.Errorf("写出 %s 失败：%w", target, err)
			}
			if err := destination.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// 链接目标必须落在 destDir 之内，否则跳过（不让包里的一条链接
			// 把我们指到系统目录里去）。
			if !linkStaysInside(destDir, target, header.Linkname) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			linkTarget := header.Linkname
			if header.Typeflag == tar.TypeLink {
				// 硬链接在 tar 里的 Linkname 是包内相对路径，同样要剥掉顶层目录。
				if stripped, ok := stripArchiveRoot(header.Linkname, false); ok {
					linkTarget = filepath.FromSlash(stripped)
				}
			}
			if err := os.Symlink(linkTarget, target); err != nil && runtime.GOOS != "windows" {
				return fmt.Errorf("创建符号链接 %s 失败：%w", target, err)
			}
		default:
			// 其它类型（设备、FIFO 等）官方包里不该有，静默跳过。
			continue
		}
	}
}

// linkStaysInside 判断一条链接是否落在 destDir 之内。
//
// ⚠️ 不能用 `filepath.IsAbs` 单独判断：它**是平台相关的** —— 在 Windows 上
// `filepath.IsAbs("/etc/passwd")` 返回 false（Windows 的绝对路径要有盘符），
// 于是同一条链接会在 Linux 上被拒、在 Windows 上被当成相对路径放行。
// 判据跟平台走，安全结论就会跟平台走。所以这里显式挡掉两种绝对形态，
// 再加一层"解析后仍在根内"的兜底。
func linkStaysInside(destDir, linkPath, linkTarget string) bool {
	if linkTarget == "" {
		return false
	}
	if strings.HasPrefix(linkTarget, "/") || strings.HasPrefix(linkTarget, "\\") {
		return false
	}
	if len(linkTarget) > 1 && linkTarget[1] == ':' {
		return false // 盘符形态（C:\... 或 C:/...）
	}
	if filepath.IsAbs(linkTarget) {
		return false
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), filepath.FromSlash(strings.ReplaceAll(linkTarget, "\\", "/"))))
	root := filepath.Clean(destDir)
	relative, err := filepath.Rel(root, resolved)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// ── 落点与安装 ──────────────────────────────────────────────────────────────

// toolchainRootEnv 可以覆盖托管工具链的落点。
//
// 存在的理由有两个，都不是"预留"：① 用户想把工具链放到别的盘（系统盘紧张）；
// ② 让整条安装链路能在临时目录里端到端跑测试，而不必真的往用户目录装东西。
const toolchainRootEnv = "AUTO_TOOLCHAIN_ROOT"

// managedToolchainRoot 给出托管工具链的根目录。
//
// Windows：`%LOCALAPPDATA%\Milevia\toolchain`
// Linux/macOS：`~/.local/share/milevia/toolchain`
//
// 一律在用户目录下 —— 这正是"不要提权"的落点，也是"删目录即还原"的前提。
func managedToolchainRoot() (string, error) {
	if override := strings.TrimSpace(os.Getenv(toolchainRootEnv)); override != "" {
		return override, nil
	}
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, "Milevia", "toolchain"), nil
	}
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return filepath.Join(base, "milevia", "toolchain"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "milevia", "toolchain"), nil
}

// nodeHomeBinary 给出"某个 node 家目录"里的 node 可执行文件路径。
//
// ⚠️ 两个平台的**包内布局不同**，这一点是抓官方数据时确认的，不是猜的：
//   - Windows 的 zip 把 `node.exe` / `npm.cmd` / `npx.cmd` 放在**顶层**；
//   - Linux 的 tar.gz 放在 `bin/` 下（`bin/npm` 还是指向包内脚本的符号链接）。
//
// 猜成同一种布局的表现是"解压成功但找不到 node"，报错完全指不到真正原因。
func nodeHomeBinary(nodeHome string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(nodeHome, "node.exe")
	}
	return filepath.Join(nodeHome, "bin", "node")
}

// nodeHomeNpm 给出"某个 node 家目录"里的 npm 命令路径。
func nodeHomeNpm(nodeHome string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(nodeHome, "npm.cmd")
	}
	return filepath.Join(nodeHome, "bin", "npm")
}

// managedNodeBinary 给出托管 Node 的 node 可执行文件路径。
func managedNodeBinary(toolchainRoot string) string {
	return nodeHomeBinary(filepath.Join(toolchainRoot, "node"))
}

// managedNpmCommand 给出托管 Node 的 npm 命令路径。
func managedNpmCommand(toolchainRoot string) string {
	return nodeHomeNpm(filepath.Join(toolchainRoot, "node"))
}

// stagedNodeBinary 给出解压目录里的 node 可执行文件路径。
func stagedNodeBinary(payload string) string {
	return nodeHomeBinary(payload)
}

// managedNpmGlobalPrefix 给出给 CLI 用的 npm 全局 prefix。
//
// 与工具链放在同一棵树里，于是每个 CLI 的二进制路径可预测、可登记
// （见 docs/42 §7.2；`npmCLIInstall` 的 packageRoot / binaryPath / commandPath
// 已经按这个形状写好，只是把 prefix 换成这里）。
func managedNpmGlobalPrefix(toolchainRoot string) string {
	return filepath.Join(toolchainRoot, "npm-global")
}
