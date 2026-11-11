package app

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Both DB and Tx implement this; transaction callers must never query s.db
// while holding the single SQLite connection.
type insightQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type insightDedupState struct {
	aliases   map[string]string
	protected map[string]bool
	permanent map[string]bool // Preserve unknown suppression reasons.
	tasks     map[string]insightLinkedTask
}

func loadInsightDedupState(ctx context.Context, q insightQueryer, projectID string) (*insightDedupState, error) {
	d := &insightDedupState{map[string]string{}, map[string]bool{}, map[string]bool{}, map[string]insightLinkedTask{}}
	rows, err := q.QueryContext(ctx, `select fingerprint,reason,canonical_fingerprint from project_insight_suppressions where project_id=?`, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fp, reason, canonical string
		if err := rows.Scan(&fp, &reason, &canonical); err != nil {
			rows.Close()
			return nil, err
		}
		if reason != insightSuppressionSuperseded {
			d.permanent[fp] = true
		} else if canonical != "" && canonical != fp {
			d.aliases[fp] = canonical
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `select fingerprint from project_insights where project_id=? and (coalesce(verification_result,'')<>'invalid' or status=?)`, projectID, insightStatusDismissed)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			rows.Close()
			return nil, err
		}
		d.protected[fp] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Active tasks take precedence over newer terminal tasks, including aliases.
	rows, err = q.QueryContext(ctx, `select source_insight_fingerprint,status,title from tasks where project_id=? and coalesce(source_insight_fingerprint,'')<>''
		order by (status not in (?,?)) desc,created_at desc,id desc`, projectID, taskDone, taskCancelled)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fp string
		var task insightLinkedTask
		if err := rows.Scan(&fp, &task.Status, &task.Title); err != nil {
			rows.Close()
			return nil, err
		}
		if canonical := d.aliases[fp]; canonical != "" {
			fp = canonical
		}
		if _, exists := d.tasks[fp]; !exists {
			d.tasks[fp] = task
		}
	}
	err = rows.Err()
	rows.Close()
	return d, err
}

func (d *insightDedupState) rootActive(fp string) bool {
	task, ok := d.tasks[fp]
	return d.protected[fp] || (ok && !insightTaskTerminal(task.Status))
}

func (d *insightDedupState) root(fp string) string {
	if canonical := d.aliases[fp]; canonical != "" && d.rootActive(canonical) {
		return canonical
	}
	return fp
}

func (d *insightDedupState) linked(fp string) (insightLinkedTask, bool) {
	task, ok := d.tasks[d.root(fp)]
	return task, ok
}

func (d *insightDedupState) activeTask(fp string) bool {
	task, ok := d.linked(fp)
	return ok && !insightTaskTerminal(task.Status)
}

func (d *insightDedupState) suppressed(fp string) bool {
	return d.protected[fp] || d.permanent[fp] || d.root(fp) != fp || d.activeTask(fp)
}

func insightFindingUnchanged(ctx context.Context, q insightQueryer, projectID string, f InsightFinding) (bool, error) {
	var updated time.Time
	err := q.QueryRowContext(ctx, `select updated_at from project_insights where project_id=? and id=?`, projectID, f.ID).Scan(&updated)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil && updated.Equal(f.UpdatedAt), err
}

var errInsightAliasConflict = errors.New("建议内容与另一条建议或其历史文案重复，请调整后重试")

// Called before saving an edit, in that same transaction. Always flatten the
// entire group, including reverting to an earlier spelling.
func prepareInsightAliasEdit(ctx context.Context, tx *sql.Tx, projectID, oldFP, newFP string, now time.Time) error {
	if oldFP == newFP {
		return nil
	}
	d, err := loadInsightDedupState(ctx, tx, projectID)
	if err != nil {
		return err
	}
	if d.permanent[oldFP] || d.permanent[newFP] {
		return errInsightAliasConflict
	}
	if target := d.aliases[newFP]; target != "" && target != oldFP && d.rootActive(target) {
		return errInsightAliasConflict
	}
	if _, err := tx.ExecContext(ctx, `update project_insight_suppressions set canonical_fingerprint='' where project_id=? and fingerprint=? and reason=?`, projectID, newFP, insightSuppressionSuperseded); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update project_insight_suppressions set canonical_fingerprint=? where project_id=? and canonical_fingerprint=? and reason=?`, newFP, projectID, oldFP, insightSuppressionSuperseded); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `insert into project_insight_suppressions (project_id,fingerprint,reason,created_at,canonical_fingerprint) values (?,?,?,?,?)
		on conflict(project_id,fingerprint) do update set canonical_fingerprint=excluded.canonical_fingerprint where project_insight_suppressions.reason=?`,
		projectID, oldFP, insightSuppressionSuperseded, now, newFP, insightSuppressionSuperseded)
	return err
}
