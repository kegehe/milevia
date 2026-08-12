package app

import "testing"

// 复现「分析代理未返回有效结果，请重试」(insights.go:557)。
// 该错误 = parseInsightCandidates 返回 error。以下喂入真实 agent 可能产出的形态，
// 逐一确认哪种会命中该错误，打印精确 error 文本。

func TestReproInsightPassAFailure(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "A.原始数组被截断(无闭合])",
			raw:  `[{"type":"bug","severity":"high","title":"A","summary":"s"},{"type":"feature","severity":"normal","title":"B","summary":"t"}`,
		},
		{
			name: "B.夹代码围栏+散文,数组被截断",
			raw:  "好的这是分析:\n```json\n[{\"type\":\"bug\",\"title\":\"x\",\"summary\":\"y\"}\n```\n希望对你有用",
		},
		{
			name: "C.纯散文(无JSON)",
			raw:  "我通读了项目,我认为列表页可以优化,详情请看对话。",
		},
		{
			name: "D.空输出",
			raw:  "",
		},
		{
			name: "E.对象包裹但键名非findings",
			raw:  `{"result":[{"type":"bug","title":"a","summary":"b"}]}`,
		},
		{
			name: "F.先短示例后截断大数组",
			raw:  "示例:[{\"type\":\"bug\",\"title\":\"示例\",\"summary\":\"例\"}]。最终:\n[{\"type\":\"optimization\",\"title\":\"真实\",\"summary\":\"x\"}",
		},
		{
			name: "G.围栏开启行带前导文字(剥围栏失败)",
			raw:  "结果 ```json\n[{\"type\":\"bug\",\"title\":\"a\",\"summary\":\"b\"}]\n```\n完",
		},
	}
	for _, c := range cases {
		items, err := parseInsightCandidates(c.raw)
		if err != nil {
			t.Logf("[%s] -> ERROR: %v\n   raw=%q", c.name, err, truncateInsightLog(c.raw, 300))
			continue
		}
		t.Logf("[%s] -> OK: %d items: %+v", c.name, len(items), items)
	}
}
