package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// 任务导出/导入（docs/50-任务导出与导入方案.md）。
//
// 跨电脑迁移只搬「任务内容 + 队列顺序 + 依赖关系」：执行记录、验证记录、编排状态都绑定
// 本机的会话 / 分支 / 工作区，搬过去没有意义。导入一律走一个事务，避免失败时留下半套任务。
const (
	taskTransferFormat   = "milevia.tasks"
	taskTransferVersion  = 1
	taskTransferMaxTasks = 2000
	// 导入的请求体上限单独放宽。全局 maxJSONBodyBytes 是 16MB，而单条说明上限 12000 字
	// （中文约 36KB），400 条满描述就会超过它——不放开会出现「导出的文件导不回来」。
	taskTransferMaxBytes   = 64 << 20
	taskTransferTitleLimit = 120
	taskExportScopeActive  = "active"
	taskExportScopeAll     = "all"
)

type taskTransferSource struct {
	Project string `json:"project"`
}

// taskTransferFile 是导出文件的顶层结构。version 只在格式不兼容时递增。
type taskTransferFile struct {
	Format     string             `json:"format"`
	Version    int                `json:"version"`
	ExportedAt time.Time          `json:"exportedAt"`
	Source     taskTransferSource `json:"source"`
	Tasks      []taskTransferItem `json:"tasks"`
	// DroppedDependencies 是导出时因前置任务不在导出范围内被裁掉的依赖条数，仅供提示。
	DroppedDependencies int `json:"droppedDependencies"`
}

// taskTransferItem 用文件内 key（t1…tN）而不是本机 uuid：文件自包含、可手工编辑，
// 导入到另一台电脑时不需要任何旧 id。
type taskTransferItem struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	Pinned      bool   `json:"pinned"`
	// Status 只用于判断依赖是否已满足（done 的前置在源项目里不阻塞）。导入后所有任务
	// 都是 todo，它不会让已完成任务复活。
	Status    string     `json:"status"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	DependsOn []string   `json:"dependsOn,omitempty"`
}

type taskTransferInvalid struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type taskTransferCreated struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

type taskTransferImportResult struct {
	Imported   int                   `json:"imported"`
	Duplicates int                   `json:"duplicates"`
	Invalid    []taskTransferInvalid `json:"invalid"`
	// SatisfiedDependencies 是「前置在源项目里已完成」而没有重建的依赖；
	// DroppedDependencies 是前置不存在（未导出 / 无效条目）被丢弃的依赖。
	SatisfiedDependencies int                   `json:"satisfiedDependencies"`
	DroppedDependencies   int                   `json:"droppedDependencies"`
	Dependencies          int                   `json:"dependencies"`
	Created               []taskTransferCreated `json:"created"`
}

func (s *Server) exportTasks(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = taskExportScopeActive
	}
	if scope != taskExportScopeActive && scope != taskExportScopeAll {
		writeError(w, http.StatusBadRequest, errors.New("unsupported task export scope"))
		return
	}

	query := `select id,project_id,title,description,priority,pinned,position,status,created_at,updated_at,completed_at,cancelled_at from tasks where project_id=?`
	args := []any{projectID}
	if scope == taskExportScopeActive {
		// 与前端 filterQueueTasks 的 active 定义保持一致：done / cancelled 都算「已结束」。
		query += ` and status not in (?,?)`
		args = append(args, taskDone, taskCancelled)
	}
	// 按 position 排，**不能带 pinned desc**：导入端会按数组顺序重新编号 position，
	// 而看板列内排序只看 position（compareTaskOrder）。带上置顶优先会让置顶任务在文件里
	// 提前，导入后看板上的手工顺序与源项目不一致；pinned 是独立字段，队列视图自己会排。
	query += ` order by position,created_at`
	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	tasks := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var projectName string
	if err := s.db.QueryRowContext(r.Context(), `select coalesce(name,'') from projects where id=?`, projectID).Scan(&projectName); err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	file := taskTransferFile{
		Format:     taskTransferFormat,
		Version:    taskTransferVersion,
		ExportedAt: time.Now().UTC(),
		Source:     taskTransferSource{Project: projectName},
		Tasks:      make([]taskTransferItem, 0, len(tasks)),
	}
	keyByTaskID := make(map[string]string, len(tasks))
	for index := range tasks {
		key := fmt.Sprintf("t%d", index+1)
		keyByTaskID[tasks[index].ID] = key
		createdAt := tasks[index].CreatedAt
		file.Tasks = append(file.Tasks, taskTransferItem{
			Key:         key,
			Title:       tasks[index].Title,
			Description: tasks[index].Description,
			Priority:    tasks[index].Priority,
			Pinned:      tasks[index].Pinned,
			Status:      exportTaskTransferStatus(tasks[index].Status),
			CreatedAt:   &createdAt,
			DependsOn:   []string{},
		})
	}
	dependencies, dropped, err := s.taskTransferDependencies(r.Context(), projectID, keyByTaskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for index := range file.Tasks {
		if list, ok := dependencies[file.Tasks[index].Key]; ok {
			file.Tasks[index].DependsOn = list
		}
	}
	file.DroppedDependencies = dropped
	writeJSON(w, http.StatusOK, file)
}

// taskTransferDependencies 取项目内全部依赖边，翻译成文件内 key。
// 排序必须带 rowid：同一任务的依赖共用 created_at，只按时间排会让文件里的依赖顺序
// 每次导出都不一样（与 hydrateTaskDependencies 的排序契约一致）。
func (s *Server) taskTransferDependencies(ctx context.Context, projectID string, keyByTaskID map[string]string) (map[string][]string, int, error) {
	rows, err := s.db.QueryContext(ctx, `select dependency.task_id,dependency.predecessor_task_id
		from task_dependencies dependency
		join tasks dependent on dependent.id=dependency.task_id
		where dependent.project_id=?
		order by dependency.created_at,dependency.rowid`, projectID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := map[string][]string{}
	dropped := 0
	for rows.Next() {
		var taskID, predecessorID string
		if err := rows.Scan(&taskID, &predecessorID); err != nil {
			return nil, 0, err
		}
		key, ok := keyByTaskID[taskID]
		if !ok {
			continue
		}
		predecessorKey, ok := keyByTaskID[predecessorID]
		if !ok {
			// 前置被 scope 排除了（例如 active 导出时的已完成 / 已取消任务）。
			dropped++
			continue
		}
		result[key] = append(result[key], predecessorKey)
	}
	return result, dropped, rows.Err()
}

func exportTaskTransferStatus(status string) string {
	if status == taskDone || status == taskCancelled {
		return status
	}
	// running / awaiting_review / action_required 都代表「还没做完」，导出成 todo；
	// 导入端一律写 todo，这里只是为了文件可读。
	return taskTodo
}

func (s *Server) importTasks(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	dedupe := strings.TrimSpace(r.URL.Query().Get("dedupe")) != "false"

	var file taskTransferFile
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskTransferMaxBytes))
	if err := decoder.Decode(&file); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, errors.New("task export file is too large"))
			return
		}
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		return
	}
	if err := normalizeTaskTransferFile(&file); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	invalidKeys := map[string]bool{}
	doneKeys := map[string]bool{}
	for _, item := range file.Tasks {
		description := strings.TrimSpace(item.Description)
		if description == "" || len([]rune(description)) > 12000 {
			invalidKeys[item.Key] = true
		}
		if item.Status == taskDone {
			doneKeys[item.Key] = true
		}
	}
	if taskTransferGraphHasCycle(file.Tasks, invalidKeys, doneKeys) {
		writeError(w, http.StatusBadRequest, errors.New("task export dependencies form a cycle"))
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	var maxPosition float64
	if err := tx.QueryRowContext(ctx, `select coalesce(max(position),0) from tasks where project_id=?`, projectID).Scan(&maxPosition); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 去重索引：说明（trim 后）→ 已存在任务 id。title 不能进键——它是 AI 精炼产物，
	// 同一任务在两台电脑上可能标题不同；而任务编辑器没有 title 输入，说明才是一致的那一列。
	existingIDs := map[string]string{}
	if dedupe {
		rows, err := tx.QueryContext(ctx, `select id,description from tasks where project_id=? order by position,created_at`, projectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for rows.Next() {
			var id, description string
			if err := rows.Scan(&id, &description); err != nil {
				rows.Close()
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			key := strings.TrimSpace(description)
			if _, exists := existingIDs[key]; exists {
				continue
			}
			existingIDs[key] = id
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := rows.Close(); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	now := time.Now().UTC()
	idByKey := make(map[string]string, len(file.Tasks))
	sourceDone := make(map[string]bool, len(file.Tasks))
	// 去重命中的条目在目标项目里已经有对应任务：不能再往那条任务上追加依赖，
	// 否则一次重复导入会悄悄改变已有任务的阻塞关系（源文件里的依赖也可能早已过时）。
	duplicateKeys := make(map[string]bool, len(file.Tasks))
	invalid := []taskTransferInvalid{}
	created := []taskTransferCreated{}
	imported, duplicates := 0, 0
	for _, item := range file.Tasks {
		description := strings.TrimSpace(item.Description)
		if description == "" {
			invalid = append(invalid, taskTransferInvalid{Key: item.Key, Reason: "任务说明为空"})
			continue
		}
		if len([]rune(description)) > 12000 {
			invalid = append(invalid, taskTransferInvalid{Key: item.Key, Reason: "任务说明超过 12000 字"})
			continue
		}
		if dedupe {
			if existingID, ok := existingIDs[description]; ok {
				// 命中重复时也要记住映射：后面指向它的依赖必须指向这条已存在的任务，
				// 否则「重复导入」会静默丢掉依赖关系。
				idByKey[item.Key] = existingID
				sourceDone[item.Key] = item.Status == taskDone
				duplicateKeys[item.Key] = true
				duplicates++
				continue
			}
		}
		title := normalizeTaskTransferTitle(item.Title)
		if title == "" {
			title = generatedTaskTitle(description)
		}
		priority := strings.TrimSpace(item.Priority)
		if !validTaskPriority(priority) {
			priority = "normal"
		}
		maxPosition++
		id := uuid.NewString()
		createdAt := now
		if item.CreatedAt != nil && !item.CreatedAt.IsZero() && item.CreatedAt.Before(now) {
			createdAt = item.CreatedAt.UTC()
		}
		if _, err := tx.ExecContext(ctx, `insert into tasks (id,project_id,title,description,priority,pinned,position,status,created_at,updated_at) values (?,?,?,?,?,?,?,?,?,?)`,
			id, projectID, title, description, priority, item.Pinned, maxPosition, taskTodo, createdAt, now); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := s.recordTaskEventTx(ctx, tx, id, "", "task.created", map[string]string{"status": taskTodo, "source": "import"}, now); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		idByKey[item.Key] = id
		sourceDone[item.Key] = item.Status == taskDone
		if dedupe {
			existingIDs[description] = id
		}
		imported++
		created = append(created, taskTransferCreated{Key: item.Key, ID: id})
	}

	dependencies, satisfied, dropped := 0, 0, 0
	for _, item := range file.Tasks {
		if duplicateKeys[item.Key] {
			// 重复任务的依赖整体跳过：不计数、不落库。目标项目里那条任务的依赖
			// 由它自己维护，导入只负责「补上缺的任务」。
			continue
		}
		taskID, ok := idByKey[item.Key]
		if !ok {
			// 无效条目没有建成任务，它自己的依赖也就不存在。
			continue
		}
		for _, predecessorKey := range item.DependsOn {
			predecessorID, ok := idByKey[predecessorKey]
			if !ok {
				dropped++
				continue
			}
			if sourceDone[predecessorKey] {
				// 源项目里这条前置已经完成（不阻塞），导入后它变成了待办；如果照建依赖，
				// 后继会永远「等待前置完成」且无法下发。所以直接视为已满足。
				satisfied++
				continue
			}
			result, err := tx.ExecContext(ctx, `insert or ignore into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, taskID, predecessorID, now)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			// 同一个新任务可能通过两个重复命中的 key 指到同一条已有前置，第二次 insert
			// 会被忽略；按影响行数计数，别把没落库的依赖算进结果。
			if affected, err := result.RowsAffected(); err == nil && affected > 0 {
				dependencies++
			}
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, taskTransferImportResult{
		Imported:              imported,
		Duplicates:            duplicates,
		Invalid:               invalid,
		SatisfiedDependencies: satisfied,
		DroppedDependencies:   dropped,
		Dependencies:          dependencies,
		Created:               created,
	})
}

// normalizeTaskTransferFile 做结构校验并规范化 key / 依赖列表。任何硬错误都返回
// 固定短句——localizedErrorText 的翻译表是精确匹配，带参数的句子会露出英文原文。
func normalizeTaskTransferFile(file *taskTransferFile) error {
	if file.Format != taskTransferFormat {
		return errors.New("unsupported task export file")
	}
	if file.Version != taskTransferVersion {
		return errors.New("task export file version is not supported")
	}
	if len(file.Tasks) == 0 {
		return errors.New("task export contains no tasks")
	}
	if len(file.Tasks) > taskTransferMaxTasks {
		return errors.New("task export contains too many tasks")
	}
	keys := make(map[string]bool, len(file.Tasks))
	for _, item := range file.Tasks {
		key := strings.TrimSpace(item.Key)
		if !validTaskTransferKey(key) || keys[key] {
			return errors.New("duplicate task key in export file")
		}
		keys[key] = true
	}
	for index := range file.Tasks {
		item := &file.Tasks[index]
		item.Key = strings.TrimSpace(item.Key)
		// 手工编辑的文件可能写成 "HIGH" / " Done "：归一后再进入状态与优先级判断，
		// 否则前置会被误判成未完成（阻塞后继），优先级也会静默回退成 normal。
		item.Status = strings.ToLower(strings.TrimSpace(item.Status))
		item.Priority = strings.ToLower(strings.TrimSpace(item.Priority))
		seen := map[string]bool{}
		normalized := make([]string, 0, len(item.DependsOn))
		for _, raw := range item.DependsOn {
			predecessor := strings.TrimSpace(raw)
			if predecessor == "" || predecessor == item.Key || !keys[predecessor] {
				return errors.New("task export references an unknown dependency")
			}
			if seen[predecessor] {
				continue
			}
			seen[predecessor] = true
			normalized = append(normalized, predecessor)
		}
		item.DependsOn = normalized
	}
	return nil
}

func validTaskTransferKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// normalizeTaskTransferTitle 把标题折成单行并截断。标题只用于展示，说明才是内容来源。
func normalizeTaskTransferTitle(title string) string {
	title = strings.Join(strings.Fields(strings.TrimSpace(title)), " ")
	runes := []rune(title)
	if len(runes) <= taskTransferTitleLimit {
		return title
	}
	return string(runes[:taskTransferTitleLimit]) + "..."
}

// taskTransferGraphHasCycle 在内存里做 Kahn 拓扑排序。任务此刻还没入库，
// wouldCreateTaskCycleTx 用不了；只统计**文件内可以判定的边**：前置已完成（done）
// 或条目无效时依赖不会建，不能把它们算进环里。是否去重要到事务里查库才知道，
// 所以这里按文件全图保守判定——导出的原文件本身不可能有环。
func taskTransferGraphHasCycle(items []taskTransferItem, invalid, done map[string]bool) bool {
	indegree := map[string]int{}
	edges := map[string][]string{}
	for _, item := range items {
		if invalid[item.Key] {
			continue
		}
		if _, ok := indegree[item.Key]; !ok {
			indegree[item.Key] = 0
		}
	}
	for _, item := range items {
		if invalid[item.Key] {
			continue
		}
		for _, predecessor := range item.DependsOn {
			if invalid[predecessor] || done[predecessor] {
				continue
			}
			edges[predecessor] = append(edges[predecessor], item.Key)
			indegree[item.Key]++
		}
	}
	queue := make([]string, 0, len(indegree))
	for key, degree := range indegree {
		if degree == 0 {
			queue = append(queue, key)
		}
	}
	visited := 0
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range edges[key] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	return visited != len(indegree)
}
