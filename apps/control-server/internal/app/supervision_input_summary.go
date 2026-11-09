package app

import (
	"encoding/json"
	"unicode/utf8"
)

func supervisionTextPrefix(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// 摘要只用于模型观察；原文和完整证据始终保留于数据库及过程接口。
func supervisionModelJSON(raw json.RawMessage) json.RawMessage {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	var compact func(any) any
	compact = func(v any) any {
		switch item := v.(type) {
		case string:
			if len(item) > 4096 {
				return supervisionTextPrefix(item, 4096) + "\n[资料摘录已截断，完整内容保留于原始执行记录，不能当作完整证据]"
			}
		case []any:
			for i := range item {
				item[i] = compact(item[i])
			}
		case map[string]any:
			for key, child := range item {
				item[key] = compact(child)
			}
		}
		return v
	}
	return mustJSON(compact(value))
}

func supervisionTaskSummary(raw json.RawMessage) json.RawMessage {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	if description, ok := value["description"].(string); ok && len(description) > 512 {
		value["description"] = supervisionTextPrefix(description, 512)
		value["descriptionTruncated"] = true
		value["readFullTask"] = "使用read_context并携带taskId读取任务完整原文，禁止根据截断内容擅自减少需求"
	}
	return mustJSON(value)
}
