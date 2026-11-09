package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type wslPathWaitContext struct {
	context.Context
	waiting chan struct{}
}

func (ctx wslPathWaitContext) Done() <-chan struct{} {
	close(ctx.waiting)
	return ctx.Context.Done()
}

func TestWSLPathDiscoveryWaitRespectsRequestDeadline(t *testing.T) {
	done := make(chan struct{})
	runner := &wslAgentRunner{nodeBinLookup: done}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := runner.wslPathPrefixContext(wslPathWaitContext{ctx, waiting})
		result <- err
	}()
	<-waiting
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("path discovery ignored request cancellation", err)
		}
	case <-time.After(time.Second):
		close(done)
		t.Fatal("request waited for shared path discovery after cancellation")
	}
	// A canceled waiter must not cancel the shared lookup or poison its cache.
	runner.nodeBinMu.Lock()
	runner.nodeBin = "/native/node/bin"
	runner.nodeBinResolved = true
	runner.nodeBinLookup = nil
	close(done)
	runner.nodeBinMu.Unlock()
	prefix, err := runner.wslPathPrefixContext(context.Background())
	if err != nil || !strings.Contains(prefix, "'/native/node/bin'") {
		t.Fatal("later request lost shared discovery result", prefix, err)
	}
}
