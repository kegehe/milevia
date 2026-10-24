package app

import "testing"

func TestWslPathPrefixCachesMissingZshNode(t *testing.T) {
	r := &wslAgentRunner{nodeBinResolved: true}
	if got, want := r.wslPathPrefix(), `export PATH="`+crossManagedPathValue+`:$PATH"`; got != want {
		t.Fatalf("wslPathPrefix()=%q want %q", got, want)
	}
}
