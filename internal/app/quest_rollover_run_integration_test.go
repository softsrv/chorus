//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/softsrv/chorus/internal/app"
)

// TestIntegration_QuestRolloverRun: a schedule due today in a household that
// has never rolled over (as after a server that was down at midnight) is
// assigned by the startup run of RunQuestRollover, without waiting for a
// tick, and the function returns promptly once its context is cancelled.
func TestIntegration_QuestRolloverRun(t *testing.T) {
	w := newScheduleWorld(t, "questrolloverrun")
	ann := w.member("Ann")
	quest := w.quest(app.QuestInput{Name: "Water the plants", Points: num(5)})
	w.schedule(quest, ann, 0)

	runCtx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.RunQuestRollover(runCtx, w.q, w.pool)
	}()

	assigned := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if isHeldBy(w.holder(quest), ann) {
			assigned = true
			break
		}
	}
	if !assigned {
		t.Fatal("the startup run did not assign the due schedule")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunQuestRollover did not return after its context was cancelled")
	}
}
