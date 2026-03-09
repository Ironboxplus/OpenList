package tool

import (
	"context"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	task2 "github.com/OpenListTeam/OpenList/v4/internal/task"
)

type mockTool struct {
	name       string
	addURLFunc func(args *AddUrlArgs) (string, error)
	removeFunc func(task *DownloadTask) error
	statusFunc func(task *DownloadTask) (*Status, error)
	runFunc    func(task *DownloadTask) error
}

func (m *mockTool) Name() string { return m.name }

func (m *mockTool) Items() []model.SettingItem { return nil }

func (m *mockTool) Init() (string, error) { return "ok", nil }

func (m *mockTool) IsReady() bool { return true }

func (m *mockTool) AddURL(args *AddUrlArgs) (string, error) {
	return m.addURLFunc(args)
}

func (m *mockTool) Remove(task *DownloadTask) error {
	return m.removeFunc(task)
}

func (m *mockTool) Status(task *DownloadTask) (*Status, error) {
	return m.statusFunc(task)
}

func (m *mockTool) Run(task *DownloadTask) error {
	return m.runFunc(task)
}

func TestDownloadTaskRun_RemovesCompleted115OpenRecord(t *testing.T) {
	previousDelay := completedOfflineTaskCleanupDelay
	completedOfflineTaskCleanupDelay = 0
	defer func() {
		completedOfflineTaskCleanupDelay = previousDelay
	}()

	removeCount := 0
	tool := &mockTool{
		name: "115 Open",
		addURLFunc: func(args *AddUrlArgs) (string, error) {
			return "gid-1", nil
		},
		removeFunc: func(task *DownloadTask) error {
			removeCount++
			if task.GID != "gid-1" {
				t.Fatalf("unexpected gid: %s", task.GID)
			}
			return nil
		},
		statusFunc: func(task *DownloadTask) (*Status, error) {
			return &Status{
				Completed: true,
				Status:    "completed",
			}, nil
		},
		runFunc: func(task *DownloadTask) error {
			return errs.NotSupport
		},
	}

	task := &DownloadTask{
		TaskExtension: task2.TaskExtension{},
		Url:           "https://example.com/test.torrent",
		DstDirPath:    "/115",
		TempDir:       "/115",
		tool:          tool,
	}
	task.SetCtx(context.Background())

	if err := task.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removeCount != 1 {
		t.Fatalf("want 1 cleanup remove, got %d", removeCount)
	}
}
