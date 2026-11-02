package darkibox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
)

const apiBase = "https://darkibox.com/api"

// callAPI makes a GET request to the Darkibox API with the given endpoint and params.
// It automatically injects the API key. The result JSON is unmarshalled into out if non-nil.
func (d *Darkibox) callAPI(ctx context.Context, endpoint string, params map[string]string, out any) error {
	query := map[string]string{
		"key": d.APIKey,
	}
	for k, v := range params {
		if strings.TrimSpace(v) == "" {
			continue
		}
		query[k] = v
	}

	var resp apiResponse
	r, err := base.RestyClient.R().
		SetContext(ctx).
		SetQueryParams(query).
		SetResult(&resp).
		Get(apiBase + endpoint)
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK {
		return fmt.Errorf("darkibox http error: %d", r.StatusCode())
	}
	if resp.Status != 200 {
		return fmt.Errorf("darkibox api error: status=%d msg=%s", resp.Status, resp.Msg)
	}
	if out == nil || len(resp.Result) == 0 || string(resp.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("decode darkibox result failed: %w", err)
	}
	return nil
}

// fldIDStr converts a folder ID (which may be the root "0") to a string suitable for API params.
func fldIDStr(id string) string {
	if id == "" {
		return "0"
	}
	return id
}

// encodeFolderID prefixes a folder ID so we can distinguish folders from files.
func encodeFolderID(id int64) string {
	return "d:" + strconv.FormatInt(id, 10)
}

// encodeFileID prefixes a file code so we can distinguish files from folders.
func encodeFileID(code string) string {
	return "f:" + code
}

// folderIDFromObjID extracts the numeric folder ID string from an object ID.
func folderIDFromObjID(id string) string {
	if strings.HasPrefix(id, "d:") {
		return strings.TrimPrefix(id, "d:")
	}
	if id == "" || id == "/" {
		return "0"
	}
	return id
}

// fileCodeFromObjID extracts the file code from an object ID.
func fileCodeFromObjID(id string) string {
	if strings.HasPrefix(id, "f:") {
		return strings.TrimPrefix(id, "f:")
	}
	return id
}
