package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 集成验证：在真实 WSL 环境（本机有 Ubuntu）实测探测链路。无 WSL 时跳过。
func TestWSLDiscoveryIntegration(t *testing.T) {
	distro, err := detectDefaultWSLDistro(context.Background())
	if err != nil {
		t.Skipf("no WSL, skip: %v", err)
	}
	t.Logf("distro: %s", distro)
	if distro == "" {
		t.Fatal("distro empty")
	}
	home, err := detectWSLHome(context.Background(), distro)
	if err != nil {
		t.Fatalf("detectWSLHome: %v", err)
	}
	t.Logf("home: %s", home)
	if home == "" {
		t.Fatal("home empty")
	}
	unc := wslToUncPath(home, distro)
	t.Logf("uncHome: %s", unc)
	// 往返：UNC -> Linux 应还原 home
	if back, ok := uncToWslPath(unc, distro); !ok || back != home {
		t.Errorf("roundtrip failed: uncToWslPath(%q)=%q,%v; want %q", unc, back, ok, home)
	}

	// wslAgentRunner 就绪 / 版本探测（真实 WSL 内执行；WSL 无 claude 时读就绪为 false）
	runner := newWSLAgentRunner(Config{ClaudePath: "claude", CodexPath: "codex"}, distro)
	ready := runner.Ready(context.Background())
	t.Logf("claude ready in WSL: %v", ready)
	if ready {
		v := runner.Version(context.Background())
		t.Logf("claude version in WSL: %s", v)
		if v == "" {
			t.Errorf("version empty though ready")
		}
	}

	// 文件层：LocalFilesystem 直接读 UNC 项目目录（复用核心让 wsl-local 项目走它的前提）。
	projPath := unc // \\wsl$\Ubuntu\home\tangmaoke
	fs := &LocalFilesystem{allowedRoot: unc, projectPath: projPath}
	entries, err := fs.ReadDir(context.Background(), "/")
	if err != nil {
		t.Fatalf("LocalFilesystem.ReadDir on UNC root: %v", err)
	}
	t.Logf("ReadDir UNC root got %d entries", len(entries))
	if len(entries) == 0 {
		t.Errorf("expected non-empty home dir listing")
	}

	// 注册 meta：模拟 loadProjector 用 wslLocalRunnerMeta 生成的 roots，须是合法 UNC home。
	srv := &Server{}
	meta := srv.wslLocalRunnerMeta(distro, home)
	t.Logf("meta: ID=%s Environment=%s Root=%s Roots=%v", meta.ID, meta.Environment, meta.Root, meta.Roots)
	if meta.ID != "wsl-local" || meta.Environment != "wsl" {
		t.Errorf("meta identity wrong: ID=%q Env=%q", meta.ID, meta.Environment)
	}
	if len(meta.Roots) == 0 {
		t.Fatal("expected at least one root (WSL home)")
	}
	root := meta.Roots[0].Path
	if _, err := os.Stat(root); err != nil {
		t.Errorf("meta root not readable as UNC: %q: %v", root, err)
	}
	// roots 相对判断：home 下的任意子路径应覆盖在 root 内（allowedPathForRunner 依赖此）。
	if rel, err := filepath.Rel(root, filepath.Join(root, "subdir")); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("subpath should be within root, rel=%q err=%v", rel, err)
	}
}

// TestWSLReadyCacheTTL 验证 cachedReady 的 TTL 缓存语义：TTL 内复用不再探测，
// TTL 过期后重新探测。用 codex 的轻量探测（command -v codex）做真实 WSL probe。
func TestWSLReadyCacheTTL(t *testing.T) {
	distro, err := detectDefaultWSLDistro(context.Background())
	if err != nil {
		t.Skipf("no WSL, skip: %v", err)
	}
	runner := newWSLAgentRunner(Config{ClaudePath: "claude", CodexPath: "codex"}, distro)

	// 首次调用触发探测并写入缓存。
	first := runner.codexReady(context.Background())
	runner.mu.Lock()
	at := runner.codexAt
	cached := runner.codexCache
	runner.mu.Unlock()
	if at.IsZero() {
		t.Fatal("codexReady did not populate cache timestamp")
	}
	if cached != first {
		t.Fatalf("cache value=%v differs from result=%v", cached, first)
	}

	// TTL 内再次调用应命中缓存：时间戳不被刷新，缓存值保持一致。
	runner.mu.Lock()
	runner.codexAt = at.Add(-1 * time.Millisecond) // 保持 fresh（寿命 < TTL）
	runner.mu.Unlock()
	second := runner.codexReady(context.Background())
	runner.mu.Lock()
	atAfter := runner.codexAt
	runner.mu.Unlock()
	if !atAfter.Equal(at.Add(-1 * time.Millisecond)) {
		t.Fatalf("cache-hit call refreshed timestamp: before=%v after=%v", at.Add(-1*time.Millisecond), atAfter)
	}
	if second != first {
		t.Fatalf("cache value changed between hits: %v -> %v", first, second)
	}

	// 过期后再次调用应重新探测并刷新时间戳。
	runner.mu.Lock()
	runner.codexAt = time.Now().Add(-wslReadyCacheTTL - time.Second) // 过期
	runner.mu.Unlock()
	runner.codexReady(context.Background())
	runner.mu.Lock()
	atExpired := runner.codexAt
	runner.mu.Unlock()
	if !atExpired.After(time.Now().Add(-2 * time.Second)) {
		t.Fatalf("expired call did not re-probe/refresh cache timestamp: at=%v", atExpired)
	}
}
