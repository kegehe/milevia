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

func TestParseConflictSuggestionJSONRequiresMergedString(t *testing.T) {
	for _, raw := range []string{`{}`, `{"explanation":"无法解决"}`, `{"merged":null}`, `{"merged":42}`} {
		t.Run(raw, func(t *testing.T) {
			if merged, _, err := parseConflictSuggestionJSON(raw); err == nil {
				t.Fatalf("invalid result was accepted as merged=%q", merged)
			}
		})
	}
	// 合法结果可以是空文件；不能用 merged == "" 代替字段校验。
	if merged, _, err := parseConflictSuggestionJSON(`{"merged":""}`); err != nil || merged != "" {
		t.Fatalf("explicit empty file was rejected: merged=%q err=%v", merged, err)
	}
}

func TestConflictSuggestionBudget(t *testing.T) {
	detail := GitConflictContent{Base: strings.Repeat("b", 1024), Ours: strings.Repeat("o", 1024), Theirs: strings.Repeat("t", 1024), Working: strings.Repeat("w", 1024)}
	if got := conflictSuggestionContentBudget(detail); got != 4096 {
		t.Fatalf("budget=%d want 4096", got)
	}
}
