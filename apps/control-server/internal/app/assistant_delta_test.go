package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// partialSink implements AgentRunSink plus the two optional incremental
// interfaces, so the stream parser can be exercised without a database.
type partialSink struct {
	messageID string
	deltas    []string
	finals    []string
}

func (s *partialSink) Event(string, json.RawMessage)          {}
func (s *partialSink) SessionIdentified(string)               {}
func (s *partialSink) SessionInitialized()                    {}
func (s *partialSink) AssistantText(content, _ string)        { s.finals = append(s.finals, content) }
func (s *partialSink) SetAssistantMessageID(messageID string) { s.messageID = messageID }
func (s *partialSink) AssistantDelta(delta string)            { s.deltas = append(s.deltas, delta) }

// plainSink implements only AgentRunSink, standing in for the sinks that never
// opted into incremental output (orchestration review, SSH turns, tests).
type plainSink struct {
	events []string
}

func (s *plainSink) Event(eventType string, _ json.RawMessage) {
	s.events = append(s.events, eventType)
}
func (s *plainSink) SessionIdentified(string)     {}
func (s *plainSink) SessionInitialized()          {}
func (s *plainSink) AssistantText(string, string) {}

// The CLI emits one streaming envelope per token, and only the visible text
// belongs in a transcript: thinking deltas are excluded for the same reason
// thinking_tokens never reach it, and block boundaries carry no content.
func TestClaudePartialMessageForwardsOnlyVisibleText(t *testing.T) {
	sink := &partialSink{}
	handleClaudePartialMessage(json.RawMessage(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"0e486950-7d90-4ed3-b9b0-504b45c29e1d"}}}`), sink)
	if sink.messageID != "0e486950-7d90-4ed3-b9b0-504b45c29e1d" {
		t.Fatalf("assistant message id was not adopted: %q", sink.messageID)
	}
	handleClaudePartialMessage(json.RawMessage(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}}`), sink)
	handleClaudePartialMessage(json.RawMessage(`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}}`), sink)
	handleClaudePartialMessage(json.RawMessage(`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`), sink)

	if len(sink.deltas) != 1 || sink.deltas[0] != "hi" {
		t.Fatalf("deltas=%v want exactly [hi]", sink.deltas)
	}
}

// A sink that does not implement the optional interfaces must be left alone
// rather than panicking or swallowing the message.
func TestClaudePartialMessageToleratesSinksWithoutDeltaSupport(t *testing.T) {
	sink := &plainSink{}
	handleClaudePartialMessage(json.RawMessage(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"x"}}}`), sink)
	handleClaudePartialMessage(json.RawMessage(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`), sink)
	if len(sink.events) != 0 {
		t.Fatalf("plain sink received events: %v", sink.events)
	}
}

// Malformed envelopes must be ignored rather than aborting the run.
func TestClaudePartialMessageIgnoresMalformedInput(t *testing.T) {
	sink := &partialSink{}
	handleClaudePartialMessage(json.RawMessage(`{`), sink)
	handleClaudePartialMessage(json.RawMessage(`{"type":"stream_event"}`), sink)
	handleClaudePartialMessage(json.RawMessage(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta"}}}`), sink)
	if len(sink.deltas) != 0 {
		t.Fatalf("deltas=%v want none", sink.deltas)
	}
}

func newAssistantDeltaTestSink(t *testing.T) (*Server, *agentRunSink) {
	t.Helper()
	server := newTestServer(t)
	// Deltas are relay-only, so the remote relay has to be configured and its
	// outbox migrated before they can be observed at all.
	server.config.RemoteCloudURL = "https://cloud.example.com"
	server.config.RemoteCloudToken = "token"
	if err := server.migrateRemoteControl(context.Background()); err != nil {
		t.Fatalf("migrate remote control: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project','/tmp/project',?, 'main',1,?)`, server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return server, &agentRunSink{server: server, runID: "run", conversationID: "conversation"}
}

// A reply of 200 tokens must not become 200 relay events: every one of them
// would otherwise reach the desktop outbox and the cloud's event table.
func TestAssistantDeltaIsCoalesced(t *testing.T) {
	server, sink := newAssistantDeltaTestSink(t)
	sink.SetAssistantMessageID("cli-message-1")
	for i := 0; i < 200; i++ {
		sink.AssistantDelta("x")
	}

	var relayed int
	if err := server.db.QueryRow(`select count(*) from remote_outbox where type='assistant.delta'`).Scan(&relayed); err != nil {
		t.Fatal(err)
	}
	if relayed == 0 {
		t.Fatal("no assistant.delta was relayed")
	}
	if relayed > 20 {
		t.Fatalf("200 tokens produced %d relay rows; coalescing is not working", relayed)
	}

	// Deltas must stay out of the conversation's stored event history: it is
	// reloaded whole whenever a client opens the conversation, and no local view
	// renders a chunk.
	var persisted int
	if err := server.db.QueryRow(`select count(*) from events where type='assistant.delta'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 0 {
		t.Fatalf("deltas were persisted as conversation events: %d rows", persisted)
	}
}

// The client matches deltas to the finished message through the CLI's own id.
// That id rides on the event rather than becoming the row key, so a replayed
// message can never collide with an existing row.
func TestAssistantMessageCarriesAgentMessageID(t *testing.T) {
	server, sink := newAssistantDeltaTestSink(t)
	sink.SetAssistantMessageID("cli-message-2")
	sink.AssistantDelta("partial")
	sink.AssistantText("the full reply", "")

	var payload string
	if err := server.db.QueryRow(`select payload from events where type='assistant.message'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["agentMessageId"] != "cli-message-2" {
		t.Fatalf("agentMessageId=%v want cli-message-2", decoded["agentMessageId"])
	}
	if decoded["content"] != "the full reply" {
		t.Fatalf("content=%v", decoded["content"])
	}

	// Without an incremental id the field is omitted, so a runtime that never
	// emits partial messages produces exactly the payload it did before.
	sink.AssistantText("second reply", "")
	// Order by rowid rather than created_at: two messages written in the same
	// microsecond tie on the timestamp and the ordering becomes arbitrary.
	if err := server.db.QueryRow(`select payload from events where type='assistant.message' order by rowid desc limit 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	// Decode into a nil map: unmarshalling onto an existing map merges into it
	// and would keep the key from the previous payload, hiding the omission.
	decoded = nil
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal("second payload unparsable")
	}
	if _, present := decoded["agentMessageId"]; present {
		t.Fatalf("agentMessageId should be omitted when no CLI id was seen: %s", payload)
	}
}

// A finished message must clear the pending CLI id and drop the unflushed tail,
// otherwise a later message with no message_start of its own would adopt the
// previous id and the client would overwrite the wrong reply.
func TestAssistantTextClearsPendingMessageID(t *testing.T) {
	server, sink := newAssistantDeltaTestSink(t)
	sink.SetAssistantMessageID("cli-message-3")
	sink.AssistantDelta("buffered")
	sink.AssistantText("first", "")

	if sink.assistantMessageID != "" {
		t.Fatalf("pending id survived the finished message: %q", sink.assistantMessageID)
	}
	// The first delta flushes immediately (the interval gate has no previous
	// emission to compare against); anything buffered after it must be dropped
	// rather than relayed after the authoritative message has already landed.
	var relayed int
	if err := server.db.QueryRow(`select count(*) from remote_outbox where type='assistant.delta'`).Scan(&relayed); err != nil {
		t.Fatal(err)
	}
	if relayed != 1 {
		t.Fatalf("relayed=%d want 1 (the tail must not be flushed after the message)", relayed)
	}
}
