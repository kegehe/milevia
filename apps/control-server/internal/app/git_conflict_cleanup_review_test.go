package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Keep the production Done/finish contract: one exit value, no channel close.
// Stop only requests termination; the test controls when exit is confirmed.
type conflictCleanupReviewSession struct {
	*streamJSONSession
	send func(AgentRunSink) error
}

func (s *conflictCleanupReviewSession) Send(_ AgentRunRequest, sink AgentRunSink) error {
	return s.send(sink)
}

func TestConflictSuggestionCleanupReleasesAdmissionAfterRealExitSignal(t *testing.T) {
	for _, agent := range []string{"codebuddy", "qoder"} {
		for _, outcome := range []string{"completed", "completed-exit", "failed", "cancelled", "send-error", "early-exit"} {
			t.Run(agent+"/"+outcome, func(t *testing.T) {
				s, p, _ := admissionProjectForTest(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				session := &conflictCleanupReviewSession{streamJSONSession: newStreamJSONSession(
					streamJSONSessionConfig{agentID: agent, label: agent}, nil, nil, nil, nil)}
				var once sync.Once
				finish := func() { once.Do(func() { session.finish(nil) }) }
				defer finish()
				// 生产代码在进程退出（Done）之前不会返回（runInsightStreamingTurn 的
				// defer 要等它），所以调用必须放到 goroutine 里；否则"turn 已结束、进程
				// 还在"这个窗口永远观察不到，测试会一直等到超时。
				entered := make(chan struct{})
				var enteredOnce sync.Once
				session.send = func(sink AgentRunSink) error {
					enteredOnce.Do(func() { close(entered) })
					switch outcome {
					case "completed", "completed-exit":
						sink.AssistantText("resolved", "")
						sink.(AgentTurnSink).TurnFinished(nil)
						if outcome == "completed-exit" {
							finish()
						}
					case "failed":
						sink.(AgentTurnSink).TurnFinished(errors.New("turn failed"))
					case "cancelled":
						cancel()
					case "send-error":
						return errors.New("send failed")
					case "early-exit":
						// No turn result: the caller must consume Done itself.
						finish()
					}
					return nil
				}
				runner := &admissionStreamingRunner{session: session}
				s.codebuddyRunner, s.qoderRunner = runner, runner

				type reviewResult struct {
					text string
					err  error
				}
				call := make(chan reviewResult, 1)
				go func() {
					text, err := s.runConflictSuggestionAgent(ctx, p, agent, "review")
					call <- reviewResult{text: text, err: err}
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("conflict suggestion agent did not start")
				}

				a := reserveAdmissionForTest(t, s, p, "session")
				if outcome != "early-exit" && outcome != "completed-exit" {
					err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned")
					if !errors.Is(err, errWorkspaceNotQuiescent) {
						t.Fatalf("released before exit was confirmed: %v", err)
					}
				}
				finish()
				var result reviewResult
				select {
				case result = <-call:
				case <-time.After(10 * time.Second):
					t.Fatal("conflict suggestion agent did not return after the process exited")
				}
				if outcome == "completed" || outcome == "completed-exit" {
					if result.err != nil || result.text != "resolved" {
						t.Fatalf("result=%q err=%v", result.text, result.err)
					}
				} else if result.err == nil {
					t.Fatal("failed/cancelled/early-exit session succeeded")
				}
				deadline := time.Now().Add(2 * time.Second)
				for {
					err := s.transitionWorkspaceAdmission(context.Background(), a.ID, a.OwnerID, a.Epoch, "owned")
					if err == nil {
						break
					}
					if !errors.Is(err, errWorkspaceNotQuiescent) || time.Now().After(deadline) {
						t.Fatalf("exit was confirmed but workspace still cannot be admitted: %v", err)
					}
					time.Sleep(time.Millisecond)
				}
				var active int
				if err := s.db.QueryRow("select count(*) from workspace_guard_operations where state<>'released'").Scan(&active); err != nil || active != 0 {
					t.Fatalf("unreleased controller operations=%d err=%v", active, err)
				}
			})
		}
	}
}
