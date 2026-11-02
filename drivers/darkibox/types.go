package darkibox

import "encoding/json"

// apiResponse is the common wrapper for all Darkibox API responses.
type apiResponse struct {
	Msg        string          `json:"msg"`
	Result     json.RawMessage `json:"result"`
	ServerTime string          `json:"server_time"`
	Status     int             `json:"status"`
}

// accountInfoResult represents the result of /api/account/info
type accountInfoResult struct {
	Email       string `json:"email"`
	Balance     string `json:"balance"`
	StorageUsed string `json:"storage_used"`
}

// fileListResult represents the result of /api/file/list
type fileListResult struct {
	Results      int        `json:"results"`
	ResultsTotal int        `json:"results_total"`
	Files        []fileItem `json:"files"`
}

type fileItem struct {
	FileCode string `json:"file_code"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Size     int64  `json:"size"`
	Uploaded string `json:"uploaded"`
	FldID    int64  `json:"fld_id"`
}

// folderListResult represents the result of /api/folder/list
type folderListResult struct {
	Folders []folderItem `json:"folders"`
}

type folderItem struct {
	FldID int64  `json:"fld_id"`
	Name  string `json:"name"`
	Code  string `json:"code"`
}

// directLinkResult represents the result of /api/file/direct_link
type directLinkResult struct {
	Versions []directLinkVersion `json:"versions"`
}

type directLinkVersion struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// uploadServerResult represents the result of /api/upload/server
type uploadServerResult struct {
	URL string `json:"url"`
}

// uploadResult represents the response from the upload endpoint
type uploadResult struct {
	Files []uploadedFile `json:"files"`
}

type uploadedFile struct {
	FileCode string `json:"filecode"`
	URL      string `json:"url"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Status   int    `json:"status"`
}

// folderCreateResult represents the result of /api/folder/create
type folderCreateResult struct {
	FldID int64  `json:"fld_id"`
	Name  string `json:"name"`
}
