package server

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestReopenDoneTaskReopensMetGoalsAndKeepsEvidence(t *testing.T) {
	m, task, cleanup := newReopenTest(t)
	defer cleanup()

	met, err := task.Store.AddGoal(map[string]any{"text": "拿到 shell"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	other, err := task.Store.AddGoal(map[string]any{"text": "读到 flag"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := task.Store.AddGoal(map[string]any{"text": "已放弃的旁路"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetNodeState(met, "met"); err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetNodeState(other, "met"); err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetNodeState(abandoned, "abandoned"); err != nil {
		t.Fatal(err)
	}
	finding, err := task.Store.AddNode(db.KindFinding, map[string]any{"summary": "保留的漏洞"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.Link(finding, db.RelProves, met); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}

	s := newAdmissionTestServer(m, func(*Task) bool { return false })
	reason := "shell 不是目标要求的稳定权限"
	result, err := s.reopenDoneTask(task, "  "+reason+"  ")
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued || result.Status != "running" || result.ReopenedGoals != 2 {
		t.Fatalf("reopen result = %+v", result)
	}
	if task.lifecycleSnapshot().Status != "running" {
		t.Fatalf("task status = %s", task.lifecycleSnapshot().Status)
	}
	assertGoalState(t, task, met, "open")
	assertGoalState(t, task, other, "open")
	assertGoalState(t, task, abandoned, "abandoned")

	kept, err := task.Store.GetNode(finding)
	if err != nil || kept == nil || kept.Kind != db.KindFinding {
		t.Fatalf("finding lost: node=%+v err=%v", kept, err)
	}
	edges, err := task.Store.Edges(100)
	if err != nil {
		t.Fatal(err)
	}
	proved := false
	for _, edge := range edges {
		if edge.From == finding && edge.To == met && edge.Rel == db.RelProves {
			proved = true
		}
	}
	if !proved {
		t.Fatalf("proves edge removed: %+v", edges)
	}
	assertHintAndTrigger(t, task, reason)
	assertReopenActivity(t, task, reason)
}

func TestReopenDoneTaskWithoutGoalsCreatesOne(t *testing.T) {
	m, task, cleanup := newReopenTest(t)
	defer cleanup()
	if err := m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, func(*Task) bool { return false })
	reason := "还没有验证登录绕过"
	result, err := s.reopenDoneTask(task, reason)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReopenedGoals != 0 || result.Status != "running" {
		t.Fatalf("reopen result = %+v", result)
	}
	goals, err := task.Store.ListByKind(db.KindGoal, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 || goals[0].State != "open" || !strings.Contains(string(goals[0].Payload), reason) {
		t.Fatalf("created goal = %+v", goals)
	}
	assertHintAndTrigger(t, task, reason)
}

func TestReopenRejectsEmptyReasonAndNonDone(t *testing.T) {
	m, task, cleanup := newReopenTest(t)
	defer cleanup()
	if err := m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, func(*Task) bool { return false })

	if _, err := s.reopenDoneTask(task, "   "); err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("empty reason err = %v", err)
	}
	long := strings.Repeat("长", maxReopenReasonRunes+1)
	if _, err := s.reopenDoneTask(task, long); err == nil || !strings.Contains(err.Error(), "4000") {
		t.Fatalf("long reason err = %v", err)
	}
	if task.lifecycleSnapshot().Status != "done" {
		t.Fatalf("rejected reopen changed status to %s", task.lifecycleSnapshot().Status)
	}

	for _, status := range []string{"running", "paused", "failed", "timeout"} {
		if err := m.SetTaskStatus(task.ID, status); err != nil {
			t.Fatal(err)
		}
		if _, err := s.reopenDoneTask(task, "应该拒绝"); err == nil || !strings.Contains(err.Error(), "只有已完成") {
			t.Fatalf("status %s err = %v", status, err)
		}
		if got := task.lifecycleSnapshot().Status; got != status {
			t.Fatalf("status %s changed to %s", status, got)
		}
	}
}

func newReopenTest(t *testing.T) (*Manager, *Task, func()) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	restore := restoreConcurrencySetting(t, m)
	if err := m.SetConcurrency(false, 1); err != nil {
		m.Close()
		t.Fatal(err)
	}
	task, err := m.CreateTask("reopen me", "finish the objective", nil, 0, 0)
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	return m, task, func() {
		_, _ = m.DeleteTask(task.ID, DeleteTaskOptions{})
		restore()
		m.Close()
	}
}

func assertGoalState(t *testing.T, task *Task, id int64, want string) {
	t.Helper()
	node, err := task.Store.GetNode(id)
	if err != nil || node == nil || node.State != want {
		t.Fatalf("goal %d state = %+v err=%v, want %s", id, node, err, want)
	}
}

func assertHintAndTrigger(t *testing.T, task *Task, reason string) {
	t.Helper()
	hints, err := task.Store.ListByKind(db.KindHint, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hint := range hints {
		if strings.Contains(string(hint.Payload), reason) && strings.Contains(string(hint.Payload), "不能用打回前的旧证据") {
			found = true
		}
	}
	if !found {
		t.Fatalf("hint missing reason %q: %+v", reason, hints)
	}
	var matched bool
	for _, ev := range task.drainTriggers() {
		if ev.Kind == "reopened" && ev.Detail == reason {
			matched = true
		}
	}
	if !matched {
		t.Fatal("reopened trigger was not queued")
	}
}

func assertReopenActivity(t *testing.T, task *Task, reason string) {
	t.Helper()
	rows, _, err := task.Store.ActivityList(nil, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Worker == "system" && strings.Contains(row.Summary, reason) {
			return
		}
	}
	t.Fatalf("activity missing reopen reason %q: %+v", reason, rows)
}
