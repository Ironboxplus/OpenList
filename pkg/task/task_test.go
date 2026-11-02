package task

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/pkg/errors"
)

// waitState polls the task's state until it is want or two seconds pass, and
// reports the last state seen. A fixed sleep before reading is a race against
// the goroutine that runs the task.
func waitState[K comparable](t *testing.T, task *Task[K], want, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := task.GetState()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("task status not %s: %s", what, got)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTask_Manager(t *testing.T) {
	var next atomic.Uint64
	tm := NewTaskManager(3, func(id *uint64) {
		*id = next.Add(1)
	})
	id := tm.Submit(WithCancelCtx(&Task[uint64]{
		Name: "test",
		Func: func(task *Task[uint64]) error {
			time.Sleep(time.Millisecond * 500)
			return nil
		},
	}))
	task, ok := tm.Get(id)
	if !ok {
		t.Fatal("task not found")
	}
	waitState(t, task, RUNNING, "running")
	waitState(t, task, SUCCEEDED, "finished")
}

func TestTask_Cancel(t *testing.T) {
	var next atomic.Uint64
	tm := NewTaskManager(3, func(id *uint64) {
		*id = next.Add(1)
	})
	id := tm.Submit(WithCancelCtx(&Task[uint64]{
		Name: "test",
		Func: func(task *Task[uint64]) error {
			for {
				if utils.IsCanceled(task.Ctx) {
					return nil
				} else {
					t.Logf("task is running")
				}
			}
		},
	}))
	task, ok := tm.Get(id)
	if !ok {
		t.Fatal("task not found")
	}
	waitState(t, task, RUNNING, "running")
	task.Cancel()
	waitState(t, task, CANCELED, "canceled")
}

func TestTask_Retry(t *testing.T) {
	var next atomic.Uint64
	tm := NewTaskManager(3, func(id *uint64) {
		*id = next.Add(1)
	})
	num := 0
	id := tm.Submit(WithCancelCtx(&Task[uint64]{
		Name: "test",
		Func: func(task *Task[uint64]) error {
			num++
			if num&1 == 1 {
				return errors.New("test error")
			}
			return nil
		},
	}))
	task, ok := tm.Get(id)
	if !ok {
		t.Fatal("task not found")
	}
	time.Sleep(time.Millisecond)
	if task.Error == nil {
		t.Error(task.state)
		t.Fatal("task error is nil, but expected error")
	} else {
		t.Logf("task error: %s", task.Error)
	}
	task.retry()
	time.Sleep(time.Millisecond)
	if task.Error != nil {
		t.Errorf("task error: %+v, but expected nil", task.Error)
	}
}
