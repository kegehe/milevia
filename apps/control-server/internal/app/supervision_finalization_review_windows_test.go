//go:build windows

package app

import (
	"context"
	"fmt"
	"testing"
)

func TestSupervisionNoAcceptedTasksEndsAsFailedAndReleasesOwnership(t *testing.T) {
	for _, disposition := range []string{"unresolved", "deferred"} {
		for _, implemented := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/implemented=%v", disposition, implemented), func(t *testing.T) {
				var s *Server
				var taskID string
				var ss supervisionSession
				if implemented {
					s, _, taskID, ss, _, _ = implementedWorkflowForTest(t)
				} else {
					s, _, taskID, ss, _ = supervisionPlanFixture(t)
				}
				applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "record_task_disposition", SessionID: ss.ID, TaskID: taskID, Disposition: disposition, Instruction: "本任务仍缺少必要实现或有效验收证据，无法继续"})
				ss = supervisionSessionForTest(t, s, ss.ID)
				if ss.State != "failed" {
					t.Fatal("zero accepted tasks were reported as partial completion", ss.State)
				}
				var status string
				if err := s.db.QueryRow(`select status from tasks where id=?`, taskID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				want := taskTodo
				if implemented {
					want = taskActionRequired
				}
				if status != want {
					t.Fatal("failure changed task implementation facts", status, want)
				}
				var event string
				if err := s.db.QueryRow(`select type from supervision_events where session_id=? and type in ('session.completed','session.completed_with_issues','session.failed') order by seq desc limit 1`, ss.ID).Scan(&event); err != nil || event != "session.failed" {
					t.Fatal("failure left a misleading completion event", event, err)
				}
				if err := s.releaseTerminalSupervision(context.Background(), ss); err != nil {
					t.Fatal(err)
				}
				assertExecutionClaimForTest(t, s, taskID, "", "")
			})
		}
	}
}
