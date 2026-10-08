package _115_open

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
)

func tokenFetcher(calls *atomic.Int32, expiration func() string) func(context.Context) (*sdk.UploadGetTokenResp, error) {
	return func(context.Context) (*sdk.UploadGetTokenResp, error) {
		calls.Add(1)
		return &sdk.UploadGetTokenResp{AccessKeyId: "k", Expiration: expiration()}, nil
	}
}

func TestUploadTokenIsFetchedOnceWhileFresh(t *testing.T) {
	var calls atomic.Int32
	fetch := tokenFetcher(&calls, func() string { return time.Now().Add(time.Hour).Format(time.RFC3339) })
	var c uploadTokenCache
	first, err := c.get(context.Background(), fetch)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.get(context.Background(), fetch)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || first != second {
		t.Fatalf("fetches = %d, same token = %v; want 1 fetch and the cached token", calls.Load(), first == second)
	}
}

func TestUploadTokenIsRefetchedOnceItIsWithinTheMarginOfExpiry(t *testing.T) {
	var calls atomic.Int32
	// Valid for 4 more minutes, but the margin is 5: already unusable.
	fetch := tokenFetcher(&calls, func() string { return time.Now().Add(4 * time.Minute).Format(time.RFC3339) })
	var c uploadTokenCache
	for i := 0; i < 2; i++ {
		if _, err := c.get(context.Background(), fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("fetches = %d, want 2 (token inside the 5 minute margin must not be reused)", calls.Load())
	}
}

func TestUploadTokenWithUnparsableExpirationLivesTenMinutes(t *testing.T) {
	now := time.Now()
	got := uploadTokenExpiry(&sdk.UploadGetTokenResp{Expiration: "not a time"}, now)
	if want := now.Add(10 * time.Minute); !got.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got, want)
	}
	exp := now.Add(time.Hour).Truncate(time.Second)
	got = uploadTokenExpiry(&sdk.UploadGetTokenResp{Expiration: exp.Format(time.RFC3339)}, now)
	if want := exp.Add(-5 * time.Minute); !got.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got, want)
	}
}

func TestUploadTokenRefreshReplacesRejectedTokenOnlyOnce(t *testing.T) {
	var calls atomic.Int32
	fetch := tokenFetcher(&calls, func() string { return time.Now().Add(time.Hour).Format(time.RFC3339) })
	var c uploadTokenCache
	old, _ := c.get(context.Background(), fetch)

	// Two uploads both find `old` rejected; only one fetch happens.
	a, err := c.refresh(context.Background(), old, fetch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.refresh(context.Background(), old, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || a == old || a != b {
		t.Fatalf("fetches = %d, a==old %v, a==b %v; want 2 fetches and a shared new token", calls.Load(), a == old, a == b)
	}
	// And the next ordinary get sees the refreshed token.
	if got, _ := c.get(context.Background(), fetch); got != a || calls.Load() != 2 {
		t.Fatal("get after refresh did not return the refreshed token from cache")
	}
}

func TestFormatAPIStatsLine(t *testing.T) {
	line := formatAPIStats(map[string]int64{"UploadInit": 5, "Get": 3, "MakeDir": 1, "Link": 2}, 12340*time.Millisecond)
	want := "api 60s: total=11 UploadInit=5 Get=3 List=0 GetToken=0 Link=2 MakeDir=1 waited=12.3s"
	if line != want {
		t.Fatalf("line = %q\nwant   %q", line, want)
	}
}

func TestWaitAPICountsCallsAndTakeResets(t *testing.T) {
	takeAPIStats()
	d := &Open115{}
	for _, m := range []string{"List", "List", "UploadInit"} {
		if err := d.waitAPI(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	counts, _ := takeAPIStats()
	if counts["List"] != 2 || counts["UploadInit"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	if again, _ := takeAPIStats(); len(again) != 0 || strings.Contains(formatAPIStats(again, 0), "total=1") {
		t.Fatalf("counts not reset: %v", again)
	}
}
