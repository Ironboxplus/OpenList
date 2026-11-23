package handles

import (
	"crypto/sha256"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

// A player that has two ranges of one file open (each range a download over the
// 2 connections the link asks for) is two playbacks: together they hold no more
// than the 2 connections the storage allows, so neither is refused. No
// connection goes beyond the limit: a download's first connection takes the
// slot of another download's extra worker, or waits for a free one, and its
// other workers wait behind the first connections.
func TestTwoPlaybacksOfOneFileStayWithinTheStorageLimit(t *testing.T) {
	r := newGateRig(t, 2)

	const played = 24 << 20
	starts := []int64{0, 20 << 20}
	var wg sync.WaitGroup
	for _, start := range starts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", r.server.URL+"/d/gsrc/big.bin?d", nil)
			req.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				t.Errorf("playback from %d got status %d", start, resp.StatusCode)
				return
			}
			got, err := io.ReadAll(io.LimitReader(resp.Body, played))
			if err != nil || len(got) != played {
				t.Errorf("playback from %d read %d bytes, err %v", start, len(got), err)
				return
			}
			if sha256.Sum256(got) != sha256.Sum256(r.data[start:start+played]) {
				t.Errorf("the bytes played from %d differ from the source", start)
			}
		}()
	}
	wg.Wait()
	r.checkConnections(0)
}
