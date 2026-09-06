package app

import (
	"strings"
	"testing"
)

func TestIsThinkingTokensEvent(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		body string
		want bool
	}{
		{"system subtype", "system", `{"type":"system","subtype":"thinking_tokens"}`, true},
		{"system type", "system", `{"type":"thinking_tokens"}`, true},
		{"named event", "system.thinking_tokens", `{}`, true},
		{"init", "system", `{"type":"system","subtype":"init"}`, false},
		{"assistant text", "assistant", `{"subtype":"thinking_tokens"}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isThinkingTokensEvent(test.typ, []byte(test.body)); got != test.want {
				t.Fatalf("isThinkingTokensEvent() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAppendEventSkipsThinkingTokensPersistence(t *testing.T) {
	server := newTestServer(t)
	server.appendEvent("run", "conversation", "system", []byte(`{"type":"system","subtype":"thinking_tokens","tokens":1}`))
	server.appendEvent("run", "conversation", "system.thinking_tokens", []byte(`{"tokens":1}`))
	var count int
	if err := server.db.QueryRow(`select count(*) from events where conversation_id=?`, "conversation").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("thinking_tokens event persisted: %d", count)
	}
}

func TestThinkingTokensPredicateCoversLegacyEventTypes(t *testing.T) {
	predicate := thinkingTokensPredicate("e")
	for _, fragment := range []string{"e.type", "e.payload", "thinking_tokens", "thinking.tokens"} {
		if !strings.Contains(predicate, fragment) {
			t.Fatalf("predicate %q does not contain %q", predicate, fragment)
		}
	}
}
