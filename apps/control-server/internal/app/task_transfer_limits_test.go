package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTaskTransferExportEnforcesImportLimits(t *testing.T) {
	s := newTestServer(t)
	p := newTaskTransferProject(t, s, "export-limits")
	target := newTaskTransferProject(t, s, "import-limits")
	now := time.Now().UTC()
	_, err := s.db.Exec(`with recursive n(x) as (select 1 union all select x+1 from n where x < ?)
		insert into tasks (id,project_id,title,description,priority,pinned,position,status,created_at,updated_at)
		select 'limit-'||x,?,'title','description-'||x,'normal',0,x,'done',?,? from n`, taskTransferMaxTasks+1, p, now, now)
	if err != nil {
		t.Fatal(err)
	}
	handler := s.routes()
	checkRejected := func(scope, message string) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+p+"/tasks/export?scope="+scope, nil))
		if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), message) {
			t.Fatalf("export must reject before download: %d %s", response.Code, response.Body)
		}
	}
	checkRejected("all", "2000")
	// 改选未完成范围仍能正常导出，不受历史任务数量影响。
	if _, err := s.db.Exec(`update tasks set status='todo' where id='limit-1'`); err != nil {
		t.Fatal(err)
	}
	if file := exportTasksForTest(t, handler, p, "active"); len(file.Tasks) != 1 {
		t.Fatalf("active scope = %d", len(file.Tasks))
	}
	if _, err := s.db.Exec(`delete from tasks where id=?`, fmt.Sprintf("limit-%d", taskTransferMaxTasks+1)); err != nil {
		t.Fatal(err)
	}
	file := exportTasksForTest(t, handler, p, "all")
	if len(file.Tasks) != taskTransferMaxTasks {
		t.Fatalf("limit boundary = %d", len(file.Tasks))
	}
	status, result, message := importTasksForTest(t, handler, target, file, true)
	if status != http.StatusOK || result.Imported != taskTransferMaxTasks {
		t.Fatalf("export at limit cannot round-trip: %d %+v %s", status, result, message)
	}
	// 2000 条合法中文描述，序列化后超过 64 MiB，不能生成成功下载。
	if _, err := s.db.Exec(`update tasks set description=? where project_id=?`, strings.Repeat("测", 12000), p); err != nil {
		t.Fatal(err)
	}
	checkRejected("all", "64MB")
}
