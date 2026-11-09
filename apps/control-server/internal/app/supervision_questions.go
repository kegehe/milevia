package app

import (
	"context"
	"database/sql"
	"encoding/json"
)

func supervisionQuestionRequirementsTx(ctx context.Context, tx *sql.Tx, sessionID, taskID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `select task_id,requirements_revision from supervision_tasks where session_id=? and (?='' or task_id=?) order by task_id`, sessionID, taskID, taskID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	versions := map[string]int64{}
	for rows.Next() {
		var id string
		var revision int64
		if err := rows.Scan(&id, &revision); err != nil {
			return "", err
		}
		versions[id] = revision
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(versions)
	return string(raw), err
}

func expireSupervisionQuestionsTx(ctx context.Context, tx *sql.Tx, sessionID, taskID string) error {
	_, err := tx.ExecContext(ctx, `update supervision_questions set expired=1 where session_id=? and answered_at is null and (?='' or task_id='' or task_id=?)`, sessionID, taskID, taskID)
	return err
}
