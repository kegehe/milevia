package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Codex 原生 JSONL 协议；不能伪造 Claude init，也不将工具结果当作助手答案。
type supervisionCodexStream struct {
	sink                            AgentRunSink
	cancel                          context.CancelFunc
	mu                              sync.Mutex
	err                             error
	buffer                          []byte
	total                           int
	initialized, started, completed bool
	output                          string
	messageID                       string
}

func newSupervisionCodexStream(sink AgentRunSink, cancel context.CancelFunc) *supervisionCodexStream {
	return &supervisionCodexStream{sink: sink, cancel: cancel}
}

func (s *supervisionCodexStream) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *supervisionCodexStream) fail(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = errors.New(message)
		s.cancel()
	}
}

func (s *supervisionCodexStream) Write(data []byte) (int, error) {
	s.total += len(data)
	if s.total > 4<<20 || len(s.buffer)+len(data) > 1<<20 {
		s.fail("Codex 托管输出超过协议上限")
		return 0, s.failure()
	}
	s.buffer = append(s.buffer, data...)
	for {
		end := bytes.IndexByte(s.buffer, '\n')
		if end < 0 {
			break
		}
		s.line(bytes.TrimSpace(s.buffer[:end]))
		s.buffer = s.buffer[end+1:]
		if err := s.failure(); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (s *supervisionCodexStream) flush() {
	if s.failure() == nil && len(bytes.TrimSpace(s.buffer)) > 0 {
		s.line(bytes.TrimSpace(s.buffer))
	}
	s.buffer = nil
}

func (s *supervisionCodexStream) line(raw []byte) {
	if len(raw) == 0 || s.failure() != nil {
		return
	}
	clean, err := sanitizeAgentJSONL(raw)
	if err != nil {
		s.fail("Codex 托管输出不是有效 JSONL")
		return
	}
	var event struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
		Error    struct {
			Message string `json:"message"`
		} `json:"error"`
		Item struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Text    string `json:"text"`
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"item"`
	}
	if json.Unmarshal(clean, &event) != nil || s.completed {
		s.fail("Codex 托管事件格式或终结顺序无效")
		return
	}
	switch event.Type {
	case "thread.started":
		if s.initialized || event.ThreadID == "" {
			s.fail("Codex 托管线程身份重复或缺失")
			return
		}
		s.initialized = true
		s.sink.SessionIdentified(event.ThreadID)
		s.sink.SessionInitialized()
	case "turn.started":
		if !s.initialized || s.started {
			s.fail("Codex 托管回合重复或未初始化")
			return
		}
		s.started = true
	case "item.started", "item.updated", "item.completed":
		// 0.166.0 把未收录的自定义模型元数据提示编码为 item/error，而非
		// turn.failed。仅接受这个已实测的启动提示，不接受一般 error 或工具字段。
		if s.initialized && !s.started && event.Type == "item.completed" && event.Item.Type == "error" && supervisionCodexStartupNotice(event.Item.Message) {
			var envelope struct {
				Item map[string]json.RawMessage `json:"item"`
			}
			_ = json.Unmarshal(clean, &envelope)
			for key := range envelope.Item {
				if key != "id" && key != "type" && key != "message" {
					s.fail("Codex 启动提示包含执行字段")
					return
				}
			}
			s.sink.Event(event.Type, clean)
			return
		}
		if !s.started || (event.Item.Type != "reasoning" && event.Item.Type != "agent_message") {
			if event.Item.Type == "error" && event.Item.Message != "" {
				s.fail("Codex 托管启动或执行报告错误：" + codexDiagnostic(event.Item.Message))
				return
			}
			s.fail(fmt.Sprintf("Codex 托管执行意外返回工具或未知执行内容（%.64s/%.64s，回合已开始=%t）", event.Type, event.Item.Type, s.started))
			return
		}
		var envelope struct {
			Item map[string]json.RawMessage `json:"item"`
		}
		_ = json.Unmarshal(clean, &envelope)
		for key := range envelope.Item {
			if key != "id" && key != "type" && key != "text" && key != "phase" {
				s.fail("Codex 托管文本项包含不支持的执行字段")
				return
			}
		}
		if event.Item.Type == "agent_message" && event.Type == "item.completed" {
			if event.Item.Phase == "commentary" {
				s.sink.AssistantText(event.Item.Text, "")
				s.sink.Event(event.Type, clean)
				return
			}
			if event.Item.Phase != "" && event.Item.Phase != "final_answer" {
				s.fail("Codex 托管结果阶段与协议不符")
				return
			}
			if s.messageID != "" || event.Item.ID == "" || len(event.Item.Text) > 256<<10 {
				s.fail("Codex 托管候选结果重复或超过协议上限")
				return
			}
			s.messageID, s.output = event.Item.ID, event.Item.Text
			s.sink.AssistantText(event.Item.Text, "")
		}
	case "turn.completed":
		if !s.started || s.messageID == "" {
			s.fail("Codex 托管回合缺少候选结果")
			return
		}
		s.completed = true
	case "error", "turn.failed":
		// Native error frames are failures, never candidate results. Retain a
		// bounded, redacted reason so authentication/provider failures can be
		// distinguished from an unsupported protocol and transient retries can
		// use the actual cause. Do not publish the unvalidated error envelope.
		message := event.Message
		if event.Type == "turn.failed" {
			message = event.Error.Message
		}
		if strings.TrimSpace(message) == "" {
			s.fail("Codex 托管执行失败，CLI 未提供错误原因")
		} else {
			s.fail("Codex 托管执行失败：" + codexDiagnostic(message))
		}
		return
	default:
		s.fail("Codex 托管执行返回失败或不支持的事件")
		return
	}
	s.sink.Event(event.Type, clean)
}

func supervisionCodexStartupNotice(message string) bool {
	// 较新模型元数据偏好 Code Mode，但托管角色显式关闭它，不安装或开启
	// code-mode host。此通知确认 fail-closed，不代表模型回合失败。
	if message == "Code Mode is unavailable because code-mode host is disabled. Code mode will fail closed; enable `features.code_mode_host` and install `codex-code-mode-host`." {
		return true
	}
	const prefix = "Model metadata for `"
	const suffix = "` not found. Defaulting to fallback metadata; this can degrade performance and cause issues."
	return len(message) <= 1024 && strings.HasPrefix(message, prefix) && strings.HasSuffix(message, suffix)
}
