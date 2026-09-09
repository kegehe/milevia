package app

import (
	"strings"
	"testing"
)

func TestParseConflictSuggestionJSON(t *testing.T) {
	merged, explanation, err := parseConflictSuggestionJSON(`{"merged":"line1\nhand\n","explanation":"取当前侧"}`)
	if err != nil || merged != "line1\nhand\n" || explanation != "取当前侧" {
		t.Fatalf("unexpected parse result: merged=%q explanation=%q err=%v", merged, explanation, err)
	}
}

func TestParseConflictSuggestionJSONAcceptsFencedText(t *testing.T) {
	merged, _, err := parseConflictSuggestionJSON("```json\n{\"merged\":\"ok\",\"explanation\":\"x\"}\n```")
	if err != nil || merged != "ok" {
		t.Fatalf("fenced JSON not parsed: merged=%q err=%v", merged, err)
	}
}

func TestParseConflictSuggestionJSONRejectsLeftoverMarkers(t *testing.T) {
	if _, _, err := parseConflictSuggestionJSON(`{"merged":"<<<<<<< HEAD\nbroken","explanation":""}`); err == nil {
		t.Fatal("leftover conflict markers were accepted")
	}
}

func TestParseConflictSuggestionJSONRejectsNonJSON(t *testing.T) {
	if _, _, err := parseConflictSuggestionJSON("抱歉，我无法解决"); err == nil {
		t.Fatal("plain text was accepted as suggestion JSON")
	}
}

func TestConflictSuggestionBudget(t *testing.T) {
	detail := GitConflictContent{Base: strings.Repeat("b", 1024), Ours: strings.Repeat("o", 1024), Theirs: strings.Repeat("t", 1024), Working: strings.Repeat("w", 1024)}
	if got := conflictSuggestionContentBudget(detail); got != 4096 {
		t.Fatalf("budget=%d want 4096", got)
	}
}
