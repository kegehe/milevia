package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type cliIntegrationSink struct {
	events []struct {
		kind string
		data json.RawMessage
	}
	texts, parents []string
}

type cliFailedReader struct{}

func (cliFailedReader) Read([]byte) (int, error) { return 0, errors.New("fixture read failure") }

type cliFinishingSink struct {
	cliIntegrationSink
	finished chan error
}

func (*cliFinishingSink) TurnStarted()             {}
func (s *cliFinishingSink) TurnFinished(err error) { s.finished <- err }

func TestCLIProbeOutputCapStillDrainsBothPipes(t *testing.T) {
	if os.Getenv("MILEVIA_CLI_PROBE_OUTPUT_FIXTURE") == "1" {
		_, _ = os.Stdout.WriteString(strings.Repeat("out", 32768))
		_, _ = os.Stderr.WriteString(strings.Repeat("err", 32768))
		os.Exit(0)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestCLIProbeOutputCapStillDrainsBothPipes$")
	cmd.Env = append(os.Environ(), "MILEVIA_CLI_PROBE_OUTPUT_FIXTURE=1")
	stdout, stderr, err := runProbeCommandStreamsLimit(ctx, cmd, 1024)
	if err == nil || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("oversized probe output accepted: err=%v stdout=%d stderr=%d", err, len(stdout), len(stderr))
	}
	if ctx.Err() != nil {
		t.Fatal("bounded probe stopped draining its pipes")
	}
}

type heldCLIProcess struct{ stopped chan struct{} }

func (*heldCLIProcess) identity() supervisionProcessIdentity { return supervisionProcessIdentity{} }
func (*heldCLIProcess) resume() error                        { return nil }
func (p *heldCLIProcess) terminate() error {
	select {
	case p.stopped <- struct{}{}:
	default:
	}
	return nil
}
func (*heldCLIProcess) quiescent() (bool, error) { return false, nil }
func (*heldCLIProcess) wait() error              { return nil }
func (*heldCLIProcess) close()                   {}

func TestCLIStreamReadFailureFinishesActiveAndQueuedTurns(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	process := cmd.Process
	held := &heldCLIProcess{stopped: make(chan struct{}, 1)}
	state := &ordinaryCLIProcess{process: held, done: make(chan struct{})}
	ordinaryCLIProcesses.Store(process, state)
	defer ordinaryCLIProcesses.Delete(process)
	session := newStreamJSONSession(streamJSONSessionConfig{agentID: "codebuddy"}, cmd, &captureWriteCloser{}, io.NopCloser(cliFailedReader{}), io.NopCloser(strings.NewReader("")))
	first, second := &cliFinishingSink{finished: make(chan error, 1)}, &cliFinishingSink{finished: make(chan error, 1)}
	if err := session.Send(AgentRunRequest{Prompt: "one"}, first); err != nil {
		t.Fatal(err)
	}
	if err := session.Send(AgentRunRequest{Prompt: "two"}, second); err != nil {
		t.Fatal(err)
	}
	session.watch(context.Background())
	select {
	case <-held.stopped:
	case <-time.After(time.Second):
		t.Fatal("failed stream did not stop its writer")
	}
	select {
	case <-session.Done():
		t.Fatal("session ended before its container settled")
	case <-time.After(20 * time.Millisecond):
	}
	if err := session.Send(AgentRunRequest{Prompt: "three"}, newRecordingTurnSink()); err == nil {
		t.Fatal("broken stream accepted another turn")
	}
	close(state.done)
	for _, sink := range []*cliFinishingSink{first, second} {
		select {
		case err := <-sink.finished:
			if err == nil {
				t.Fatal("stream failure reported success")
			}
		case <-time.After(5 * time.Second): // race-instrumented child processes delay exit
			t.Fatal("failed stream left a turn active")
		}
	}
	if err := session.Send(AgentRunRequest{Prompt: "three"}, newRecordingTurnSink()); err == nil {
		t.Fatal("broken stream accepted another turn")
	}
}

func TestCLIStreamAcceptsLargeImageEchoAndDrainsLongStderr(t *testing.T) {
	session := newStreamJSONSession(streamJSONSessionConfig{agentID: "codebuddy"}, nil, &captureWriteCloser{}, nil, nil)
	sink := &cliIntegrationSink{}
	if err := session.Send(AgentRunRequest{Prompt: "image"}, sink); err != nil {
		t.Fatal(err)
	}
	image := `{"type":"user","message":{"content":[{"type":"image","source":{"type":"base64","data":"` + strings.Repeat("x", 17<<20) + `"}}]}}`
	session.readOutput(strings.NewReader(image + "\n" + `{"type":"result","subtype":"success"}`))
	if len(sink.events) != 2 || strings.Contains(string(sink.events[0].data), strings.Repeat("x", 100)) {
		t.Fatal("large image echo was lost or persisted")
	}
	stderr := strings.NewReader(strings.Repeat("x", 1<<20) + "\nfinal diagnostic")
	drainLines(stderr)
	if stderr.Len() != 0 {
		t.Fatal("long stderr line stopped pipe draining")
	}
}

func TestCLILiveContextUsageRemainsAvailableBeforeResult(t *testing.T) {
	usage := newRunUsageAccumulator("run", "conversation")
	usage.collect("assistant", json.RawMessage(`{"message":{"id":"m","usage":{"input_tokens":13,"cache_read_input_tokens":2}}}`))
	view := usage.snapshot("running", nil)
	if !view.Available || view.ContextInputTokens != 15 || view.HasResult {
		t.Fatalf("live context hidden: %+v", view)
	}
}

func TestCLINativeCatalogFollowsExecutionEnvironment(t *testing.T) {
	local, cross := &opencodeCLIRunner{}, &wslAgentRunner{}
	server := &Server{opencodeRunner: local, wslRunner: cross}
	if runtime.GOOS == "windows" {
		if server.nativeCatalogRunner("opencode", "wsl-local", `\\wsl.localhost\Ubuntu\home\p`) != cross {
			t.Fatal("WSL probe did not select execution runner")
		}
		if server.nativeCatalogRunner("opencode", "windows-local", `D:\p`) != local {
			t.Fatal("local probe selected a remote runner")
		}
	} else {
		if server.nativeCatalogRunner("opencode", "wsl-local", "/mnt/c/project") != nil {
			t.Fatal("unsupported Windows target fell back to Linux CLI")
		}
		if server.nativeCatalogRunner("opencode", "wsl-local", "/home/project") != local {
			t.Fatal("local Linux target not selected")
		}
	}
}

func TestCLIUnsupportedImageTypeUsesPathReference(t *testing.T) {
	file := AgentAttachment{Path: "design.svg", MediaType: "image/svg+xml"}
	for _, resume := range []bool{false, true} {
		request := AgentRunRequest{Attachments: []AgentAttachment{file}, Resume: resume, SessionID: "session"}
		if containsArgument(codexExecArgs(request, nil, "", "read-only"), "--image") {
			t.Fatal("SVG passed as native image")
		}
		if strings.Contains(buildSSHCodexExecCommand(request, "", "sandbox_mode=read-only", "", "read-only"), "--image") {
			t.Fatal("SSH SVG passed as native image")
		}
	}
	if nativeImageMediaType("image/svg+xml") || !nativeImageMediaType("image/png") {
		t.Fatal("image formats misclassified")
	}
	// A directory must be rejected before attempting to open a read stream.
	fs := &LocalFilesystem{projectPath: t.TempDir()}
	if reader, _, err := fs.OpenRead(context.Background(), "."); err == nil {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatal("directory opened as attachment")
	}
}

func (s *cliIntegrationSink) Event(kind string, raw json.RawMessage) {
	s.events = append(s.events, struct {
		kind string
		data json.RawMessage
	}{kind, append(json.RawMessage(nil), raw...)})
}
func (s *cliIntegrationSink) AssistantText(text, parent string) {
	s.texts = append(s.texts, text)
	s.parents = append(s.parents, parent)
}
func (*cliIntegrationSink) SessionIdentified(string) {}
func (*cliIntegrationSink) SessionInitialized()      {}

func TestCLISharedStreamPreservesToolsUsageAndParent(t *testing.T) {
	for _, agent := range []string{"codebuddy", "qoder"} {
		t.Run(agent, func(t *testing.T) {
			sink := &cliIntegrationSink{}
			session := newStreamJSONSession(streamJSONSessionConfig{agentID: agent}, nil, &captureWriteCloser{}, nil, nil)
			if err := session.Send(AgentRunRequest{Prompt: "inspect", AgentID: agent}, sink); err != nil {
				t.Fatal(err)
			}
			session.readOutput(strings.NewReader(strings.Join([]string{
				`{"type":"system","subtype":"init","session_id":"native","slash_commands":["compact"]}`,
				`{"type":"assistant","parent_tool_use_id":"parent","message":{"id":"message","usage":{"input_tokens":3},"content":[{"type":"tool_use","id":"tool","name":"Read","input":{"path":"README.md"}},{"type":"text","text":"child reply"}]}}`,
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool","content":"file result"}]}}`,
				`{"type":"result","subtype":"success","num_turns":1,"usage":{"input_tokens":17,"output_tokens":4}}`,
			}, "\n")))
			if len(sink.events) != 4 || len(sink.parents) != 1 || sink.parents[0] != "parent" {
				t.Fatalf("events/parent lost: %+v", sink)
			}
			usage := newRunUsageAccumulator("run", "conversation")
			for _, event := range sink.events {
				usage.collect(event.kind, event.data)
			}
			if usage.inputTokens != 17 || usage.outputTokens != 4 || usage.toolCalls != 1 || len(usage.parentIDs) != 1 {
				t.Fatalf("usage lost: %+v", usage)
			}
		})
	}
}

func TestCLIOpenCodeMultiStepUsageForwardedAndDeduplicated(t *testing.T) {
	first := `{"type":"step_finish","part":{"id":"step1","reason":"tool-calls","tokens":{"input":10,"output":2,"cache":{"read":3,"write":1}}}}`
	last := `{"type":"step_finish","part":{"id":"step2","reason":"stop","tokens":{"input":17,"output":4,"cache":{"read":5,"write":2}}}}`
	sink := &cliIntegrationSink{}
	readOpenCodeJSONL(strings.NewReader(first+"\n"+last+"\n"+last), sink, newOpenCodeCompletion())
	usage := newRunUsageAccumulator("run", "conversation")
	for _, event := range sink.events {
		usage.collect(event.kind, event.data)
	}
	if usage.inputTokens != 27 || usage.outputTokens != 6 || usage.cacheRead != 8 || usage.contextInput != 24 || usage.modelSteps != 2 || usage.agentTurns != 1 {
		t.Fatalf("incorrect step aggregation: %+v", usage)
	}
}

func TestCLIReasoningSettingsReachArguments(t *testing.T) {
	for _, agent := range []string{"opencode", "qoder", "codebuddy", "claude-code"} {
		t.Run(agent, func(t *testing.T) {
			server := newTestServer(t)
			seedModelConversation(t, server, agent, "idle")
			response := httptest.NewRecorder()
			server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/reasoning-effort", strings.NewReader(`{"reasoningEffort":"high"}`)))
			if response.Code != http.StatusOK {
				t.Fatalf("setting failed: %d %s", response.Code, response.Body.String())
			}
			effort := server.conversationReasoningEffort(context.Background(), "model-conversation")
			var args []string
			switch agent {
			case "qoder":
				args = qoderSessionArgs(AgentSessionRequest{ReasoningEffort: effort})
			case "codebuddy":
				args = codebuddySessionArgs(AgentSessionRequest{ReasoningEffort: effort})
			case "opencode":
				args, _ = opencodeExecArgs(AgentRunRequest{Prompt: "inspect", ReasoningEffort: effort})
			default:
				args = []string{effort}
			}
			if !containsArgument(args, "high") {
				t.Fatalf("effort lost: %v", args)
			}
		})
	}
}

func TestCLICodeBuddyCatalogFromRunAndPermissionSeparation(t *testing.T) {
	server := newTestServer(t)
	projectID := createCommandTestProject(t, server, t.TempDir())
	server.observeCommandCatalog(projectID, "codebuddy", []byte(`{"type":"system","subtype":"init","slash_commands":["buddy-only"],"terminal_slash_commands":["buddy-only"]}`))
	view := projectCommandsView(t, server, projectID, "agentId=codebuddy")
	if !view.Authoritative || len(view.Commands) != 1 || view.Commands[0].Name != "buddy-only" || !view.Commands[0].TerminalOnly {
		t.Fatalf("catalog lost: %+v", view)
	}
	write := codebuddySessionArgs(AgentSessionRequest{PermissionMode: "workspace_write"})
	full := codebuddySessionArgs(AgentSessionRequest{PermissionMode: "full_control"})
	if containsArgument(write, "--dangerously-skip-permissions") || !containsArguments(write, "--permission-mode", "acceptEdits") || !containsArgument(full, "--dangerously-skip-permissions") {
		t.Fatalf("permissions conflated: %v / %v", write, full)
	}
}

func TestCLIAttachmentPersistenceDispatchHistoryAndIdempotency(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "claude-code", "idle")
	var root string
	if err := server.db.QueryRow(`select path from projects where id='model-project'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	// Old databases and tests may not have a workspace row until first access.
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSharedConversationWorkspaceTx(context.Background(), tx, "model-conversation", "model-project", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	requests := make(chan AgentRunRequest, 1)
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		requests <- request
		return nil
	})
	body := `{"content":"inspect file","clientRequestId":"attachment-request","attachments":[{"path":"sample.txt","mediaType":"fake/type","size":99}]}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/messages", strings.NewReader(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", response.Code, response.Body.String())
	}
	var sent struct {
		Message Message
		RunID   string `json:"runId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		if len(request.Attachments) != 1 || request.Attachments[0].Path != "sample.txt" || !strings.Contains(request.Prompt, "@sample.txt") {
			t.Fatalf("not delivered: %+v", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachment not dispatched")
	}
	waitForConversationIdle(t, server, "model-conversation")
	if len(sent.Message.Attachments) != 1 || sent.Message.Attachments[0].Size != 10 {
		t.Fatalf("client metadata was trusted: %+v", sent.Message)
	}
	if err := os.Remove(filepath.Join(root, "sample.txt")); err != nil {
		t.Fatal(err)
	}
	replay := httptest.NewRecorder()
	server.routes().ServeHTTP(replay, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/messages", strings.NewReader(body)))
	if replay.Code != http.StatusAccepted {
		t.Fatalf("idempotency: %s", replay.Body.String())
	}
	history := httptest.NewRecorder()
	server.routes().ServeHTTP(history, httptest.NewRequest(http.MethodGet, "/api/conversations/model-conversation", nil))
	if history.Code != 200 || !strings.Contains(history.Body.String(), `"attachments":[{"path":"sample.txt"`) {
		t.Fatalf("history lost attachments: %d %s", history.Code, history.Body.String())
	}
	conflict := httptest.NewRecorder()
	server.routes().ServeHTTP(conflict, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/messages", strings.NewReader(strings.ReplaceAll(body, "sample.txt", "other.txt"))))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("changed attachment retry accepted: %d %s", conflict.Code, conflict.Body.String())
	}
	missing := httptest.NewRecorder()
	server.routes().ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/messages", strings.NewReader(strings.ReplaceAll(body, "attachment-request", "new-request"))))
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing file accepted: %d %s", missing.Code, missing.Body.String())
	}
}

func TestCLINativeCatalogRejectsProseAndPreservesIDs(t *testing.T) {
	models := parseNativeModelCatalog("opencode", "Loading providers...\nhttps://example.com/error\n./config/file\n../config/file\nopenai/gpt-test\nanthropic/claude-test\ncustom.provider/model\nopenai/gpt-test\nnot logged in\n")
	if len(models) != 3 || models[0].ID != "anthropic/claude-test" || models[1].ID != "custom.provider/model" {
		t.Fatalf("catalog: %+v", models)
	}
	if len(parseNativeModelCatalog("qoder", "Not logged in. Please run /login")) != 0 {
		t.Fatal("error prose became a model")
	}
	models = parseNativeModelCatalog("qoder", `{"models":[{"id":"auto","label":"Auto"},{"id":"bad;command"}]}`)
	if len(models) != 1 || models[0].ID != "auto" {
		t.Fatalf("structured catalog: %+v", models)
	}
}

func TestCLIOpenCodeCustomVariantAndMissingUsage(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "opencode", "idle")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/model-conversation/reasoning-effort", strings.NewReader(`{"reasoningEffort":"provider-Custom"}`)))
	if response.Code != http.StatusOK || server.conversationReasoningEffort(context.Background(), "model-conversation") != "provider-Custom" {
		t.Fatalf("custom variant: %d %s", response.Code, response.Body.String())
	}
	usage := newRunUsageAccumulator("r", "c")
	if usage.collect("opencode.step_finish", json.RawMessage(`{"part":{"id":"step","reason":"stop"}}`)) || usage.hasResult {
		t.Fatal("absent usage became a known zero")
	}
	if usage.snapshot("completed", nil).Available {
		t.Fatal("missing tokens reported available")
	}
	usage.collect("result", json.RawMessage(`{"type":"result","subtype":"success"}`))
	if usage.snapshot("completed", nil).Available {
		t.Fatal("missing result usage became a known zero")
	}
	usage.collect("turn.completed", json.RawMessage(`{"type":"turn.completed"}`))
	if usage.snapshot("completed", nil).Available {
		t.Fatal("missing Codex usage became a known zero")
	}
	usage.collect("turn.completed", json.RawMessage(`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0}}`))
	if !usage.snapshot("completed", nil).Available {
		t.Fatal("explicit zero usage was treated as missing")
	}
}

func TestCLIRemoteAttachmentOnlyMessageAndSnapshot(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "claude-code", "idle")
	var root string
	if err := server.db.QueryRow(`select path from projects where id='model-project'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "remote.txt"), []byte("remote attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := make(chan AgentRunRequest, 1)
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		requests <- request
		return nil
	})
	value, err := server.executeRemoteCommand(context.Background(), remoteCommand{
		Type:    "conversation.message",
		Payload: json.RawMessage(`{"conversationId":"model-conversation","content":"","clientRequestId":"remote-attachment","attachments":[{"path":"remote.txt"}]}`),
	})
	if err != nil {
		t.Fatalf("remote send: %v", err)
	}
	if !strings.Contains(string(mustJSON(value)), `"path":"remote.txt"`) {
		t.Fatalf("remote acknowledgement lost attachment: %s", mustJSON(value))
	}
	if !strings.Contains(string(mustJSON(value)), `"clientRequestId":"remote-attachment"`) {
		t.Fatalf("remote acknowledgement lost request identity: %s", mustJSON(value))
	}
	select {
	case request := <-requests:
		if len(request.Attachments) != 1 || !strings.Contains(request.Prompt, "@remote.txt") {
			t.Fatalf("remote dispatch lost attachment: %+v", request)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote attachment not dispatched")
	}
	waitForConversationIdle(t, server, "model-conversation")
	response := httptest.NewRecorder()
	server.remoteSnapshot(response, httptest.NewRequest(http.MethodGet, "/api/remote/snapshot", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"attachments":[{"path":"remote.txt"`) {
		t.Fatalf("remote snapshot lost attachment: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"clientRequestId":"remote-attachment"`) {
		t.Fatalf("remote snapshot lost request identity: %s", response.Body.String())
	}
}

func TestCLISharedStreamRedactsCredentialsAndImagePayload(t *testing.T) {
	session := newStreamJSONSession(streamJSONSessionConfig{agentID: "qoder"}, nil, &captureWriteCloser{}, nil, nil)
	sink := &cliIntegrationSink{}
	if err := session.Send(AgentRunRequest{Prompt: "inspect"}, sink); err != nil {
		t.Fatal(err)
	}
	session.readOutput(strings.NewReader(`{"type":"user","api_key":"secret-value","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"PRIVATE-BASE64"}}]}}`))
	raw := string(mustJSON(sink.events[0].data))
	if strings.Contains(raw, "secret-value") || strings.Contains(raw, "PRIVATE-BASE64") || !strings.Contains(raw, "REDACTED_IMAGE") {
		t.Fatalf("event payload leaked: %s", raw)
	}
	if validateAttachmentShimArgs("opencode.cmd", []AgentAttachment{{Path: "file&command.png"}}) == nil {
		t.Fatal("batch metacharacter was accepted")
	}
}

func TestCLIInitBeforeSendAndPermissionControl(t *testing.T) {
	stdin := &captureWriteCloser{}
	session := newStreamJSONSession(streamJSONSessionConfig{agentID: "codebuddy", denyPermissionRequests: true}, nil, stdin, nil, nil)
	session.readOutput(strings.NewReader(`{"type":"system","subtype":"init","session_id":"native","slash_commands":["compact"]}`))
	sink := &cliIntegrationSink{}
	if err := session.Send(AgentRunRequest{Prompt: "hello"}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 || sink.events[0].kind != "system" {
		t.Fatalf("init before sink was lost: %+v", sink)
	}
	session.readOutput(strings.NewReader(`{"type":"control_request","request_id":"req","request":{"subtype":"can_use_tool","tool_name":"Bash","tool_use_id":"tool"}}`))
	if !strings.Contains(stdin.String(), `"allowed":false`) || !strings.Contains(stdin.String(), `"request_id":"req"`) {
		t.Fatalf("permission request left hanging: %s", stdin.String())
	}
}

func TestCLIConfiguredPluginsOnlyExposePluginMetadata(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode.jsonc")
	if err := os.WriteFile(p, []byte(`{// comment
"plugin":["@org/plugin@1.0.0", "@org/plugin@1.0.0", "https://user:secret@host/plugin?token=secret"],
"provider":{"apiKey":"secret"},}`), 0600); err != nil {
		t.Fatal(err)
	}
	plugins := configuredOpenCodePlugins([]string{p})
	if len(plugins) != 1 || plugins[0].Name != "@org/plugin@1.0.0" || plugins[0].Status != "configured" {
		t.Fatalf("plugins: %+v", plugins)
	}
	if strings.Contains(string(mustJSON(plugins)), "secret") {
		t.Fatal("configuration secret leaked")
	}
}

func TestCLIAttachmentUsesAdmittedWorkspaceAndChecksSymlinks(t *testing.T) {
	server := newTestServer(t)
	seedModelConversation(t, server, "claude-code", "idle")
	workspace, outside := t.TempDir(), t.TempDir()
	png := []byte("\x89PNG\r\n\x1a\nfixture")
	if err := os.WriteFile(filepath.Join(workspace, "picture.png"), png, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved := resolvedConversationWorkspace{Project: Project{Runner: server.localRunnerID()}, Workspace: ConversationWorkspace{Path: workspace}}
	validated, err := server.validateMessageAttachments(context.Background(), resolved, []MessageAttachment{{Path: "picture.png", MediaType: "fake", Size: 100}})
	if err != nil || len(validated) != 1 || validated[0].Size != int64(len(png)) {
		t.Fatalf("workspace validation: %+v %v", validated, err)
	}
	_, err = server.db.Exec(`insert into messages(id,conversation_id,run_id,role,content,created_at,attachments) values('file-message','model-conversation','file-run','user','',?,?)`, time.Now().UTC(), attachmentJSON(validated))
	if err != nil {
		t.Fatal(err)
	}
	files, err := server.runAttachments(context.Background(), "file-run", "claude-code", server.localRunnerID(), workspace)
	if err != nil || len(files) != 1 || files[0].Data != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("native image read from wrong root: %+v %v", files, err)
	}
	_, err = server.runAttachments(context.Background(), "file-run", "claude-code", server.localRunnerID(), outside)
	if err == nil {
		t.Fatal("execution silently read outside the admitted workspace")
	}
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(workspace, "escape.txt")); err != nil {
		t.Logf("symlink fixture unavailable: %v", err)
		return
	}
	if _, err := server.validateMessageAttachments(context.Background(), resolved, []MessageAttachment{{Path: "escape.txt"}}); err == nil {
		t.Fatal("symlink escaped workspace")
	}
}

func TestCLIAttachmentPathsAndNativeMapping(t *testing.T) {
	for _, bad := range []string{"../outside", "a/../../b", "C:/secret", "/etc/passwd", "a\ncommand", "\\\\host\\file"} {
		if _, err := validateAttachmentPath(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	image := AgentAttachment{Path: "image with spaces.png", MediaType: "image/png", Data: "aW1hZ2U="}
	frame, err := encodeClaudeUserFrame("inspect", image)
	if err != nil || !strings.Contains(string(frame), `"type":"image"`) || !strings.Contains(string(frame), `"media_type":"image/png"`) {
		t.Fatalf("image block missing: %s %v", frame, err)
	}
	for _, resume := range []bool{false, true} {
		args := codexExecArgs(AgentRunRequest{Attachments: []AgentAttachment{image}, Resume: resume, SessionID: "session"}, nil, "", "read-only")
		if !containsArguments(args, "--image", image.Path) {
			t.Fatalf("image flag missing: %v", args)
		}
		cmd := buildSSHCodexExecCommand(AgentRunRequest{Attachments: []AgentAttachment{image}, Resume: resume}, "", "sandbox_mode=read-only", "", "read-only")
		if !strings.Contains(cmd, "--image "+shellQuote(image.Path)) {
			t.Fatalf("SSH image flag lost: %s", cmd)
		}
	}
	args, err := opencodeExecArgs(AgentRunRequest{Prompt: "inspect", Attachments: []AgentAttachment{image}})
	if err != nil || !containsArguments(args, "--file", image.Path) {
		t.Fatalf("file flag missing: %v %v", args, err)
	}
}

func TestCLIAllNativeSkillRootsAndPluginDiscovery(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	for _, dir := range []string{".claude/skills", ".agents/skills", ".codex/skills", ".codebuddy/skills", ".qoder/skills", ".opencode/skills", ".cursor/skills"} {
		makeSkillTree(t, project, dir, "native-skill", "project skill", false)
	}
	roots := skillRootsForPaths(home, project, "windows", "", "")
	skills := resolveSkillScans(scanLocalSkills(context.Background(), roots))
	for _, agent := range []string{"claude-code", "codex", "codebuddy", "qoder", "opencode", "cursor"} {
		found := false
		for _, skill := range skills {
			if skill.Agent == agent && skill.Name == "native-skill" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s", agent)
		}
	}
	dir := filepath.Join(home, ".codebuddy/plugins/cache/demo/.codebuddy-plugin")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"demo","version":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	plugins := scanPluginManifests(context.Background(), home, "codebuddy", ".codebuddy-plugin/plugin.json")
	if len(plugins) != 1 || plugins[0].Status != "discovered" {
		t.Fatalf("plugin discovery: %+v", plugins)
	}
}
