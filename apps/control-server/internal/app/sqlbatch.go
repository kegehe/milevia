package app

import "strings"

// sqlIDBatchSize 限制一次 IN (...) 里绑定的 id 个数：单条 SQLite 语句的绑定参数有上限
// （老构建是 999），分批比拼一个超长列表稳。
const sqlIDBatchSize = 400

// forEachIDBatch 把 ids 按 sqlIDBatchSize 分批，为每批拼好 in (...) 的占位符再调用 fn。
// 列表接口的派生字段都是"拿一批 id 补一批数据"：任务列表的任务依赖/最近一次执行/编排状态、
// 定时任务列表的最近一次执行、编排批次列表的作业汇总都走这一条分批规则。
func forEachIDBatch(ids []string, fn func(placeholders string, args []any) error) error {
	for start := 0; start < len(ids); start += sqlIDBatchSize {
		end := min(start+sqlIDBatchSize, len(ids))
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		if err := fn(placeholders, args); err != nil {
			return err
		}
	}
	return nil
}
