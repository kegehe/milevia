package app

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestConflictSuggestionAgentsFollowCatalogAndEnvironment(t *testing.T) {
	server := &Server{}
	for _, test := range []struct {
		runner string
		want   []string
	}{
		{"", []string{"claude-code", "codex", "codebuddy", "qoder", "opencode", "cursor"}},
		{"ssh-test", []string{"claude-code", "codex", "opencode"}},
	} {
		var got []string
		for _, agent := range server.conflictSuggestionAgents(Project{Runner: test.runner}) {
			got = append(got, agent.ID)
			entry, ok := agentByID(agent.ID)
			if !ok || agent.Name != entry.Name || !entry.RunnableInProject {
				t.Fatalf("invalid conflict agent: %+v", agent)
			}
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("runner %q agents=%v want=%v", test.runner, got, test.want)
		}
	}
	// 新增目录条目不能未经接线就出现在可执行列表。
	withExtraCatalogEntry(t, AgentCatalogEntry{ID: "future", Name: "Future", RunnableInProject: true})
	for _, agent := range server.conflictSuggestionAgents(Project{}) {
		if agent.ID == "future" {
			t.Fatal("unsupported catalog entry was advertised for conflict suggestions")
		}
	}
}

func TestConflictSuggestionAgentsRespectWindowsBridge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this case exercises a WSL/Linux service driving a Windows project")
	}
	server := &Server{}
	project := Project{Runner: "wsl-local", Path: "/mnt/c/project"}
	if target := server.resolveAgentTargetEnv(project.Runner, project.Path); target != agentTargetEnvWindows {
		t.Fatalf("test project must target Windows, got %s", target)
	}
	for _, agent := range server.conflictSuggestionAgents(project) {
		switch agent.ID {
		case "codebuddy", "qoder", "cursor", "opencode":
			t.Errorf("%s was advertised although its Windows bridge is not implemented", agent.ID)
		}
	}
	for _, id := range []string{"codebuddy", "qoder", "cursor", "opencode"} {
		if _, err := server.conflictSuggestionLocalRunner(project, id); err == nil {
			t.Errorf("%s accepted a project in another execution environment", id)
		}
	}
}

type conflictTestStreamingRunner struct {
	runnerFunc
	session        *conflictTestSession
	sessionRequest AgentSessionRequest
	startErr       error
}

func (runner *conflictTestStreamingRunner) StartSession(_ context.Context, request AgentSessionRequest) (AgentSession, error) {
	runner.sessionRequest = request
	return runner.session, runner.startErr
}

type conflictTestSession struct {
	request AgentRunRequest
	send    func(AgentRunSink) error
	stopped bool
	done    chan error
}

func (session *conflictTestSession) Send(request AgentRunRequest, sink AgentRunSink) error {
	session.request = request
	return session.send(sink)
}
func (session *conflictTestSession) Stop()              { session.stopped = true }
func (session *conflictTestSession) Done() <-chan error { return session.done }

func TestConflictSuggestionSessionAgentsReadOnlyAndStop(t *testing.T) {
	for _, agentID := range []string{"codebuddy", "qoder"} {
		for _, outcome := range []string{"completed", "failed", "cancelled", "early-exit", "send-error", "start-error"} {
			t.Run(agentID+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				session := &conflictTestSession{done: make(chan error, 1)}
				session.send = func(sink AgentRunSink) error {
					switch outcome {
					case "cancelled":
						cancel()
					case "early-exit":
						session.done <- nil
					case "send-error":
						return errors.New("send failed")
					default:
						sink.AssistantText(`{"merged":"resolved\n","explanation":"合并两侧"}`, "")
						var err error
						if outcome == "failed" {
							err = errors.New("turn failed")
						}
						sink.(AgentTurnSink).TurnFinished(err)
					}
					return nil
				}
				runner := &conflictTestStreamingRunner{session: session}
				if outcome == "start-error" {
					runner.startErr = errors.New("start failed")
				}
				server := &Server{codebuddyRunner: runner, qoderRunner: runner}
				project := Project{ID: "p", Path: t.TempDir()}
				record := &gitConflictSuggestion{ProjectID: "p", Agent: agentID, Path: "test.txt"}
				server.runConflictSuggestion(ctx, project, record, GitConflictContent{Working: "conflict"}, "main", "feature")
				wantStatus := gitConflictSuggestStatusFailed
				if outcome == "completed" {
					wantStatus = gitConflictSuggestStatusCompleted
					if record.Merged != "resolved\n" || record.Explanation != "合并两侧" {
						t.Fatalf("unexpected merge: %+v", record)
					}
				} else if outcome == "cancelled" {
					wantStatus = gitConflictSuggestStatusCancelled
				}
				if record.Status != wantStatus {
					t.Fatalf("status=%s want=%s error=%s", record.Status, wantStatus, record.ErrorMessage)
				}
				if outcome != "start-error" && !session.stopped {
					t.Fatal("temporary session was not stopped")
				}
				if runner.sessionRequest.PermissionMode != "read_only" || runner.sessionRequest.ProjectPath != project.Path {
					t.Fatalf("wrong session request: %+v", runner.sessionRequest)
				}
				if outcome != "start-error" && (session.request.AgentID != agentID || session.request.PermissionMode != "read_only" || !strings.Contains(session.request.Prompt, "conflict")) {
					t.Fatalf("wrong turn request: %+v", session.request)
				}
			})
		}
	}
}

func TestConflictSuggestionCursorUsesReadOnlyRunner(t *testing.T) {
	var request AgentRunRequest
	server := &Server{cursorRunner: runnerFunc(func(_ context.Context, req AgentRunRequest, sink AgentRunSink) error {
		request = req
		sink.AssistantText(`{"merged":"cursor result"}`, "")
		return nil
	})}
	text, err := server.runConflictSuggestionAgent(context.Background(), Project{Path: "workspace"}, "cursor", "merge")
	if err != nil || text != `{"merged":"cursor result"}` {
		t.Fatalf("cursor result=%q err=%v", text, err)
	}
	if request.AgentID != "cursor" || request.PermissionMode != "read_only" || request.ProjectPath != "workspace" {
		t.Fatalf("wrong Cursor request: %+v", request)
	}
}

func TestConflictSuggestionRejectsUnsupportedTargetsAndAgents(t *testing.T) {
	server := &Server{runner: runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error {
		t.Fatal("request fell back to Claude")
		return nil
	})}
	for _, agentID := range []string{"codebuddy", "qoder", "cursor", "unknown"} {
		if _, err := server.runConflictSuggestionAgent(context.Background(), Project{Runner: "ssh-test"}, agentID, "merge"); err == nil {
			t.Fatalf("remote/unsupported %s request was accepted", agentID)
		}
	}
	for _, agentID := range []string{"codebuddy", "qoder", "cursor"} {
		if _, err := server.runConflictSuggestionAgent(context.Background(), Project{}, agentID, "merge"); err == nil {
			t.Fatalf("missing %s runner was accepted", agentID)
		}
	}
}
