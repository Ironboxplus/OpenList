package handles

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
)

// A browser downloading one file in parallel: six range requests on /d at the
// same time, each reading its sixth of the file. The storage allows two
// connections per file, so the layer holds the others back until a slot is
// free; every request is answered with the right bytes and the storage refuses
// none.
func TestParallelRangesOfOneFileStayWithinTheStorageLimit(t *testing.T) {
	r := newGateRig(t, 2)

	const parts = 6
	size := (gateFileSize + parts - 1) / parts
	var wg sync.WaitGroup
	for i := 0; i < parts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := i * size
			want := min(size, gateFileSize-start)
			req, _ := http.NewRequest("GET", r.server.URL+"/d/gsrc/big.bin?d", nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("range %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				t.Errorf("range %d got status %d", i, resp.StatusCode)
				return
			}
			got, err := io.ReadAll(io.LimitReader(resp.Body, int64(want)))
			if err != nil || len(got) != want {
				t.Errorf("range %d read %d bytes, want %d, err %v", i, len(got), want, err)
				return
			}
			if sha256.Sum256(got) != sha256.Sum256(r.data[start:start+want]) {
				t.Errorf("range %d differs from the source", i)
			}
		}()
	}
	wg.Wait()
	r.checkConnections(0)
}
