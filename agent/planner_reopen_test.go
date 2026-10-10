package agent

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestRenderTriggersReopened(t *testing.T) {
	got := renderTriggers(&db.ExplorationStore{}, []TriggerEvent{{
		Kind:   "reopened",
		Detail: "覆盖度只有 40%，目标要求 100%",
	}})
	for _, want := range []string{"用户打回了任务完成", "覆盖度只有 40%", "旧证据", "prove_goal"} {
		if !strings.Contains(got, want) {
			t.Fatalf("trigger text missing %q:\n%s", want, got)
		}
	}
}
