package _115_open

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

type mockOfflineTaskClient struct {
	offlineDownloadFunc            func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error)
	offlineDownloadWithDetailsFunc func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, []sdk.AddOfflineTaskURIsResp, string, error)
	offlineListFunc                func(ctx context.Context) (*sdk.OfflineTaskListResp, error)
	deleteOfflineFunc              func(ctx context.Context, infoHash string, deleteFiles bool) error
	waitLimitFunc                  func(ctx context.Context) error
	waitLimitCalls                 int
}

func (m *mockOfflineTaskClient) OfflineDownload(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
	return m.offlineDownloadFunc(ctx, uris, dstDir)
}

func (m *mockOfflineTaskClient) OfflineDownloadWithDetails(ctx context.Context, uris []string, dstDir model.Obj) ([]string, []sdk.AddOfflineTaskURIsResp, string, error) {
	if m.offlineDownloadWithDetailsFunc == nil {
		hashes, err := m.OfflineDownload(ctx, uris, dstDir)
		return hashes, nil, "", err
	}
	return m.offlineDownloadWithDetailsFunc(ctx, uris, dstDir)
}

func (m *mockOfflineTaskClient) OfflineList(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
	return m.offlineListFunc(ctx)
}

func (m *mockOfflineTaskClient) DeleteOfflineTask(ctx context.Context, infoHash string, deleteFiles bool) error {
	return m.deleteOfflineFunc(ctx, infoHash, deleteFiles)
}

func (m *mockOfflineTaskClient) WaitLimit(ctx context.Context) error {
	m.waitLimitCalls++
	if m.waitLimitFunc != nil {
		return m.waitLimitFunc(ctx)
	}
	return nil
}

func TestIsDuplicateOfflineTaskError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "code 10008", err: fmt.Errorf("code: 10008"), want: true},
		{name: "chinese duplicate", err: fmt.Errorf("任务重复"), want: true},
		{name: "already exists", err: fmt.Errorf("任务已存在"), want: true},
		{name: "english duplicate", err: fmt.Errorf("duplicate task"), want: true},
		{name: "other", err: fmt.Errorf("network timeout"), want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isDuplicateOfflineTaskError(tc.err); got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestOfflineTaskURLMatches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		taskURL string
		rawURL  string
		want    bool
	}{
		{
			name:    "exact match",
			taskURL: "ed2k://|file|test.avi|123|ABC|/",
			rawURL:  "ed2k://|file|test.avi|123|ABC|/",
			want:    true,
		},
		{
			name:    "percent encoded file name",
			taskURL: "ed2k://|file|[AVS]Azumi Mizushima [ネオパンストフェティッシュ Ver.19 水嶋あずみ](NOP-019)(2011.01.13).avi|1593601796|9E5CCC55541BD46EE8252BF100EFC46D|/",
			rawURL:  "ed2k://|file|[AVS]Azumi%20Mizushima%20[ネオパンストフェティッシュ%20Ver.19%20水嶋あずみ](NOP-019)(2011.01.13).avi|1593601796|9E5CCC55541BD46EE8252BF100EFC46D|/",
			want:    true,
		},
		{
			name:    "case and trailing slash normalized",
			taskURL: "ED2K://|FILE|TEST.AVI|123|ABC|",
			rawURL:  "ed2k://|file|test.avi|123|abc|/",
			want:    true,
		},
		{
			name:    "different link",
			taskURL: "ed2k://|file|a.avi|123|ABC|/",
			rawURL:  "ed2k://|file|b.avi|123|ABC|/",
			want:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := offlineTaskURLMatches(tc.taskURL, tc.rawURL); got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestOfflineTaskMatches(t *testing.T) {
	t.Parallel()

	t.Run("match ed2k by parsed fields when task url differs", func(t *testing.T) {
		t.Parallel()

		task := sdk.OfflineTask{
			InfoHash: "server-task-hash",
			Name:     "[AVS]Azumi Mizushima [ネオパンストフェティッシュ Ver.19 水嶋あずみ](NOP-019)(2011.01.13).avi",
			Size:     1593601796,
			URL:      "",
		}
		rawURL := "ed2k://|file|[AVS]Azumi%20Mizushima%20[ネオパンストフェティッシュ%20Ver.19%20水嶋あずみ](NOP-019)(2011.01.13).avi|1593601796|9E5CCC55541BD46EE8252BF100EFC46D|/"

		if !offlineTaskMatches(task, rawURL) {
			t.Fatal("expected task to match by ed2k parsed fields")
		}
	})

	t.Run("do not match different ed2k size", func(t *testing.T) {
		t.Parallel()

		task := sdk.OfflineTask{
			Name: "[AVS]Azumi Mizushima [ネオパンストフェティッシュ Ver.19 水嶋あずみ](NOP-019)(2011.01.13).avi",
			Size: 1,
		}
		rawURL := "ed2k://|file|[AVS]Azumi%20Mizushima%20[ネオパンストフェティッシュ%20Ver.19%20水嶋あずみ](NOP-019)(2011.01.13).avi|1593601796|9E5CCC55541BD46EE8252BF100EFC46D|/"

		if offlineTaskMatches(task, rawURL) {
			t.Fatal("expected task not to match")
		}
	})

	t.Run("magnet still matches by url", func(t *testing.T) {
		t.Parallel()

		rawURL := "magnet:?xt=urn:btih:1234567890ABCDEF1234567890ABCDEF12345678&dn=test"
		task := sdk.OfflineTask{
			InfoHash: "1234567890abcdef1234567890abcdef12345678",
			URL:      rawURL,
		}

		if !offlineTaskMatches(task, rawURL) {
			t.Fatal("expected magnet task to match by url")
		}
	})

	t.Run("match magnet by btih despite noisy tracker", func(t *testing.T) {
		t.Parallel()

		rawURL := "magnet:?xt=urn:btih:1234567890ABCDEF1234567890ABCDEF12345678&dn=test"
		task := sdk.OfflineTask{
			InfoHash: "1234567890abcdef1234567890abcdef12345678",
			URL:      "magnet:?xt=urn:btih:1234567890ABCDEF1234567890ABCDEF12345678&dn=test&tr=%3C!DOCTYPE%20html%3E",
		}

		if !offlineTaskMatches(task, rawURL) {
			t.Fatal("expected magnet task to match by btih")
		}
	})

	t.Run("match http by host and path", func(t *testing.T) {
		t.Parallel()

		rawURL := "https://example.com/files/test.mp4"
		task := sdk.OfflineTask{
			URL: "https://EXAMPLE.com/files/test.mp4?token=abc",
		}

		if !offlineTaskMatches(task, rawURL) {
			t.Fatal("expected http task to match by host and path")
		}
	})
}

func TestAddOfflineDownloadTask(t *testing.T) {
	t.Parallel()

	const (
		testURL     = "https://example.com/test.torrent"
		firstHash   = "hash-1"
		staleHash   = "hash-stale"
		deleteError = "delete failed"
	)

	t.Run("success on first try", func(t *testing.T) {
		t.Parallel()

		listCount := 0
		client := &mockOfflineTaskClient{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				return []string{firstHash}, nil
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				listCount++
				return &sdk.OfflineTaskListResp{Tasks: nil}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				t.Fatal("DeleteOfflineTask should not be called")
				return nil
			},
		}

		hashes, err := addOfflineDownloadTask(context.Background(), client, testURL, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hashes) != 1 || hashes[0] != firstHash {
			t.Fatalf("unexpected hashes: %+v", hashes)
		}
		if listCount < 1 {
			t.Fatalf("want pre-add offline list call, got %d", listCount)
		}
	})

	t.Run("wait limit applied for add flow", func(t *testing.T) {
		t.Parallel()

		client := &mockOfflineTaskClient{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				return []string{firstHash}, nil
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				return &sdk.OfflineTaskListResp{Tasks: nil}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				return nil
			},
		}

		_, err := addOfflineDownloadTask(context.Background(), client, testURL, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if client.waitLimitCalls < 2 {
			t.Fatalf("want wait limit calls >= 2, got %d", client.waitLimitCalls)
		}
	})

	t.Run("delete duplicate and retry", func(t *testing.T) {
		t.Parallel()

		callCount := 0
		deleteCount := 0
		listCount := 0
		client := &mockOfflineTaskClient{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				callCount++
				if callCount == 1 {
					return nil, fmt.Errorf("code: 10008, message: 任务已存在")
				}
				return []string{firstHash}, nil
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				listCount++
				return &sdk.OfflineTaskListResp{
					Tasks: []sdk.OfflineTask{
						{InfoHash: staleHash, URL: testURL},
					},
				}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				deleteCount++
				if infoHash != staleHash {
					t.Fatalf("unexpected hash: %s", infoHash)
				}
				if deleteFiles {
					t.Fatal("deleteFiles should be false")
				}
				return nil
			},
		}

		hashes, err := addOfflineDownloadTask(context.Background(), client, testURL, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hashes) != 1 || hashes[0] != firstHash {
			t.Fatalf("unexpected hashes: %+v", hashes)
		}
		if callCount != 2 {
			t.Fatalf("want 2 download attempts, got %d", callCount)
		}
		if deleteCount != 2 {
			t.Fatalf("want 2 delete attempts (pre-add + duplicate), got %d", deleteCount)
		}
		if listCount < 2 {
			t.Fatalf("want at least 2 offline list calls, got %d", listCount)
		}
	})

	t.Run("delete duplicate magnet and retry", func(t *testing.T) {
		t.Parallel()

		callCount := 0
		deleteCount := 0
		listCount := 0
		magnetURL := "magnet:?xt=urn:btih:1234567890ABCDEF1234567890ABCDEF12345678&dn=test"
		client := &mockOfflineTaskClient{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				callCount++
				if callCount == 1 {
					return nil, fmt.Errorf("code: 10008, message: 任务已存在")
				}
				return []string{firstHash}, nil
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				listCount++
				return &sdk.OfflineTaskListResp{
					Tasks: []sdk.OfflineTask{
						{InfoHash: staleHash, URL: magnetURL},
					},
				}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				deleteCount++
				if infoHash != staleHash {
					t.Fatalf("unexpected hash: %s", infoHash)
				}
				if deleteFiles {
					t.Fatal("deleteFiles should be false")
				}
				return nil
			},
		}

		hashes, err := addOfflineDownloadTask(context.Background(), client, magnetURL, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hashes) != 1 || hashes[0] != firstHash {
			t.Fatalf("unexpected hashes: %+v", hashes)
		}
		if callCount != 2 {
			t.Fatalf("want 2 download attempts, got %d", callCount)
		}
		if deleteCount != 2 {
			t.Fatalf("want 2 delete attempts (pre-add + duplicate), got %d", deleteCount)
		}
		if listCount < 2 {
			t.Fatalf("want at least 2 offline list calls, got %d", listCount)
		}
	})

	t.Run("delete duplicate and retry with decoded ed2k url", func(t *testing.T) {
		t.Parallel()

		callCount := 0
		deleteCount := 0
		listCount := 0
		decodedURL := "ed2k://|file|[AVS]Azumi Mizushima [ネオパンストフェティッシュ Ver.19 水嶋あずみ](NOP-019)(2011.01.13).avi|1593601796|9E5CCC55541BD46EE8252BF100EFC46D|/"
		encodedURL := "ed2k://|file|[AVS]Azumi%20Mizushima%20[ネオパンストフェティッシュ%20Ver.19%20水嶋あずみ](NOP-019)(2011.01.13).avi|1593601796|9E5CCC55541BD46EE8252BF100EFC46D|/"

		client := &mockOfflineTaskClient{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				callCount++
				if callCount == 1 {
					return nil, fmt.Errorf("code: 10008, message: 任务已存在，请勿输入重复的链接地址")
				}
				return []string{firstHash}, nil
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				listCount++
				return &sdk.OfflineTaskListResp{
					Tasks: []sdk.OfflineTask{
						{InfoHash: staleHash, URL: decodedURL},
					},
				}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				deleteCount++
				if infoHash != staleHash {
					t.Fatalf("unexpected hash: %s", infoHash)
				}
				if deleteFiles {
					t.Fatal("deleteFiles should be false")
				}
				return nil
			},
		}

		hashes, err := addOfflineDownloadTask(context.Background(), client, encodedURL, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hashes) != 1 || hashes[0] != firstHash {
			t.Fatalf("unexpected hashes: %+v", hashes)
		}
		if callCount != 2 {
			t.Fatalf("want 2 download attempts, got %d", callCount)
		}
		if deleteCount != 2 {
			t.Fatalf("want 2 delete attempts (pre-add + duplicate), got %d", deleteCount)
		}
		if listCount < 2 {
			t.Fatalf("want at least 2 offline list calls, got %d", listCount)
		}
	})

	t.Run("delete duplicate and retry with empty task url but matching name and size", func(t *testing.T) {
		t.Parallel()

		callCount := 0
		deleteCount := 0
		listCount := 0
		encodedURL := "ed2k://|file|[AVS]Azumi%20Mizushima%20[ネオパンストフェティッシュ%20Ver.19%20水嶋あずみ](NOP-019)(2011.01.13).avi|1593601796|9E5CCC55541BD46EE8252BF100EFC46D|/"

		client := &mockOfflineTaskClient{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				callCount++
				if callCount == 1 {
					return nil, fmt.Errorf("code: 10008, message: 任务已存在，请勿输入重复的链接地址")
				}
				return []string{firstHash}, nil
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				listCount++
				return &sdk.OfflineTaskListResp{
					Tasks: []sdk.OfflineTask{
						{
							InfoHash: staleHash,
							Name:     "[AVS]Azumi Mizushima [ネオパンストフェティッシュ Ver.19 水嶋あずみ](NOP-019)(2011.01.13).avi",
							Size:     1593601796,
							URL:      "",
						},
					},
				}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				deleteCount++
				if infoHash != staleHash {
					t.Fatalf("unexpected hash: %s", infoHash)
				}
				if deleteFiles {
					t.Fatal("deleteFiles should be false")
				}
				return nil
			},
		}

		hashes, err := addOfflineDownloadTask(context.Background(), client, encodedURL, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hashes) != 1 || hashes[0] != firstHash {
			t.Fatalf("unexpected hashes: %+v", hashes)
		}
		if callCount != 2 {
			t.Fatalf("want 2 download attempts, got %d", callCount)
		}
		if deleteCount != 2 {
			t.Fatalf("want 2 delete attempts (pre-add + duplicate), got %d", deleteCount)
		}
		if listCount < 2 {
			t.Fatalf("want at least 2 offline list calls, got %d", listCount)
		}
	})

	t.Run("delete duplicate directly from add response info hash", func(t *testing.T) {
		t.Parallel()

		callCount := 0
		deleteCount := 0
		listCount := 0
		client := &mockOfflineTaskClient{
			offlineDownloadWithDetailsFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, []sdk.AddOfflineTaskURIsResp, string, error) {
				callCount++
				if callCount == 1 {
					return nil, []sdk.AddOfflineTaskURIsResp{
						{InfoHash: staleHash, URL: testURL},
					}, `{"state":false,"code":10008,"message":"任务已存在","data":[{"info_hash":"hash-stale","url":"` + testURL + `"}]}`, fmt.Errorf("code: 10008, message: 任务已存在")
				}
				return []string{firstHash}, nil, "", nil
			},
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				return nil, fmt.Errorf("unexpected fallback OfflineDownload call")
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				listCount++
				return &sdk.OfflineTaskListResp{Tasks: nil}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				deleteCount++
				if infoHash != staleHash {
					t.Fatalf("unexpected hash: %s", infoHash)
				}
				if deleteFiles {
					t.Fatal("deleteFiles should be false")
				}
				return nil
			},
		}

		hashes, err := addOfflineDownloadTask(context.Background(), client, testURL, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(hashes) != 1 || hashes[0] != firstHash {
			t.Fatalf("unexpected hashes: %+v", hashes)
		}
		if callCount != 2 {
			t.Fatalf("want 2 download attempts, got %d", callCount)
		}
		if deleteCount != 1 {
			t.Fatalf("want 1 delete attempt, got %d", deleteCount)
		}
		if listCount < 1 {
			t.Fatalf("want pre-add offline list call, got %d", listCount)
		}
	})

	t.Run("duplicate delete failure", func(t *testing.T) {
		t.Parallel()

		listCount := 0
		client := &mockOfflineTaskClient{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				return nil, fmt.Errorf("duplicate task")
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				listCount++
				return &sdk.OfflineTaskListResp{
					Tasks: []sdk.OfflineTask{
						{InfoHash: staleHash, URL: testURL},
					},
				}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				return fmt.Errorf(deleteError)
			},
		}

		_, err := addOfflineDownloadTask(context.Background(), client, testURL, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), deleteError) {
			t.Fatalf("unexpected error: %v", err)
		}
		if listCount < 1 {
			t.Fatalf("want pre-add offline list call, got %d", listCount)
		}
	})

	t.Run("non duplicate error is returned", func(t *testing.T) {
		t.Parallel()

		listCount := 0
		client := &mockOfflineTaskClient{
			offlineDownloadFunc: func(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error) {
				return nil, fmt.Errorf("network timeout")
			},
			offlineListFunc: func(ctx context.Context) (*sdk.OfflineTaskListResp, error) {
				listCount++
				return &sdk.OfflineTaskListResp{Tasks: nil}, nil
			},
			deleteOfflineFunc: func(ctx context.Context, infoHash string, deleteFiles bool) error {
				t.Fatal("DeleteOfflineTask should not be called")
				return nil
			},
		}

		_, err := addOfflineDownloadTask(context.Background(), client, testURL, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "network timeout") {
			t.Fatalf("unexpected error: %v", err)
		}
		if listCount < 1 {
			t.Fatalf("want pre-add offline list call, got %d", listCount)
		}
	})
}

func TestOpen115BasicMethods(t *testing.T) {
	t.Parallel()

	o := &Open115{}

	if o.Name() != "115 Open" {
		t.Fatalf("unexpected name: %s", o.Name())
	}
	if o.Items() != nil {
		t.Fatal("Items should return nil")
	}
	msg, err := o.Init()
	if err != nil {
		t.Fatalf("unexpected init error: %v", err)
	}
	if msg != "ok" {
		t.Fatalf("unexpected init message: %s", msg)
	}
}
