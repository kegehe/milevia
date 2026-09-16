package app

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// MCP 探针客户端（P1）。
//
// 目标是「在目标环境里真实拉起 server 并列出工具」——这是抵御工具投毒最有效的一环：
// 让用户在授权前看清 server 声称自己能做什么（见 docs/34 §10.4）。
//
// 传输实现分三类：
//   - stdio + Windows 本地：直接 exec，交互式握手（先 initialize，再 tools/list）。
//   - stdio + WSL：经 wsl.exe 包裹同一个命令，同样是交互式握手。
//   - stdio + SSH 远端：远端 session 没有交互式通道，改用「流水线」——一次性把
//     initialize / notifications/initialized / tools/list 三条消息写入 stdin 后关闭，
//     再读回 stdout，按 id 取 tools/list 的响应。对顺序处理的 server 等价可用。
//   - http / sse：由控制服务直连（Streamable HTTP）。SSE 传输已被 MCP 规范标记为
//     过时，这里按同一套 POST 语义处理；若对端只支持旧式 SSE 端点，会返回可读错误。
//
// 所有路径都以「宁可失败并给出可读原因，也不静默回退到别的环境」为原则。

const (
	// mcpProtocolVersion 是探针请求的协议版本。
	mcpProtocolVersion = "2025-06-18"
	// mcpDefaultProbeTimeout 是探针的默认总超时。
	mcpDefaultProbeTimeout = 25 * time.Second
	// mcpProbeStdioTimeout 是 stdio 握手中单步等待响应的超时。
	mcpProbeStdioTimeout = 12 * time.Second
	// mcpMaxProbeTools 是一次 tools/list 最多渲染的工具数，避免超大列表拖垮前端。
	mcpMaxProbeTools = 300
	// mcpMaxProbeResources 是 resources/prompts 列表的渲染上限。
	mcpMaxProbeResources = 200
	// mcpProbeOptionalTimeout 是查询 resources/list 与 prompts/list 的等待上限。
	// 这两个原语是可选的：不支持的 server 可能返回 -32601，也可能干脆不回应，
	// 因此用一个较短的共享超时，避免不支持的实现把测试拖长。
	mcpProbeOptionalTimeout = 4 * time.Second
	// mcpMaxProbeBytes 限制读回的响应体大小。
	mcpMaxProbeBytes = 4 << 20
)

type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// mcpToolInfo 是工具列表中的一项（含可疑模式标注）。
type mcpToolInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// RequiresInteraction 来自 _meta["anthropic/requiresUserInteraction"]。
	// 带此标记的工具即使命中自动放行白名单，在 -p 模式下仍会被 Claude 拒绝——
	// hook-allow 无法覆盖，因此必须在 UI 里给出可操作的原因（docs/34 §8.5）。
	RequiresInteraction bool          `json:"requiresInteraction,omitempty"`
	Flags               []mcpToolFlag `json:"flags,omitempty"`
}

// mcpToolFlag 是工具元数据里命中的可疑模式或需要用户知晓的能力标记。
type mcpToolFlag struct {
	Code     string `json:"code"`
	Label    string `json:"label"`
	Detail   string `json:"detail,omitempty"`
	Severity string `json:"severity"` // info | warn | danger
	// Note 是解释性说明（而非命中的原文片段）。前端对 Note 直显，对 Detail 加
	// 「命中片段：」前缀——两者语义不同，混用会让用户误读。
	Note string `json:"note,omitempty"`
}

// mcpResourceInfo 是 resources/list 中的一项（仅 Claude Code 会用到）。
type mcpResourceInfo struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// mcpPromptInfo 是 prompts/list 中的一项（仅 Claude Code 会用到）。
type mcpPromptInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
}

// mcpProbeOutcome 是一次成功握手的返回值。
type mcpProbeOutcome struct {
	ProtocolVersion string
	ServerInfo      json.RawMessage
	Capabilities    json.RawMessage
	Tools           []mcpToolInfo
	Resources       []mcpResourceInfo
	Prompts         []mcpPromptInfo
}

// mcpProbeRequest 是探针的输入（由 HTTP handler 组装）。
type mcpProbeRequest struct {
	Transport string
	Command   string
	Args      []string
	Env       map[string]string
	Cwd       string
	URL       string
	Headers   map[string]string
	// Environment 是目标环境；决定 stdio 用哪条拉起通道。
	Environment agentTargetEnv
	// ProjectPath 是 stdio 的工作目录（已按目标环境解析为对应形态）。
	ProjectPath string
	// ConnID 仅在 Environment=remote-linux 时使用（SSH 连接 id）。
	ConnID  string
	Timeout time.Duration
}

// probeMCPServer 在目标环境执行一次 initialize + tools/list。
func (s *Server) probeMCPServer(ctx context.Context, req mcpProbeRequest) (mcpProbeOutcome, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = mcpDefaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch req.Transport {
	case mcpTransportStdio:
		return s.probeMCPStdio(ctx, req)
	case mcpTransportHTTP, mcpTransportSSE:
		return probeMCPHTTP(ctx, req, nil)
	default:
		return mcpProbeOutcome{}, fmt.Errorf("不支持的传输类型：%s", req.Transport)
	}
}

// probeMCPStdio 在目标环境拉起 stdio server 并握手。
func (s *Server) probeMCPStdio(ctx context.Context, req mcpProbeRequest) (mcpProbeOutcome, error) {
	if strings.TrimSpace(req.Command) == "" {
		return mcpProbeOutcome{}, errors.New("stdio server 未配置启动命令")
	}
	switch req.Environment {
	case agentTargetEnvWindows:
		if runtime.GOOS != "windows" {
			return mcpProbeOutcome{}, errors.New("控制服务不在 Windows 上，无法在 Windows 环境测试该 server")
		}
		cmd := exec.CommandContext(ctx, req.Command, req.Args...)
		cmd.Dir = req.ProjectPath
		cmd.Env = mergedProbeEnv(req.Env)
		configureProcessGroup(cmd)
		return probeStdioInteractive(ctx, cmd, "（本地）")

	case agentTargetEnvWSL:
		if runtime.GOOS != "windows" {
			return mcpProbeOutcome{}, errors.New("控制服务不在 Windows 上，无法在 WSL 环境测试该 server")
		}
		w := s.wslAgentRunner()
		runner, ok := w.(*wslAgentRunner)
		if !ok || runner == nil {
			return mcpProbeOutcome{}, errors.New("未探测到可用的 WSL 发行版，无法在 WSL 环境测试该 server")
		}
		linuxDir := req.ProjectPath
		if linuxDir != "" {
			if converted, ok := uncToWslPath(linuxDir, runner.distro); ok {
				linuxDir = converted
			} else {
				linuxDir = windowsToWSLMntPath(linuxDir)
			}
		}
		cmd := runner.wslNativeCommand(ctx, req.Command, req.Args, envPairs(req.Env), linuxDir)
		return probeStdioInteractive(ctx, cmd, "（WSL）")

	case agentTargetEnvRemote:
		if strings.TrimSpace(req.ConnID) == "" {
			return mcpProbeOutcome{}, errors.New("远端测试需要指定 SSH 连接")
		}
		client, err := s.sshClientForConnection(req.ConnID)
		if err != nil {
			return mcpProbeOutcome{}, err
		}
		return probeStdioPipelined(ctx, func(payload []byte) ([]byte, error) {
			command := buildRemoteStdioProbeCommand(req)
			out, err := client.execCommandWithStdin(ctx, command, payload)
			if err != nil {
				return nil, err
			}
			return out, nil
		}, "（远端）")
	default:
		return mcpProbeOutcome{}, fmt.Errorf("不支持的目标环境：%s", req.Environment)
	}
}

// sshClientForConnection 取指定 SSH 连接当前注册的 runner 及其底层 client。
// 未连接（或未注册）时返回可读错误，由调用方提示用户先建立连接。
func (s *Server) sshClientForConnection(connID string) (*sshClient, error) {
	runner, ok := s.runnerRegistry.get("ssh-" + connID)
	if !ok || runner == nil {
		return nil, errors.New("该 SSH 连接当前未建立，请先在「SSH连接」中连接后再测试")
	}
	ssh, ok := runner.(*sshRunner)
	if !ok || ssh == nil || ssh.client == nil {
		return nil, errors.New("该远端 runner 不可用于 MCP 测试")
	}
	return ssh.client, nil
}

// buildRemoteStdioProbeCommand 组装远端流水线探针命令：把命令与参数经 base64 传给 sh，
// 避免引号/空格/非 ASCII 在远端 shell 被重解释。远端没有安全 env 通道（见 docs/34 §0.4），
// 因此 env 以 `env KEY=VAL` 前缀形式内联——这一点与「密钥不进 argv」原则冲突，故仅在
// 用户主动点击「测试连接」时发生，且不落盘、不写日志。
func buildRemoteStdioProbeCommand(req mcpProbeRequest) string {
	parts := []string{}
	for _, key := range sortedKeys(req.Env) {
		parts = append(parts, key+"="+shellQuote(req.Env[key]))
	}
	script := ""
	// 未指定工作目录时不要拼 `cd ''` —— 那会让整个探测脚本以「没有那个文件或目录」失败，
	// 错误还指向 cwd（草稿态试连常常没有项目，很容易踩到）。
	if req.ProjectPath != "" {
		script = "cd " + shellQuote(req.ProjectPath) + " && "
	}
	if len(parts) > 0 {
		script += "env " + strings.Join(parts, " ") + " "
	}
	script += shellQuote(req.Command)
	for _, arg := range req.Args {
		script += " " + shellQuote(arg)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return fmt.Sprintf("printf '%%s' %s | base64 -d | sh", shellQuote(encoded))
}

// probeStdioInteractive 启动进程后按 MCP 生命周期逐步握手：initialize → 等响应 →
// notifications/initialized + tools/list → 等响应。比流水线更贴近规范，能兼容要求
// 「先完成 initialize 才接受后续请求」的实现。
func probeStdioInteractive(ctx context.Context, cmd *exec.Cmd, label string) (mcpProbeOutcome, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return mcpProbeOutcome{}, fmt.Errorf("创建 stdin 失败：%w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return mcpProbeOutcome{}, fmt.Errorf("创建 stdout 失败：%w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return mcpProbeOutcome{}, fmt.Errorf("启动 MCP server 失败%s：%w", label, err)
	}
	defer forceTerminateProcessGroup(cmd)

	reader := bufio.NewReaderSize(stdout, 1<<16)

	if err := writeJSONRPCLine(stdin, mcpInitializeMessage()); err != nil {
		return mcpProbeOutcome{}, fmt.Errorf("发送 initialize 失败：%w", err)
	}
	initMsg, err := readMCPResponse(ctx, reader, "1", mcpProbeStdioTimeout)
	if err != nil {
		return mcpProbeOutcome{}, withStderrHint(err, &stderrBuf, label)
	}
	_ = writeJSONRPCLine(stdin, mcpInitializedNotification())
	_ = writeJSONRPCLine(stdin, mcpToolsListMessage())
	toolsMsg, err := readMCPResponse(ctx, reader, "2", mcpProbeStdioTimeout)
	if err != nil {
		return mcpProbeOutcome{}, withStderrHint(err, &stderrBuf, label)
	}
	if toolsMsg.Error != nil {
		return mcpProbeOutcome{}, fmt.Errorf("MCP server 返回错误：%s", toolsMsg.Error.Message)
	}
	outcome, err := buildProbeOutcome(initMsg, toolsMsg)
	if err != nil {
		return mcpProbeOutcome{}, err
	}
	// resources / prompts 是可选原语：不支持的 server 会返回 -32601 或干脆不回应，
	// 因此共用一个短超时，且不因失败影响已拿到的工具列表。
	_ = writeJSONRPCLine(stdin, mcpResourcesListMessage())
	_ = writeJSONRPCLine(stdin, mcpPromptsListMessage())
	applyMCPOptionalLists(&outcome, readMCPResponses(ctx, reader, []string{"3", "4"}, mcpProbeOptionalTimeout))
	return outcome, nil
}

// probeStdioPipelined 用于没有交互式通道的远端：一次性写入三条消息后关闭 stdin，再读回
// 全部 stdout，取 tools/list 的响应。要求对端顺序处理（规范实现均如此）。
func probeStdioPipelined(ctx context.Context, run func([]byte) ([]byte, error), label string) (mcpProbeOutcome, error) {
	payload := bytes.Join([][]byte{
		mcpInitializeMessage(),
		mcpInitializedNotification(),
		mcpToolsListMessage(),
		mcpResourcesListMessage(),
		mcpPromptsListMessage(),
	}, []byte("\n"))
	payload = append(payload, '\n')

	type runResult struct {
		out []byte
		err error
	}
	ch := make(chan runResult, 1)
	go func() {
		out, err := run(payload)
		ch <- runResult{out: out, err: err}
	}()
	var out []byte
	select {
	case res := <-ch:
		if res.err != nil {
			return mcpProbeOutcome{}, fmt.Errorf("远端执行失败%s：%w", label, res.err)
		}
		out = res.out
	case <-time.After(mcpProbeStdioTimeout):
		return mcpProbeOutcome{}, fmt.Errorf("远端执行超时%s", label)
	case <-ctx.Done():
		return mcpProbeOutcome{}, ctx.Err()
	}
	if len(out) > mcpMaxProbeBytes {
		out = out[:mcpMaxProbeBytes]
	}
	byID := map[string]jsonrpcMessage{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var msg jsonrpcMessage
		if json.Unmarshal(trimmed, &msg) != nil {
			continue
		}
		if len(msg.ID) > 0 {
			byID[string(msg.ID)] = msg
		}
	}
	toolsMsg, foundTools := byID["2"]
	if !foundTools {
		return mcpProbeOutcome{}, fmt.Errorf("远端 MCP server 未返回 tools/list 响应%s；请确认该命令在远端已安装，且不是把日志写到 stdout", label)
	}
	if toolsMsg.Error != nil {
		return mcpProbeOutcome{}, fmt.Errorf("远端 MCP server 返回错误：%s", toolsMsg.Error.Message)
	}
	initMsg, foundInit := byID["1"]
	if !foundInit {
		initMsg = jsonrpcMessage{}
	}
	outcome, err := buildProbeOutcome(initMsg, toolsMsg)
	if err != nil {
		return mcpProbeOutcome{}, err
	}
	applyMCPOptionalLists(&outcome, map[string]jsonrpcMessage{"3": byID["3"], "4": byID["4"]})
	return outcome, nil
}

// readMCPResponses 在同一个 reader 上等待若干可选响应，任一到来即记录；全部到齐或超时即返回。
// 超时不是错误：可选原语不被支持时不应影响主流程。
func readMCPResponses(ctx context.Context, reader *bufio.Reader, wantIDs []string, timeout time.Duration) map[string]jsonrpcMessage {
	wanted := map[string]bool{}
	for _, id := range wantIDs {
		wanted[id] = true
	}
	if len(wanted) == 0 {
		return map[string]jsonrpcMessage{}
	}
	ch := make(chan map[string]jsonrpcMessage, 1)
	go func() {
		found := map[string]jsonrpcMessage{}
		remaining := len(wanted)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				trimmed := bytes.TrimSpace(line)
				if len(trimmed) > 0 && trimmed[0] == '{' {
					var msg jsonrpcMessage
					if json.Unmarshal(trimmed, &msg) == nil {
						if key := string(msg.ID); wanted[key] {
							if _, seen := found[key]; !seen {
								remaining--
							}
							found[key] = msg
							if remaining <= 0 {
								ch <- found
								return
							}
						}
					}
				}
			}
			if err != nil {
				ch <- found
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case found := <-ch:
		return found
	case <-timer.C:
		return map[string]jsonrpcMessage{}
	case <-ctx.Done():
		return map[string]jsonrpcMessage{}
	}
}

func writeJSONRPCLine(w io.Writer, payload []byte) error {
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return err
	}
	return nil
}

// readMCPResponse 逐行读到指定 id 的响应。忽略非 JSON 行（不少 server 会往 stdout 打日志）
// 与其它 id 的通知。等待受 ctx 与 timeout 双重约束。
func readMCPResponse(ctx context.Context, reader *bufio.Reader, wantID string, timeout time.Duration) (jsonrpcMessage, error) {
	type outcome struct {
		msg jsonrpcMessage
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				trimmed := bytes.TrimSpace(line)
				if len(trimmed) > 0 && trimmed[0] == '{' {
					var msg jsonrpcMessage
					if json.Unmarshal(trimmed, &msg) == nil && string(msg.ID) == wantID {
						ch <- outcome{msg: msg}
						return
					}
				}
			}
			if err != nil {
				ch <- outcome{err: err}
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			if errors.Is(res.err, io.EOF) {
				return jsonrpcMessage{}, errors.New("MCP server 提前退出，未返回响应")
			}
			return jsonrpcMessage{}, res.err
		}
		return res.msg, nil
	case <-timer.C:
		return jsonrpcMessage{}, errors.New("等待 MCP server 响应超时")
	case <-ctx.Done():
		return jsonrpcMessage{}, ctx.Err()
	}
}

func withStderrHint(err error, stderr *bytes.Buffer, label string) error {
	text := strings.TrimSpace(stderr.String())
	if text == "" {
		return err
	}
	if len(text) > 300 {
		// 按字节限额截断，但退到完整字符边界：这段 stderr 会直接拼进用户可见的错误信息。
		text = truncateUTF8(text, 300) + "…"
	}
	return fmt.Errorf("%w%s；stderr：%s", err, label, text)
}

func mcpInitializeMessage() []byte {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "milevia", "version": "1.0.0"},
		},
	})
	return payload
}

func mcpInitializedNotification() []byte {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	})
	return payload
}

func mcpToolsListMessage() []byte {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	return payload
}

func mcpResourcesListMessage() []byte {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "resources/list",
		"params":  map[string]any{},
	})
	return payload
}

func mcpPromptsListMessage() []byte {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "prompts/list",
		"params":  map[string]any{},
	})
	return payload
}

// applyMCPOptionalLists 把 resources/prompts 的可选响应并入结果。
// 缺失的响应、带 error 的响应与格式不符的响应都会被静默忽略——这两个原语是可选能力，
// 它们的缺失不应该被呈现为「测试失败」。
func applyMCPOptionalLists(out *mcpProbeOutcome, messages map[string]jsonrpcMessage) {
	if out == nil || len(messages) == 0 {
		return
	}
	out.Resources = parseMCPResourceList(messages["3"])
	out.Prompts = parseMCPPromptList(messages["4"])
	if out.Resources == nil {
		out.Resources = []mcpResourceInfo{}
	}
	if out.Prompts == nil {
		out.Prompts = []mcpPromptInfo{}
	}
}

func parseMCPResourceList(msg jsonrpcMessage) []mcpResourceInfo {
	if msg.Error != nil || len(msg.Result) == 0 {
		return nil
	}
	var listed struct {
		Resources []struct {
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Title       string `json:"title"`
			Description string `json:"description"`
			MimeType    string `json:"mimeType"`
		} `json:"resources"`
	}
	if json.Unmarshal(msg.Result, &listed) != nil {
		return nil
	}
	out := []mcpResourceInfo{}
	for index, item := range listed.Resources {
		if index >= mcpMaxProbeResources {
			break
		}
		out = append(out, mcpResourceInfo{
			URI:         item.URI,
			Name:        item.Name,
			Title:       item.Title,
			Description: item.Description,
			MimeType:    item.MimeType,
		})
	}
	return out
}

func parseMCPPromptList(msg jsonrpcMessage) []mcpPromptInfo {
	if msg.Error != nil || len(msg.Result) == 0 {
		return nil
	}
	var listed struct {
		Prompts []struct {
			Name        string          `json:"name"`
			Title       string          `json:"title"`
			Description string          `json:"description"`
			Arguments   json.RawMessage `json:"arguments"`
		} `json:"prompts"`
	}
	if json.Unmarshal(msg.Result, &listed) != nil {
		return nil
	}
	out := []mcpPromptInfo{}
	for index, item := range listed.Prompts {
		if index >= mcpMaxProbeResources {
			break
		}
		out = append(out, mcpPromptInfo{
			Name:        item.Name,
			Title:       item.Title,
			Description: item.Description,
			Arguments:   compactJSON(item.Arguments),
		})
	}
	return out
}

// buildProbeOutcome 从 initialize / tools-list 两条响应组装结果，并标注可疑模式。
func buildProbeOutcome(initMsg, toolsMsg jsonrpcMessage) (mcpProbeOutcome, error) {
	out := mcpProbeOutcome{Tools: []mcpToolInfo{}, Resources: []mcpResourceInfo{}, Prompts: []mcpPromptInfo{}}
	if len(initMsg.Result) > 0 {
		var init struct {
			ProtocolVersion string          `json:"protocolVersion"`
			ServerInfo      json.RawMessage `json:"serverInfo"`
			Capabilities    json.RawMessage `json:"capabilities"`
		}
		if json.Unmarshal(initMsg.Result, &init) == nil {
			out.ProtocolVersion = init.ProtocolVersion
			out.ServerInfo = init.ServerInfo
			out.Capabilities = init.Capabilities
		}
	}
	if len(toolsMsg.Result) > 0 {
		var listed struct {
			Tools []struct {
				Name        string          `json:"name"`
				Title       string          `json:"title"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
				Meta        json.RawMessage `json:"_meta"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(toolsMsg.Result, &listed); err != nil {
			return mcpProbeOutcome{}, fmt.Errorf("解析 tools/list 失败：%w", err)
		}
		for index, tool := range listed.Tools {
			if index >= mcpMaxProbeTools {
				break
			}
			info := mcpToolInfo{
				Name:                tool.Name,
				Title:               tool.Title,
				Description:         tool.Description,
				InputSchema:         compactJSON(tool.InputSchema),
				RequiresInteraction: mcpToolRequiresInteraction(tool.Meta),
			}
			info.Flags = flagMCPToolMetadata(info)
			out.Tools = append(out.Tools, info)
		}
	}
	return out, nil
}

// mcpToolRequiresInteraction 报告工具是否声明了「需要用户交互」。
//
// Claude Code 用 `_meta["anthropic/requiresUserInteraction"]`（需 ≥ v2.1.199）标记这类
// 工具：它们即使命中自动放行白名单，在 `-p` 模式下仍会回落要求用户交互并被拒——Milevia
// 的 hook-allow 无法覆盖（docs/34 §8.5）。因此这不是「可疑模式」，而是「确定性不可用」，
// 必须在工具列表里给出可操作的原因。
func mcpToolRequiresInteraction(meta json.RawMessage) bool {
	if len(meta) == 0 {
		return false
	}
	var decoded map[string]any
	if err := json.Unmarshal(meta, &decoded); err != nil {
		return false
	}
	// 键名按官方命名走，同时兼容下划线写法（不同 server 实现不一致）。
	for _, key := range []string{"anthropic/requiresUserInteraction", "anthropic/requires_user_interaction"} {
		switch value := decoded[key].(type) {
		case bool:
			if value {
				return true
			}
		case string:
			if strings.EqualFold(strings.TrimSpace(value), "true") {
				return true
			}
		}
	}
	return false
}

// probeMCPHTTP 以 Streamable HTTP 形式直连远端 server。
func probeMCPHTTP(ctx context.Context, req mcpProbeRequest, client *http.Client) (mcpProbeOutcome, error) {
	if client == nil {
		client = &http.Client{Timeout: mcpDefaultProbeTimeout}
	}
	sessionID := ""
	send := func(payload []byte) (jsonrpcMessage, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(payload))
		if err != nil {
			return jsonrpcMessage{}, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json, text/event-stream")
		for key, value := range req.Headers {
			httpReq.Header.Set(key, value)
		}
		if sessionID != "" {
			httpReq.Header.Set("mcp-session-id", sessionID)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			return jsonrpcMessage{}, err
		}
		defer resp.Body.Close()
		if id := resp.Header.Get("mcp-session-id"); id != "" {
			sessionID = id
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, mcpMaxProbeBytes))
		if err != nil {
			return jsonrpcMessage{}, err
		}
		if resp.StatusCode >= 400 {
			return jsonrpcMessage{}, fmt.Errorf("MCP server 返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(truncateProbeText(string(body))))
		}
		return parseHTTPJSONRPC(body)
	}

	initMsg, err := send(mcpInitializeMessage())
	if err != nil {
		return mcpProbeOutcome{}, fmt.Errorf("initialize 失败：%w", err)
	}
	if initMsg.Error != nil {
		return mcpProbeOutcome{}, fmt.Errorf("MCP server 返回错误：%s", initMsg.Error.Message)
	}
	if _, err := send(mcpInitializedNotification()); err != nil {
		// 通知类消息对端可能不返回 body，忽略其解析失败。
		_ = err
	}
	toolsMsg, err := send(mcpToolsListMessage())
	if err != nil {
		return mcpProbeOutcome{}, fmt.Errorf("tools/list 失败：%w", err)
	}
	if toolsMsg.Error != nil {
		return mcpProbeOutcome{}, fmt.Errorf("MCP server 返回错误：%s", toolsMsg.Error.Message)
	}
	outcome, err := buildProbeOutcome(initMsg, toolsMsg)
	if err != nil {
		return mcpProbeOutcome{}, err
	}
	// 可选原语：失败（含 -32601 method not found）不影响已拿到的工具列表。
	optional := map[string]jsonrpcMessage{}
	if msg, err := send(mcpResourcesListMessage()); err == nil {
		optional["3"] = msg
	}
	if msg, err := send(mcpPromptsListMessage()); err == nil {
		optional["4"] = msg
	}
	applyMCPOptionalLists(&outcome, optional)
	return outcome, nil
}

// parseHTTPJSONRPC 解析可能是 JSON、也可能是 SSE 的响应体，取其中最后一条 JSON-RPC。
func parseHTTPJSONRPC(body []byte) (jsonrpcMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return jsonrpcMessage{}, nil
	}
	if trimmed[0] == '{' {
		var msg jsonrpcMessage
		if err := json.Unmarshal(trimmed, &msg); err != nil {
			return jsonrpcMessage{}, fmt.Errorf("解析响应失败：%w", err)
		}
		return msg, nil
	}
	// SSE：逐行取 data: 后的 JSON，保留最后一条带 result/error 的。
	var latest jsonrpcMessage
	found := false
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(line)
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		var msg jsonrpcMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if len(msg.Result) > 0 || msg.Error != nil {
			latest = msg
			found = true
		}
	}
	if !found {
		return jsonrpcMessage{}, errors.New("未从事件流中解析到 JSON-RPC 响应（该 server 可能只支持旧式 SSE 端点）")
	}
	return latest, nil
}

// mergedProbeEnv 把 server 自定义 env 叠加到当前进程环境之上。
func mergedProbeEnv(env map[string]string) []string {
	base := os.Environ()
	for _, pair := range envPairs(env) {
		base = append(base, pair)
	}
	return base
}

func envPairs(env map[string]string) []string {
	out := []string{}
	for _, key := range sortedKeys(env) {
		out = append(out, key+"="+env[key])
	}
	return out
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return json.RawMessage(buf.Bytes())
}

// truncateProbeText 把探测得到的文本截到 400 个字符以内（按 rune，避免切断多字节字符）。
func truncateProbeText(text string) string {
	runes := []rune(text)
	if len(runes) > 400 {
		return string(runes[:400]) + "…"
	}
	return text
}

// ---------------------------------------------------------------------------
// 可疑工具元数据检测
// ---------------------------------------------------------------------------

type mcpFlagRule struct {
	Code     string
	Label    string
	Severity string
	Pattern  *regexp.Regexp
}

// mcpFlagRules 是工具名/描述/标题里命中的可疑模式。目的不是「判定恶意」（无法做到），
// 而是把「这句话在教模型做事，而不只是在描述工具」的地方挑出来提醒用户审阅。
var mcpFlagRules = []mcpFlagRule{
	{"instruction_override", "试图覆盖既有指令", "danger", regexp.MustCompile(`(?i)ignore\s+(all\s+)?(previous|prior|above|earlier)\s+(instruction|prompt|rule|message)`)},
	{"instruction_override", "试图覆盖既有指令", "danger", regexp.MustCompile(`(?i)disregard\s+(all\s+)?(previous|prior|above)`)},
	{"instruction_override", "试图覆盖既有指令", "danger", regexp.MustCompile(`忽{0,1}略(之前|前面|上面|先前)(的)?(所有)?(指令|规则|提示|要求)`)},
	{"instruction_override", "试图覆盖既有指令", "danger", regexp.MustCompile(`不用(理会|遵守)(之前|前面|上面)(的)?(指令|规则)`)},
	{"concealment", "要求对用户隐瞒", "danger", regexp.MustCompile(`(?i)(do\s+not|don'?t|never)\s+(tell|inform|mention|disclose|reveal)\s+(the\s+)?(user|human)`)},
	{"concealment", "要求对用户隐瞒", "danger", regexp.MustCompile(`(?i)without\s+(telling|informing|notifying)\s+(the\s+)?(user|human)`)},
	{"concealment", "要求对用户隐瞒", "danger", regexp.MustCompile(`(?i)keep\s+(this|it)\s+(a\s+)?secret`)},
	{"concealment", "要求对用户隐瞒", "danger", regexp.MustCompile(`(不要|无需|不必)(告诉|告知|通知|告知用户|让用户知道)`)},
	{"forced_invocation", "要求无条件调用", "warn", regexp.MustCompile(`(?i)(always|must|you\s+should)\s+(call|use|invoke|run)\s+this\s+tool`)},
	{"forced_invocation", "要求无条件调用", "warn", regexp.MustCompile(`(?i)before\s+(responding|answering|replying).{0,40}(call|use|invoke)`)},
	{"forced_invocation", "要求无条件调用", "warn", regexp.MustCompile(`(必须|务必|一定要)(先)?(调用|使用)`)},
	{"exfiltration", "疑似数据外发", "danger", regexp.MustCompile(`(?i)\b(send|upload|post|forward|transmit)\b[^\n]{0,60}?\b(contents?|data|files?|\.env|environment\s*variables?|credentials?|tokens?|keys?|secrets?)\b[^\n]{0,60}?\bto\b`)},
	{"exfiltration", "疑似数据外发", "danger", regexp.MustCompile(`(?i)\b(send|upload|post|forward|transmit)\b[^\n]{0,60}?https?://`)},
	{"exfiltration", "疑似数据外发", "danger", regexp.MustCompile(`(?i)exfiltrat`)},
	{"exfiltration", "疑似数据外发", "danger", regexp.MustCompile(`(上传|发送|转发|外发)[^\n]{0,20}(到|至)(服务器|远端|网址|http|外部)`)},
	{"credential_access", "涉及凭据读取", "warn", regexp.MustCompile(`(?i)(read|collect|gather|extract|dump)\s+(the\s+)?(env|environment\s+variables|credentials?|api\s*keys?|tokens?|passwords?)`)},
	{"credential_access", "涉及凭据读取", "warn", regexp.MustCompile(`(读取|收集|导出|提取)(环境变量|凭据|密钥|令牌|密码)`)},
	{"hidden_text", "含不可见/控制字符", "warn", regexp.MustCompile("[\u200b-\u200f\u202a-\u202e\u2060-\u2064\ufeff]")},
	{"encoded_blob", "含长 base64 片段", "warn", regexp.MustCompile(`[A-Za-z0-9+/]{160,}={0,2}`)},
	// 过度授权：工具描述里要求 admin / 全量权限的，未必恶意，但值得在授权前提醒
	// 用户改用最小权限凭据（docs/34 §9）。
	{"broad_scope", "疑似要求过宽权限", "warn", regexp.MustCompile(`\*:\*`)},
	{"broad_scope", "疑似要求过宽权限", "warn", regexp.MustCompile(`(?i)\badmin(istrator)?\s+(access|privileges?|permissions?|scopes?|rights?)\b`)},
	{"broad_scope", "疑似要求过宽权限", "warn", regexp.MustCompile(`(?i)\bfull\s+(admin|access|control|privileges?)\b`)},
	{"broad_scope", "疑似要求过宽权限", "warn", regexp.MustCompile(`(?i)\bunrestricted\b|\ball[\s_-]?permissions?\b`)},
	{"broad_scope", "疑似要求过宽权限", "warn", regexp.MustCompile(`(?i)\b(admin|manage|write|delete):[\w*]+`)},
	{"broad_scope", "疑似要求过宽权限", "warn", regexp.MustCompile(`(管理员权限|超级用户|完全控制|全部权限|所有权限|不受限制)`)},
}

// flagMCPToolMetadata 检查工具名/标题/描述与能力标记，返回需用户知晓的标注（去重）。
func flagMCPToolMetadata(tool mcpToolInfo) []mcpToolFlag {
	haystack := tool.Name + "\n" + tool.Title + "\n" + tool.Description
	flags := []mcpToolFlag{}
	seen := map[string]bool{}
	for _, rule := range mcpFlagRules {
		match := rule.Pattern.FindString(haystack)
		if match == "" {
			continue
		}
		key := rule.Code + "|" + rule.Label
		if seen[key] {
			continue
		}
		seen[key] = true
		flags = append(flags, mcpToolFlag{
			Code:     rule.Code,
			Label:    rule.Label,
			Severity: rule.Severity,
			Detail:   truncateProbeText(strings.TrimSpace(match)),
		})
	}
	if nameFlag := flagMCPToolName(tool.Name); nameFlag != nil {
		flags = append(flags, *nameFlag)
	}
	// 「需交互确认」不是可疑模式，而是确定性不可用：单列一条并附带可操作说明。
	if tool.RequiresInteraction {
		flags = append(flags, mcpToolFlag{
			Code:     "requires_interaction",
			Label:    "需交互确认（-p 模式下不可用）",
			Severity: "warn",
			Note:     "该工具声明需要用户交互，Claude 在 -p 模式下会直接拒绝，加入免审批白名单也无法覆盖。",
		})
	}
	if len(flags) == 0 {
		return nil
	}
	return flags
}

var mcpToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// flagMCPToolName 检查工具名本身是否异常（非 ASCII 或含不可见字符）。
func flagMCPToolName(name string) *mcpToolFlag {
	if name == "" {
		return nil
	}
	if !mcpToolNamePattern.MatchString(name) {
		return &mcpToolFlag{
			Code:     "unusual_name",
			Label:    "工具名含非常规字符",
			Severity: "warn",
			Detail:   truncateProbeText(name),
		}
	}
	return nil
}

// countFlaggedTools 统计带可疑标注的工具数（任一项 severity=danger 亦计入）。
func countFlaggedTools(tools []mcpToolInfo) int {
	count := 0
	for _, tool := range tools {
		if len(tool.Flags) > 0 {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// HTTP handler：POST /api/mcp/servers/{id}/test
// ---------------------------------------------------------------------------

type mcpTestInput struct {
	Environment  string `json:"environment"`
	ProjectID    string `json:"projectId"`
	ConnectionID string `json:"connectionId"`
	TimeoutSec   int    `json:"timeoutSec"`
}

type mcpTestResponse struct {
	OK              bool              `json:"ok"`
	ServerID        string            `json:"serverId"`
	Name            string            `json:"name"`
	DisplayName     string            `json:"displayName"`
	Environment     string            `json:"environment"`
	Transport       string            `json:"transport"`
	ProtocolVersion string            `json:"protocolVersion,omitempty"`
	ServerInfo      json.RawMessage   `json:"serverInfo,omitempty"`
	Tools           []mcpToolInfo     `json:"tools"`
	Resources       []mcpResourceInfo `json:"resources"`
	Prompts         []mcpPromptInfo   `json:"prompts"`
	ToolCount       int               `json:"toolCount"`
	FlaggedCount    int               `json:"flaggedCount"`
	DurationMs      int64             `json:"durationMs"`
	Error           string            `json:"error,omitempty"`
	Hint            string            `json:"hint,omitempty"`
}

// testMCPServer 在目标环境真实拉起 server 并列出工具。
//
// 这是「可观测与可验证」的核心：授权之前先看清 server 声称自己有哪些工具、描述里是否
// 夹带了不该有的指令。测试失败不写库、不改状态，只回传原因。
func (s *Server) testMCPServer(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	var input mcpTestInput
	if !decodeOptional(w, r, &input) {
		return
	}
	ctx := r.Context()
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	projectID := strings.TrimSpace(input.ProjectID)
	if projectID == "" {
		projectID = stored.ProjectID
	}
	projectPath := ""
	runnerID := ""
	if projectID != "" {
		project, err := s.getProjectByID(ctx, projectID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest, errors.New("项目不存在"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		projectPath = project.Path
		runnerID = project.RunnerID
	}
	target := agentTargetEnv(strings.TrimSpace(input.Environment))
	if target != agentTargetEnvWindows && target != agentTargetEnvWSL && target != agentTargetEnvRemote {
		target = s.resolveAgentTargetEnv(runnerID, projectPath)
	}

	timeout := time.Duration(input.TimeoutSec) * time.Second
	if timeout <= 0 || timeout > 2*time.Minute {
		timeout = mcpDefaultProbeTimeout
	}

	probe := mcpProbeRequest{
		Transport:   stored.Transport,
		Command:     resolveMCPPlaceholders(stored.Command, target, projectPath),
		Cwd:         resolveMCPPlaceholders(stored.Cwd, target, projectPath),
		URL:         resolveMCPPlaceholders(stored.URL, target, projectPath),
		Environment: target,
		ProjectPath: projectPath,
		// 前端测试对话框不提供连接选择，故从项目的 runnerID 反推；否则「SSH 远端」分支
		// 永远因 ConnID 为空而失败。
		ConnID:  mcpConnectionIDFromRunner(runnerID, strings.TrimSpace(input.ConnectionID)),
		Timeout: timeout,
	}
	for _, arg := range stored.Args {
		probe.Args = append(probe.Args, resolveMCPPlaceholders(arg, target, projectPath))
	}
	// 测试需要真值（明文）才能连上：这里用内联模式解密，明文只在本次进程内存与子进程
	// 环境中存在，不落盘、不写日志、不回传前端。
	env, _, ok := s.resolveMCPValues(ctx, stored.envRaw, false, target, projectPath)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("MCP 凭据无法解密，请重新填写密钥"))
		return
	}
	probe.Env = env
	headers, _, ok := s.resolveMCPValues(ctx, stored.headersRaw, false, target, projectPath)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("MCP 凭据无法解密，请重新填写密钥"))
		return
	}
	probe.Headers = headers
	// OAuth：已授权且未显式配置 Authorization 时，用访问令牌测试。
	if !hasMCPHeaderKey(stored.headersRaw, "Authorization") {
		if header, ok := s.mcpOAuthAuthorizationHeader(ctx, stored.ID); ok {
			if probe.Headers == nil {
				probe.Headers = map[string]string{}
			}
			probe.Headers["Authorization"] = header
		}
	}
	if stored.Cwd != "" {
		probe.ProjectPath = probe.Cwd
	}

	started := time.Now()
	response := mcpTestResponse{
		ServerID:    stored.ID,
		Name:        stored.Name,
		DisplayName: stored.DisplayName,
		Environment: string(target),
		Transport:   stored.Transport,
		Tools:       []mcpToolInfo{},
		Resources:   []mcpResourceInfo{},
		Prompts:     []mcpPromptInfo{},
	}
	outcome, probeErr := s.probeMCPServer(ctx, probe)
	response.DurationMs = time.Since(started).Milliseconds()
	if probeErr != nil {
		response.Error = probeErr.Error()
		response.Hint = mcpTestHint(probeErr, target)
		writeJSON(w, http.StatusOK, response)
		return
	}
	fillMCPTestResponse(&response, outcome)
	writeJSON(w, http.StatusOK, response)
}

// fillMCPTestResponse 把一次成功的探测结果填进响应。
//
// 两个测试入口（已落库的 / 草稿态）共用，避免「同一个 server 两处测出不同口径」。
// 列表字段一律补成空切片：编成 null 时前端按 T[] 读会整块渲染失败。
func fillMCPTestResponse(response *mcpTestResponse, outcome mcpProbeOutcome) {
	response.OK = true
	response.ProtocolVersion = outcome.ProtocolVersion
	response.ServerInfo = outcome.ServerInfo
	response.Tools = outcome.Tools
	if response.Tools == nil {
		response.Tools = []mcpToolInfo{}
	}
	response.Resources = outcome.Resources
	if response.Resources == nil {
		response.Resources = []mcpResourceInfo{}
	}
	response.Prompts = outcome.Prompts
	if response.Prompts == nil {
		response.Prompts = []mcpPromptInfo{}
	}
	response.ToolCount = len(outcome.Tools)
	response.FlaggedCount = countFlaggedTools(outcome.Tools)
	if response.ToolCount == 0 {
		if len(response.Resources) > 0 || len(response.Prompts) > 0 {
			response.Hint = "该 server 没有声明工具，但提供了 resources / prompts。Claude Code 可以使用它们；Codex 目前只支持 tools。"
		} else {
			response.Hint = "server 已连通，但没有声明任何工具、resources 或 prompts。"
		}
	}
}

// mcpDraftTestInput 是「草稿态试连」的表单快照。
//
// 与创建接口同一信任模型：明文凭据只出现在本地回环请求与本次进程内存里，不落库、不回显。
type mcpDraftTestInput struct {
	Transport    string            `json:"transport"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	Environment  string            `json:"environment"`
	ProjectID    string            `json:"projectId"`
	ConnectionID string            `json:"connectionId"`
	TimeoutSec   int               `json:"timeoutSec"`
}

// testMCPDraft 在保存之前试连一条尚未落库的配置。
//
// 「一键连接」向导的最后一步靠它：`/servers/{id}/test` 依赖已落库的 serverID，于是一个
// 「先试试能不能连」的朴素需求就必然变成「先保存 → 再测 → 失败回改」—— 配错了还要先污染
// 一次配置库。这里直接把表单字段拼成探测请求，复用同一条探针通道。
func (s *Server) testMCPDraft(w http.ResponseWriter, r *http.Request) {
	var input mcpDraftTestInput
	if !decode(w, r, &input) {
		return
	}
	transport := strings.TrimSpace(input.Transport)
	if transport != mcpTransportStdio && transport != mcpTransportHTTP && transport != mcpTransportSSE {
		writeError(w, http.StatusBadRequest, fmt.Errorf("不支持的传输类型：%s", transport))
		return
	}

	projectID := strings.TrimSpace(input.ProjectID)
	projectPath := ""
	runnerID := ""
	if projectID != "" {
		project, err := s.getProjectByID(r.Context(), projectID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest, errors.New("项目不存在"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		projectPath = project.Path
		runnerID = project.RunnerID
	}

	environment := strings.TrimSpace(input.Environment)
	// 兼容前端可能传入的旧字面量。
	if environment == "remote" {
		environment = string(agentTargetEnvRemote)
	}
	target := agentTargetEnv(environment)
	if target != agentTargetEnvWindows && target != agentTargetEnvWSL && target != agentTargetEnvRemote {
		target = s.resolveAgentTargetEnv(runnerID, projectPath)
	}

	timeout := time.Duration(input.TimeoutSec) * time.Second
	if timeout <= 0 || timeout > 2*time.Minute {
		timeout = mcpDefaultProbeTimeout
	}

	probe := mcpProbeRequest{
		Transport:   transport,
		Command:     resolveMCPPlaceholders(strings.TrimSpace(input.Command), target, projectPath),
		URL:         resolveMCPPlaceholders(strings.TrimSpace(input.URL), target, projectPath),
		Environment: target,
		ProjectPath: projectPath,
		ConnID:      mcpConnectionIDFromRunner(runnerID, strings.TrimSpace(input.ConnectionID)),
		Timeout:     timeout,
	}
	for _, arg := range input.Args {
		probe.Args = append(probe.Args, resolveMCPPlaceholders(arg, target, projectPath))
	}
	// 草稿里的值就是表单刚填的明文（没有 sec_ 引用可解），但**仍要按目标环境解析占位符** ——
	// 否则「占位符预览对了、试连却拿原样文本」这条不一致会再出现一次。
	probe.Env = resolveMCPDraftValues(input.Env, target, projectPath)
	probe.Headers = resolveMCPDraftValues(input.Headers, target, projectPath)

	started := time.Now()
	response := mcpTestResponse{
		Environment: string(target),
		Transport:   transport,
		Tools:       []mcpToolInfo{},
		Resources:   []mcpResourceInfo{},
		Prompts:     []mcpPromptInfo{},
	}
	outcome, probeErr := s.probeMCPServer(r.Context(), probe)
	response.DurationMs = time.Since(started).Milliseconds()
	if probeErr != nil {
		response.Error = probeErr.Error()
		response.Hint = mcpTestHint(probeErr, target)
		writeJSON(w, http.StatusOK, response)
		return
	}
	fillMCPTestResponse(&response, outcome)
	writeJSON(w, http.StatusOK, response)
}

// resolveMCPDraftValues 对草稿里的明文键值统一解析 ${PROJECT_DIR}，口径与预览 / 注入一致。
func resolveMCPDraftValues(values map[string]string, target agentTargetEnv, projectPath string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = resolveMCPPlaceholders(value, target, projectPath)
	}
	return out
}

// mcpTestHint 把常见失败翻译成可操作的建议，避免用户拿着「超时」无从下手。
func mcpTestHint(err error, target agentTargetEnv) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "超时"):
		return "启动或响应超时：常见原因是该命令在目标环境未安装（stdio 需要 npx/uvx/node/python），或远端网络无法拉取依赖。可先在该环境手动执行一次启动命令确认。"
	case strings.Contains(message, "未安装") || strings.Contains(message, "not found") || strings.Contains(message, "command not found"):
		return "目标环境缺少该命令。stdio 型 server 依赖本机的运行时，请在目标环境安装后重试，或改用 http 型 server。"
	case strings.Contains(message, "未建立") || strings.Contains(message, "SSH"):
		return "远端测试需要该 SSH 连接处于已连接状态。请先在「SSH连接」页面建立连接。"
	case strings.Contains(message, "提前退出"):
		return "MCP server 进程启动后立即退出。可查看其 stderr 提示（已附在错误信息中），通常是参数错误或缺少凭据。"
	case target == agentTargetEnvWindows:
		return ""
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// HTTP handler：POST /api/mcp/runtime-check
// ---------------------------------------------------------------------------

// mcpRuntimeCheckTimeout 是一次运行时检查的总超时（整批命令共用）。
const mcpRuntimeCheckTimeout = 20 * time.Second

// mcpRuntimeMissingMarker 是「命令不存在」的哨兵输出。
//
// 检查命令统一写成 `command -v X || echo <marker>`，让整体退出码恒为 0：否则「命令确实
// 没装」会与「通道本身坏了」（WSL 不可用、SSH 未连接）一样表现为退出码非零，前端就无法
// 区分「去装一下」和「先把连接建起来」。
const mcpRuntimeMissingMarker = "__milevia_mcp_missing__"

// mcpRuntimeCommandPattern 限制被检查的命令名。
//
// 这个名字会被拼进目标环境的 shell（WSL 与 SSH 分支都是 `sh -c`），所以必须白名单化：
// 只允许字母数字开头、由字母数字与 . _ + - 组成。模板目录与表单里的正常取值
// （npx / uvx / docker / node / python3）都在此范围内。
var mcpRuntimeCommandPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

type mcpRuntimeCheckInput struct {
	Commands     []string `json:"commands"`
	Environment  string   `json:"environment"`
	ConnectionID string   `json:"connectionId"`
}

type mcpRuntimeCheckItem struct {
	Command string `json:"command"`
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	// Error 只用于「这一条没法检查」（如命令名非法），不含「未找到」。
	Error string `json:"error,omitempty"`
}

type mcpRuntimeCheckResponse struct {
	Environment string                `json:"environment"`
	Items       []mcpRuntimeCheckItem `json:"items"`
	// Error 是通道级失败原因（SSH 未连接、WSL 不可用、控制服务不在 Windows）。
	// 非空时 Items 里的「未找到」不成立，前端应整体提示而不是逐条报缺失。
	Error string `json:"error,omitempty"`
}

// mcpRuntimeProbeScript 生成一条 POSIX shell 形式的「这个命令在不在」检查脚本。
func mcpRuntimeProbeScript(command string) string {
	return "command -v " + command + " || echo " + mcpRuntimeMissingMarker
}

// mcpRuntimeItemFromOutput 把 `command -v` 的输出转成一条结论。
func mcpRuntimeItemFromOutput(command, output string) mcpRuntimeCheckItem {
	path := strings.TrimSpace(output)
	if path == "" || strings.Contains(path, mcpRuntimeMissingMarker) {
		return mcpRuntimeCheckItem{Command: command, Found: false}
	}
	// 只取首行：某些 shell 会在路径之后追加告警。
	if idx := strings.IndexByte(path, '\n'); idx >= 0 {
		path = strings.TrimSpace(path[:idx])
	}
	return mcpRuntimeCheckItem{Command: command, Found: true, Path: path}
}

// checkMCPRuntimes 在目标环境检查一批命令是否存在。
//
// 与「连接测试」的分工：测试会真的拉起 server，且必须已落库；本接口不落库、不启动 server，
// 只回答「这条模板在当前环境跑得起来吗」，因此可以在保存之前调用 —— 这正是把
// 「保存 → 测试 → 失败 → 回改」的返工提前掉的地方（docs/34 §10.3）。
func (s *Server) checkMCPRuntimes(w http.ResponseWriter, r *http.Request) {
	var input mcpRuntimeCheckInput
	if !decode(w, r, &input) {
		return
	}
	environment := strings.TrimSpace(input.Environment)
	// 兼容前端可能传入的旧字面量。
	if environment == "remote" {
		environment = string(agentTargetEnvRemote)
	}
	if environment == "" {
		environment = string(agentTargetEnvWindows)
	}
	if !mcpValidEnvironments[environment] {
		writeError(w, http.StatusBadRequest, errors.New("未知的目标环境"))
		return
	}
	target := agentTargetEnv(environment)

	// 合法命令名去重；非法项不拼进 shell，直接作为一条独立结论回传。
	commands := make([]string, 0, len(input.Commands))
	rejected := make([]mcpRuntimeCheckItem, 0)
	seen := map[string]bool{}
	for _, raw := range input.Commands {
		command := strings.TrimSpace(raw)
		if command == "" {
			continue
		}
		if !mcpRuntimeCommandPattern.MatchString(command) {
			rejected = append(rejected, mcpRuntimeCheckItem{Command: command, Found: false, Error: "命令名含不支持的字符，已跳过检查"})
			continue
		}
		if seen[command] {
			continue
		}
		seen[command] = true
		commands = append(commands, command)
	}

	ctx, cancel := context.WithTimeout(r.Context(), mcpRuntimeCheckTimeout)
	defer cancel()

	respond := func(channelErr string, found []mcpRuntimeCheckItem) {
		writeJSON(w, http.StatusOK, mcpRuntimeCheckResponse{
			Environment: environment,
			Items:       append(append([]mcpRuntimeCheckItem{}, rejected...), found...),
			Error:       channelErr,
		})
	}

	switch target {
	case agentTargetEnvWindows:
		if runtime.GOOS != "windows" {
			respond("控制服务不在 Windows 上，无法在 Windows 环境检查运行时", nil)
			return
		}
		// LookPath 在 Windows 上按 PATHEXT 展开，能找到 npx.cmd 这类形态。
		found := make([]mcpRuntimeCheckItem, 0, len(commands))
		for _, command := range commands {
			path, err := exec.LookPath(command)
			found = append(found, mcpRuntimeCheckItem{Command: command, Found: err == nil, Path: path})
		}
		respond("", found)

	case agentTargetEnvWSL:
		if runtime.GOOS != "windows" {
			respond("控制服务不在 Windows 上，无法在 WSL 环境检查运行时", nil)
			return
		}
		wsl := s.wslAgentRunner()
		runner, ok := wsl.(*wslAgentRunner)
		if !ok || runner == nil {
			respond("未探测到可用的 WSL 发行版，无法在 WSL 环境检查运行时", nil)
			return
		}
		found := make([]mcpRuntimeCheckItem, 0, len(commands))
		for _, command := range commands {
			output, err := runner.wslBridgeProbe(ctx, mcpRuntimeProbeScript(command))
			if err != nil {
				respond("WSL 探测失败："+err.Error(), nil)
				return
			}
			found = append(found, mcpRuntimeItemFromOutput(command, output))
		}
		respond("", found)

	case agentTargetEnvRemote:
		connID := mcpConnectionIDFromRunner("", strings.TrimSpace(input.ConnectionID))
		if connID == "" {
			respond("远端检查需要指定 SSH 连接，请先在「SSH连接」页面建立连接后在项目视图里选择该项目", nil)
			return
		}
		client, err := s.sshClientForConnection(connID)
		if err != nil {
			respond(err.Error(), nil)
			return
		}
		found := make([]mcpRuntimeCheckItem, 0, len(commands))
		for _, command := range commands {
			output, err := client.execCommand(ctx, mcpRuntimeProbeScript(command))
			if err != nil {
				respond("远端执行失败："+err.Error(), nil)
				return
			}
			found = append(found, mcpRuntimeItemFromOutput(command, string(output)))
		}
		respond("", found)

	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("不支持的目标环境：%s", environment))
	}
}

// mcpConnectionIDFromRunner 解析出目标 SSH 连接 id。
//
// 显式传入优先；否则从 runnerID 反推 —— SSH runner 的 id 恒为 `ssh-<connectionID>`
// （ssh_connection.go 各处注册时就是这个形态）。测试与运行时检查都从项目拿 runnerID，
// 而前端这两个对话框都不提供连接选择，没有这一步「SSH 远端」分支永远拿不到连接。
//
// **非 ssh- 前缀一律返回空**：项目的 runner 是本机或 WSL 时，把 runnerID 当成连接 id
// 传下去只会得到「该 SSH 连接当前未建立」这种指向错误原因的提示。
func mcpConnectionIDFromRunner(runnerID, explicit string) string {
	if explicit != "" {
		return explicit
	}
	runnerID = strings.TrimSpace(runnerID)
	if !strings.HasPrefix(runnerID, "ssh-") {
		return ""
	}
	return strings.TrimPrefix(runnerID, "ssh-")
}
