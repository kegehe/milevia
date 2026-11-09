package app

import (
	"context"
	"errors"
	"testing"
)

func TestSupervisionCompletionGuardCancellationAndIndependentExecutions(t *testing.T) {
	s := &Server{}
	release, err := s.acquireSupervisionRoleCompletion(context.Background(), "execution-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.acquireSupervisionRoleCompletion(ctx, "execution-a"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled duplicate retained a completion claim", err)
	}
	otherRelease, err := s.acquireSupervisionRoleCompletion(context.Background(), "execution-b")
	if err != nil {
		t.Fatal("independent execution blocked", err)
	}
	otherRelease()
	release()
	completedRelease, err := s.acquireSupervisionRoleCompletion(context.Background(), "execution-a")
	if err != nil {
		t.Fatal("completed claim did not release waiters", err)
	}
	completedRelease()
	if len(s.supervisionCompletions) != 0 {
		t.Fatal("completed executions retained unbounded guard entries")
	}
}
