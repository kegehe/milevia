package app

import (
	"strings"
	"testing"
)

func TestGeneratedTaskTitleUsesFirstSentenceAndNormalizesWhitespace(t *testing.T) {
	got := generatedTaskTitle("  修复通知中心的空标题。\n\n同时补充回归测试。 ")
	if got != "修复通知中心的空标题" {
		t.Fatalf("generatedTaskTitle() = %q", got)
	}
}

func TestGeneratedTaskTitleTruncatesRunes(t *testing.T) {
	input := strings.Repeat("测", taskGeneratedTitleLimit+1)
	got := generatedTaskTitle(input)
	if got != strings.Repeat("测", taskGeneratedTitleLimit)+"..." {
		t.Fatalf("generatedTaskTitle() = %q", got)
	}
}

func TestTaskInputAlwaysDerivesTitleFromDescription(t *testing.T) {
	input := taskInput{Title: "客户端传入的标题", Description: "实现自动生成任务标题。后续内容不应成为标题。"}
	if err := validateTaskInput(&input); err != nil {
		t.Fatalf("validateTaskInput(): %v", err)
	}
	if input.Title != "实现自动生成任务标题" {
		t.Fatalf("Title = %q", input.Title)
	}
}

func TestTaskInputIgnoresAnOversizedClientTitle(t *testing.T) {
	input := taskInput{Title: strings.Repeat("x", 121), Description: "仅使用任务说明生成标题。"}
	if err := validateTaskInput(&input); err != nil {
		t.Fatalf("validateTaskInput(): %v", err)
	}
	if input.Title != "仅使用任务说明生成标题" {
		t.Fatalf("Title = %q", input.Title)
	}
}

func TestParseGeneratedTaskTitleAcceptsOnlyValidNonemptyTitle(t *testing.T) {
	if got := parseGeneratedTaskTitle("```json\n{\"title\":\"整理任务创建入口\"}\n```"); got != "整理任务创建入口" {
		t.Fatalf("valid output = %q", got)
	}
	for _, value := range []string{"", "{\"title\":\"   \"}", "{\"name\":\"错误字段\"}", "not json"} {
		if got := parseGeneratedTaskTitle(value); got != "" {
			t.Fatalf("parseGeneratedTaskTitle(%q) = %q", value, got)
		}
	}
}
