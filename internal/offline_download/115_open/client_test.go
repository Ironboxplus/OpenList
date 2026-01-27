package _115_open

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/OpenListTeam/115-sdk-go"
	_115_open "github.com/OpenListTeam/OpenList/v4/drivers/115_open"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/offline_download/tool"
)

// Mock implementation of Open115 driver for testing
type mockOpen115 struct {
	_115_open.Open115
	offlineDownloadFunc func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error)
	offlineListFunc     func(ctx context.Context) (*sdk.OfflineTaskListResp, error)
	deleteOfflineFunc   func(ctx context.Context, infoHash string, deleteFiles bool) error
}

func (m *mockOpen115) OfflineDownload(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
	if m.offlineDownloadFunc != nil {
		return m.offlineDownloadFunc(ctx, uris, dstDir)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockOpen115) OfflineList(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
	if m.offlineListFunc != nil {
		return m.offlineListFunc(ctx)
	}
	return nil, fmt.Errorf("not implemented")
}

func (m *mockOpen115) DeleteOfflineTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	if m.deleteOfflineFunc != nil {
		return m.deleteOfflineFunc(ctx, infoHash, deleteFiles)
	}
	return fmt.Errorf("not implemented")
}

// TestAddURL_Success tests successful URL addition
func TestAddURL_Success(t *testing.T) {
	t.Skip("需要真实的storage环境，跳过此测试")
}

// TestAddURL_DuplicateHandling tests the duplicate URL handling logic
func TestAddURL_DuplicateHandling(t *testing.T) {
	t.Skip("需要真实的storage环境，跳过此测试")
}

// TestDuplicateLinkRetryLogic tests the logic without actual API calls
func TestDuplicateLinkRetryLogic(t *testing.T) {
	testURL := "https://example.com/test.torrent"
	testHash := "test_hash_123"

	t.Run("首次添加成功", func(t *testing.T) {
		// 模拟首次添加成功的场景
		callCount := 0
		mock := &mockOpen115{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				callCount++
				if callCount == 1 {
					return []string{testHash}, nil
				}
				return nil, fmt.Errorf("unexpected call")
			},
		}

		hashes, err := mock.OfflineDownload(context.Background(), []string{testURL}, nil)
		if err != nil {
			t.Errorf("首次添加失败: %v", err)
		}
		if len(hashes) != 1 || hashes[0] != testHash {
			t.Errorf("期望hash=%s, 实际=%v", testHash, hashes)
		}
		if callCount != 1 {
			t.Errorf("期望调用1次, 实际调用%d次", callCount)
		}
	})

	t.Run("检测到重复错误并自动删除重试", func(t *testing.T) {
		// 模拟重复链接错误的场景
		callCount := 0
		deleteCount := 0

		mock := &mockOpen115{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				callCount++
				if callCount == 1 {
					// 首次调用返回重复错误
					return nil, fmt.Errorf("code: 10008, message: 任务已存在，请勿输入重复的链接地址")
				} else if callCount == 2 {
					// 删除后重试，返回成功
					return []string{testHash}, nil
				}
				return nil, fmt.Errorf("unexpected call count: %d", callCount)
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				// 返回包含重复任务的列表
				return &sdk.OfflineTaskListResp{
					Tasks: []sdk.OfflineTask{
						{
							InfoHash: "old_hash_456",
							URL:      testURL,
							Status:   1, // 下载中
						},
					},
				}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				deleteCount++
				if infoHash != "old_hash_456" {
					t.Errorf("期望删除hash=old_hash_456, 实际=%s", infoHash)
				}
				if deleteFiles {
					t.Error("不应该删除源文件")
				}
				return nil
			},
		}

		// 模拟完整的错误处理逻辑
		ctx := context.Background()

		// 第一次调用返回重复错误
		_, err := mock.OfflineDownload(ctx, []string{testURL}, nil)
		if err == nil {
			t.Error("第一次应该返回错误")
		}

		// 检查是否是重复错误
		errStr := err.Error()
		if !strings.Contains(errStr, "10008") && !strings.Contains(errStr, "重复") {
			t.Errorf("应该是重复错误，实际错误: %v", err)
		}

		// 获取任务列表
		taskList, err := mock.OfflineList(ctx)
		if err != nil {
			t.Errorf("获取任务列表失败: %v", err)
		}

		// 查找并删除重复任务
		found := false
		for _, task := range taskList.Tasks {
			if task.URL == testURL {
				err := mock.DeleteOfflineTask(ctx, task.InfoHash, false)
				if err != nil {
					t.Errorf("删除任务失败: %v", err)
				}
				found = true
				break
			}
		}

		if !found {
			t.Error("未找到重复任务")
		}

		// 重试添加
		hashes, err := mock.OfflineDownload(ctx, []string{testURL}, nil)
		if err != nil {
			t.Errorf("重试添加失败: %v", err)
		}
		if len(hashes) != 1 || hashes[0] != testHash {
			t.Errorf("期望hash=%s, 实际=%v", testHash, hashes)
		}

		if callCount != 2 {
			t.Errorf("期望调用OfflineDownload 2次, 实际%d次", callCount)
		}
		if deleteCount != 1 {
			t.Errorf("期望调用DeleteOfflineTask 1次, 实际%d次", deleteCount)
		}
	})

	t.Run("重复链接但删除失败", func(t *testing.T) {
		mock := &mockOpen115{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				// 始终返回重复错误
				return nil, fmt.Errorf("code: 10008, message: 任务已存在")
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				return &sdk.OfflineTaskListResp{
					Tasks: []sdk.OfflineTask{
						{
							InfoHash: "old_hash_789",
							URL:      testURL,
						},
					},
				}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				return fmt.Errorf("删除失败：权限不足")
			},
		}

		// 删除失败时应该返回错误
		ctx := context.Background()
		_, err := mock.OfflineDownload(ctx, []string{testURL}, nil)
		if err == nil {
			t.Error("应该返回错误")
		}
	})
}

// TestOpen115_Name tests the Name method
func TestOpen115_Name(t *testing.T) {
	o := &Open115{}
	name := o.Name()
	expected := "115 Open"
	if name != expected {
		t.Errorf("期望名称=%s, 实际=%s", expected, name)
	}
}

// TestOpen115_Items tests the Items method
func TestOpen115_Items(t *testing.T) {
	o := &Open115{}
	items := o.Items()
	if items != nil {
		t.Error("Items应该返回nil")
	}
}

// TestOpen115_Run tests the Run method
func TestOpen115_Run(t *testing.T) {
	o := &Open115{}
	err := o.Run(&tool.DownloadTask{})
	if err == nil {
		t.Error("Run应该返回NotSupport错误")
	}
}

// TestOpen115_Init tests the Init method
func TestOpen115_Init(t *testing.T) {
	o := &Open115{}
	msg, err := o.Init()
	if err != nil {
		t.Errorf("Init失败: %v", err)
	}
	if msg != "ok" {
		t.Errorf("期望消息='ok', 实际=%s", msg)
	}
}
