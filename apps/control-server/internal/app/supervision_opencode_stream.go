package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

// OpenCode emits complete parts for a single, no-tool step. Strict identities
// and ordering prevent an incomplete prefix or a tool event becoming a result.
type supervisionOpenCodeStream struct {
	sink                         AgentRunSink
	cancel                       context.CancelFunc
	mu                           sync.Mutex
	err                          error
	buffer                       []byte
	total                        int
	initialized, completed       bool
	sessionID, messageID, output string
	textIDs                      map[string]bool
}

func newSupervisionOpenCodeStream(sink AgentRunSink, cancel context.CancelFunc) *supervisionOpenCodeStream {
	return &supervisionOpenCodeStream{sink: sink, cancel: cancel, textIDs: map[string]bool{}}
}

func (s *supervisionOpenCodeStream) failure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *supervisionOpenCodeStream) fail(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = errors.New(message)
		s.cancel()
	}
}

func (s *supervisionOpenCodeStream) Write(data []byte) (int, error) {
	s.total += len(data)
	if s.total > 4<<20 || len(s.buffer)+len(data) > 1<<20 {
		s.fail("OpenCode 托管输出超限")
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

func (s *supervisionOpenCodeStream) flush() {
	if s.failure() == nil && len(bytes.TrimSpace(s.buffer)) > 0 {
		s.line(bytes.TrimSpace(s.buffer))
	}
	s.buffer = nil
}

func (s *supervisionOpenCodeStream) line(raw []byte) {
	if len(raw) == 0 || s.failure() != nil {
		return
	}
	clean, err := sanitizeAgentJSONL(raw)
	if err != nil {
		s.fail("OpenCode 托管输出不是有效 JSONL")
		return
	}
	var event struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionID"`
		Part      struct {
			ID        string `json:"id"`
			MessageID string `json:"messageID"`
			SessionID string `json:"sessionID"`
			Type      string `json:"type"`
			Text      string `json:"text"`
			Reason    string `json:"reason"`
			Tokens    struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"part"`
	}
	if json.Unmarshal(clean, &event) != nil || s.completed || event.SessionID == "" || event.Part.SessionID != event.SessionID || event.Part.ID == "" || event.Part.MessageID == "" {
		s.fail("OpenCode 托管身份、事件格式或终结顺序无效")
		return
	}
	var envelope struct {
		Part map[string]json.RawMessage `json:"part"`
	}
	_ = json.Unmarshal(clean, &envelope)
	for key := range envelope.Part {
		switch key {
		case "id", "messageID", "sessionID", "type", "text", "time", "snapshot", "reason", "tokens", "cost":
		default:
			s.fail("OpenCode 托管文本项包含不支持的执行字段")
			return
		}
	}
	if !s.initialized {
		if event.Type != "step_start" || event.Part.Type != "step-start" {
			s.fail("OpenCode 托管回合未初始化")
			return
		}
		s.initialized, s.sessionID, s.messageID = true, event.SessionID, event.Part.MessageID
		s.sink.SessionIdentified(event.SessionID)
		s.sink.SessionInitialized()
	} else if event.SessionID != s.sessionID || event.Part.MessageID != s.messageID {
		s.fail("OpenCode 托管会话或消息身份变化")
		return
	} else {
		switch event.Type {
		case "text", "reasoning":
			if event.Part.Type != event.Type || s.textIDs[event.Part.ID] || len(s.output)+len(event.Part.Text) > 256<<10 {
				s.fail("OpenCode 托管文本重复、格式无效或超限")
				return
			}
			s.textIDs[event.Part.ID] = true
			if event.Type == "text" {
				s.output += event.Part.Text
				s.sink.AssistantText(event.Part.Text, "")
			}
		case "step_finish":
			if event.Part.Type != "step-finish" || event.Part.Reason != "stop" || strings.TrimSpace(s.output) == "" || event.Part.Tokens.Input < 0 || event.Part.Tokens.Output < 0 || event.Part.Tokens.Reasoning < 0 || event.Part.Tokens.Cache.Read < 0 || event.Part.Tokens.Cache.Write < 0 {
				s.fail("OpenCode 托管回合失败或缺少完整结果")
				return
			}
			s.completed = true
		default:
			s.fail("OpenCode 托管回合返回工具、失败或未知事件")
			return
		}
	}
	// Do not invent Claude init or Codex turns. Native usage has its own reader.
	s.sink.Event("opencode."+event.Type, clean)
}
