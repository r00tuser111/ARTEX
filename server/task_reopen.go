package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
)

const maxReopenReasonRunes = 4000

// reopenHintText is the durable graph hint the planner reads on every later round.
func reopenHintText(reason string) string {
	return "用户打回了完成判定：" + reason + "。被理由否定的目标不能用打回前的旧证据再次 prove_goal。"
}

type reopenTaskResult struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Queued        bool   `json:"queued"`
	ReopenedGoals int    `json:"reopened_goals"`
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// reopenTask is the human rejection of an AI completion. The same task keeps its
// graph, findings, assets and sessions, and goes back to running.
func (s *Server) reopenTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求体过大")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	result, err := s.reopenDoneTask(t, req.Reason)
	if err != nil {
		var se *statusError
		if errors.As(err, &se) {
			writeErr(w, se.code, se.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// reopenDoneTask reopens a done task. Graph changes are committed before admission
// so the first planner round sees the rejection; a failed admission rolls them back.
func (s *Server) reopenDoneTask(t *Task, reason string) (reopenTaskResult, error) {
	reason = strings.TrimSpace(reason)
	if t == nil {
		return reopenTaskResult{}, &statusError{code: http.StatusNotFound, msg: "task not found"}
	}
	if reason == "" {
		return reopenTaskResult{}, &statusError{code: http.StatusBadRequest, msg: "打回原因不能为空"}
	}
	if len([]rune(reason)) > maxReopenReasonRunes {
		return reopenTaskResult{}, &statusError{code: http.StatusBadRequest, msg: "打回原因不能超过 4000 个字符"}
	}
	if s.engine.IsDeleting(t.ID) {
		return reopenTaskResult{}, &statusError{code: http.StatusConflict, msg: "任务正在删除，无法打回"}
	}
	if !s.engine.beginTaskOperation(t.ID) {
		return reopenTaskResult{}, &statusError{code: http.StatusConflict, msg: "任务正在删除，无法打回"}
	}
	defer s.engine.decInflight(t.ID)

	if t.lifecycleSnapshot().Status != "done" {
		return reopenTaskResult{}, &statusError{code: http.StatusConflict, msg: "只有已完成的任务可以打回继续"}
	}

	reopened, createdGoal, hintID, err := prepareReopen(t, reason)
	if err != nil {
		return reopenTaskResult{}, err
	}
	t.queueReopenedTrigger(reason)
	queued, err := s.admitTask(t, "resume")
	if err != nil {
		t.dropReopenedTrigger(reason)
		rollbackReopen(t, reopened, createdGoal, hintID)
		return reopenTaskResult{}, &statusError{code: http.StatusConflict, msg: err.Error()}
	}
	s.engine.emitActivity(t, db.Activity{
		Worker:  "system",
		Kind:    "text",
		Summary: "用户打回完成：" + reason,
	})
	status := "running"
	if queued {
		status = "queued"
	}
	return reopenTaskResult{
		ID:            t.ID,
		Status:        status,
		Queued:        queued,
		ReopenedGoals: len(reopened),
	}, nil
}

// prepareReopen flips met goals back to open, adds a goal when none would remain
// open, and writes the durable hint. On any failure it restores what it changed.
func prepareReopen(t *Task, reason string) (reopened []int64, createdGoal, hintID int64, err error) {
	goals, listErr := t.Store.ListByKind(db.KindGoal, 10000)
	if listErr != nil {
		return nil, 0, 0, &statusError{code: http.StatusInternalServerError, msg: listErr.Error()}
	}
	for _, g := range goals {
		if g.State != "met" {
			continue
		}
		if stateErr := t.Store.SetNodeState(g.ID, "open"); stateErr != nil {
			rollbackReopen(t, reopened, 0, 0)
			return nil, 0, 0, &statusError{code: http.StatusInternalServerError, msg: stateErr.Error()}
		}
		reopened = append(reopened, g.ID)
	}
	open, openErr := t.Store.HasOpenGoal()
	if openErr != nil {
		rollbackReopen(t, reopened, 0, 0)
		return nil, 0, 0, &statusError{code: http.StatusInternalServerError, msg: openErr.Error()}
	}
	if !open {
		id, addErr := t.Store.AddGoal(map[string]any{"text": reason}, "human")
		if addErr != nil {
			rollbackReopen(t, reopened, 0, 0)
			return nil, 0, 0, &statusError{code: http.StatusInternalServerError, msg: addErr.Error()}
		}
		createdGoal = id
		if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
			_ = t.Store.Link(of, db.RelSpawns, id)
		}
	}
	hintID, err = t.Store.AddNode(db.KindHint, map[string]any{"text": reopenHintText(reason)}, 0, "active", "human", nil)
	if err != nil {
		rollbackReopen(t, reopened, createdGoal, 0)
		return nil, 0, 0, &statusError{code: http.StatusInternalServerError, msg: err.Error()}
	}
	return reopened, createdGoal, hintID, nil
}

func rollbackReopen(t *Task, reopened []int64, createdGoal, hintID int64) {
	if t == nil || t.Store == nil {
		return
	}
	for _, id := range reopened {
		_ = t.Store.SetNodeState(id, "met")
	}
	if createdGoal > 0 {
		_ = t.Store.DeleteGoal(createdGoal)
	}
	if hintID > 0 {
		_ = t.Store.DeleteNode(hintID, db.KindHint)
	}
}
