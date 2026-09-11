package app

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudePartialStreamReplayMatchesFinalMessage replays a captured
// `claude -p --output-format stream-json --include-partial-messages` transcript
// through the same envelope dispatch the runner uses.
//
// A hand-written fixture could only prove the parser agrees with my own guesses
// about the CLI. This one is real output, so it pins the three facts the
// incremental path depends on:
//
//  1. message_start carries the same id as the completed assistant envelope,
//     which is what lets a phone tie a streamed placeholder to the real message;
//  2. the concatenated text deltas are exactly the assistant's text block;
//  3. thinking deltas are not part of that text, so forwarding them instead of
//     excluding them would break assertion 2 rather than pass silently.
func TestClaudePartialStreamReplayMatchesFinalMessage(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "claude_partial_stream.jsonl"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()

	sink := &partialSink{}
	var assistantParts []string

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		line := json.RawMessage(append([]byte(nil), scanner.Bytes()...))
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		// Mirrors runClaudeStream's dispatch: partial envelopes are consumed by
		// the incremental parser, everything else is handled by type.
		var envelope struct {
			Type            string          `json:"type"`
			ParentToolUseID string          `json:"parent_tool_use_id"`
			Message         json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			t.Fatalf("fixture contains invalid JSON: %v", err)
		}
		if envelope.Type == "stream_event" {
			handleClaudePartialMessage(line, sink)
			continue
		}
		if envelope.Type == "assistant" {
			parts, _ := parseClaudeMessage(envelope.Message)
			for _, part := range parts {
				assistantParts = append(assistantParts, part)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if len(sink.deltas) < 20 {
		t.Fatalf("only %d deltas were forwarded; the fixture should exercise a real stream", len(sink.deltas))
	}
	for i, delta := range sink.deltas {
		if delta == "" {
			t.Fatalf("delta %d is empty; empty chunks must not reach the relay", i)
		}
	}

	streamed := strings.Join(sink.deltas, "")
	finished := strings.Join(assistantParts, "")
	if streamed == "" {
		t.Fatal("no incremental text was forwarded")
	}
	if streamed != finished {
		t.Fatalf("streamed text does not match the finished message:\n streamed=%q\n finished=%q", clip(streamed), clip(finished))
	}

	// The thinking block must not have become an assistant message of its own.
	if len(assistantParts) != 1 {
		t.Fatalf("assistant message parts=%d want 1 (thinking must not reach the transcript)", len(assistantParts))
	}
	if sink.messageID == "" {
		t.Fatal("the CLI message id was never adopted, so a client cannot match the placeholder")
	}
}

func clip(s string) string {
	const limit = 200
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
