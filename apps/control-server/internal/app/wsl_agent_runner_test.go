package app

import "testing"

func TestWslPathPrefixCachesMissingZshNode(t *testing.T) {
	r := &wslAgentRunner{nodeBinResolved: true}
	if got, want := r.wslPathPrefix(), `export PATH="$HOME/.npm-global/bin:$HOME/.local/bin:$PATH"`; got != want {
		t.Fatalf("wslPathPrefix()=%q want %q", got, want)
	}
}
