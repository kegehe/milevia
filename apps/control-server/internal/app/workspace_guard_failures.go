package app

import (
	"context"
	"path"
	"strings"
	"sync"
)

// Track only currently admitted operations. A failed command can affect nested
// scopes, but must not poison an unrelated project sharing the same transport.
type workspaceFailureTracker struct {
	mu      sync.Mutex
	watches map[*workspaceFailureWatch]struct{}
}
type workspaceFailureWatch struct {
	path      string
	uncertain bool
}

func (t *workspaceFailureTracker) watch(p string) func() bool {
	w := &workspaceFailureWatch{path: p}
	t.mu.Lock()
	if t.watches == nil {
		t.watches = make(map[*workspaceFailureWatch]struct{})
	}
	t.watches[w] = struct{}{}
	t.mu.Unlock()
	return func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		delete(t.watches, w)
		return !w.uncertain
	}
}

func (t *workspaceFailureTracker) mark(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for w := range t.watches {
		if p == "" || w.path == "" || workspaceFailurePathsOverlap(p, w.path) {
			w.uncertain = true
		}
	}
}

func workspaceFailurePathsOverlap(a, b string) bool {
	a, b = workspaceFailureComparisonPath(a), workspaceFailureComparisonPath(b)
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}

func workspaceWindowsDrivePath(p string) bool {
	return len(p) >= 3 && (p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z') && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}

func workspaceFailureComparisonPath(p string) string {
	// Match the target Windows canonicalizer even on a Linux controller.
	// Linux paths, including drive-looking path segments and WSL UNC views,
	// retain case: folding those would mix independent workspaces.
	if workspaceWindowsDrivePath(p) {
		p = strings.ToLower(p)
	}
	return path.Clean(strings.ReplaceAll(p, `\`, "/"))
}

type workspaceCommandPathKey struct{}

func workspaceCommandContext(ctx context.Context, p string) context.Context {
	return context.WithValue(ctx, workspaceCommandPathKey{}, p)
}
func workspaceCommandPath(ctx context.Context) string {
	p, _ := ctx.Value(workspaceCommandPathKey{}).(string)
	return p
}

func workspaceWSLTrackingPath(distro, p string) string {
	if converted, ok := uncToWslPath(p, distro); ok {
		p = converted
	}
	if len(p) > 2 && p[1] == ':' {
		p = windowsToWSLMntPath(p)
	}
	p = path.Clean(strings.ReplaceAll(p, `\`, "/"))
	if len(p) >= 6 && strings.HasPrefix(p, "/mnt/") && (len(p) == 6 || p[6] == '/') {
		p = strings.ToLower(p) // Windows drive view; Linux-native paths retain case.
	}
	return p
}
