package check

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
)

// RemoteItems reads the stored items over the HTTP API: the admin list for
// the paths, and /api/fs/media (database only, no storage access) for each
// item's external ids, which the list leaves out.
type RemoteItems struct{ API apply.API }

// Items implements ItemSource.
func (r RemoteItems) Items(ctx context.Context, root string) ([]Item, error) {
	root = strings.TrimRight(root, "/")
	existing, err := apply.Remote{API: r.API}.Existing(ctx, root)
	if err != nil {
		return nil, err
	}
	var out []Item
	for p, e := range existing {
		if p != root && !strings.HasPrefix(p, root+"/") {
			continue
		}
		it := Item{Path: p, Kind: e.Kind, Locked: e.Locked}
		if e.Kind != "ignore" && e.Kind != "collection" {
			var resp struct {
				Item *struct {
					ExternalIDs map[string]string `json:"external_ids"`
				} `json:"item"`
			}
			if err := r.API.Call("POST", "/api/fs/media", map[string]any{"path": p}, &resp); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			if resp.Item != nil {
				it.ExternalIDs = resp.Item.ExternalIDs
			}
		}
		out = append(out, it)
	}
	return out, nil
}

// ProbeBytes is how much of a video's head the probe reads.
const ProbeBytes = 4 << 20

// RemoteProber finds a video's raw URL with /api/fs/get and reads its first
// 4 MB with one Range request: at most two requests per probe.
type RemoteProber struct {
	API   apply.API
	Base  string // the server's URL, to resolve a relative raw_url
	Token string // sent only to the server itself, never to the storage host
	HTTP  *http.Client
}

// Probe implements Prober.
func (p RemoteProber) Probe(ctx context.Context, path string) ([]Track, error) {
	var got struct {
		RawURL string `json:"raw_url"`
	}
	if err := p.API.Call("POST", "/api/fs/get", map[string]any{"path": path}, &got); err != nil {
		return nil, err
	}
	if got.RawURL == "" {
		return nil, fmt.Errorf("the server gave no raw_url")
	}
	target, err := url.Parse(got.RawURL)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(p.Base)
	if err != nil {
		return nil, err
	}
	target = base.ResolveReference(target)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", ProbeBytes-1))
	if target.Host == base.Host {
		req.Header.Set("Authorization", p.Token)
	}
	hc := p.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("range request: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, ProbeBytes))
	if err != nil && len(data) == 0 {
		return nil, err
	}
	return ParseMKVTracks(data)
}
