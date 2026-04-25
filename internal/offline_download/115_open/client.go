package _115_open

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	_115_open "github.com/OpenListTeam/OpenList/v4/drivers/115_open"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/offline_download/tool"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	log "github.com/sirupsen/logrus"
)

type Open115 struct {
}

type offlineTaskClient interface {
	OfflineDownload(ctx context.Context, uris []string, dstDir model.Obj) ([]string, error)
	OfflineList(ctx context.Context) (*sdk.OfflineTaskListResp, error)
	DeleteOfflineTask(ctx context.Context, infoHash string, deleteFiles bool) error
}

type offlineTaskDetailClient interface {
	OfflineDownloadWithDetails(ctx context.Context, uris []string, dstDir model.Obj) ([]string, []sdk.AddOfflineTaskURIsResp, string, error)
}

type offlineTaskLimiter interface {
	WaitLimit(ctx context.Context) error
}

func waitOfflineTaskLimit(ctx context.Context, client offlineTaskClient) error {
	limiter, ok := client.(offlineTaskLimiter)
	if !ok {
		return nil
	}
	return limiter.WaitLimit(ctx)
}

func (o *Open115) Name() string {
	return "115 Open"
}

func (o *Open115) Items() []model.SettingItem {
	return nil
}

func (o *Open115) Run(task *tool.DownloadTask) error {
	return errs.NotSupport
}

func (o *Open115) Init() (string, error) {
	return "ok", nil
}

func (o *Open115) IsReady() bool {
	tempDir := setting.GetStr(conf.Pan115OpenTempDir)
	if tempDir == "" {
		return false
	}
	storage, _, err := op.GetStorageAndActualPath(tempDir)
	if err != nil {
		return false
	}
	if _, ok := storage.(*_115_open.Open115); !ok {
		return false
	}
	return true
}

func (o *Open115) AddURL(args *tool.AddUrlArgs) (string, error) {
	storage, actualPath, err := op.GetStorageAndActualPath(args.TempDir)
	if err != nil {
		return "", err
	}
	driver115Open, ok := storage.(*_115_open.Open115)
	if !ok {
		return "", fmt.Errorf("unsupported storage driver for offline download, only 115 Cloud is supported")
	}

	if err := op.MakeDir(args.Ctx, storage, actualPath); err != nil {
		return "", err
	}

	parentDir, err := op.GetUnwrap(args.Ctx, storage, actualPath)
	if err != nil {
		return "", err
	}
	log.Infof("[115_open] AddURL start: temp_dir=%q actual_path=%q parent_id=%q parent_name=%q url=%q", args.TempDir, actualPath, parentDir.GetID(), parentDir.GetName(), args.Url)
	logOfflineURLDetails("[115_open] AddURL input", args.Url)

	hashs, err := addOfflineDownloadTask(args.Ctx, driver115Open, args.Url, parentDir)
	if err != nil {
		return "", err
	}

	if len(hashs) < 1 {
		return "", fmt.Errorf("failed to add offline download task: no task hash returned")
	}

	return hashs[0], nil
}

func addOfflineDownloadTask(ctx context.Context, client offlineTaskClient, url string, parentDir model.Obj) ([]string, error) {
	parentID, parentName := "<nil>", "<nil>"
	if parentDir != nil {
		parentID = parentDir.GetID()
		parentName = parentDir.GetName()
	}
	log.Infof("[115_open] addOfflineDownloadTask: parent_id=%q parent_name=%q url=%q", parentID, parentName, url)
	logOfflineURLDetails("[115_open] addOfflineDownloadTask target", url)
	if err := preCleanDuplicateOfflineTasks(ctx, client, url); err != nil {
		return nil, err
	}
	hashs, addItems, rawResp, err := offlineDownloadWithDetails(ctx, client, url, parentDir)
	log.Infof("[115_open] addOfflineDownloadTask first attempt result: hashes=%v err=%v add_items=%d", hashs, err, len(addItems))
	if err == nil {
		return hashs, nil
	}
	if !isDuplicateOfflineTaskError(err) {
		return nil, fmt.Errorf("failed to add offline download task: %w", err)
	}
	log.Infof("[115_open] duplicate offline task detected, trying cleanup before retry")
	if rawResp != "" {
		log.Infof("[115_open] duplicate add response: %s", rawResp)
	}
	for _, item := range addItems {
		log.Infof("[115_open] duplicate add item: state=%v code=%d info_hash=%q url=%q", item.State, item.Code, item.InfoHash, item.URL)
		logOfflineURLDetails("[115_open] duplicate add item url", item.URL)
		if item.InfoHash == "" {
			log.Infof("[115_open] skipping add-response duplicate item: empty info_hash")
			continue
		}
		if item.URL != "" && !offlineTaskURLMatches(item.URL, url) {
			log.Infof("[115_open] skipping add-response duplicate item: url mismatch")
			continue
		}
		log.Infof("[115_open] deleting duplicate task directly from add response: info_hash=%s url=%s", item.InfoHash, item.URL)
		if err := waitOfflineTaskLimit(ctx, client); err != nil {
			return nil, err
		}
		if deleteErr := client.DeleteOfflineTask(ctx, item.InfoHash, false); deleteErr != nil {
			log.Errorf("[115_open] delete duplicate task from add response failed: info_hash=%s err=%v", item.InfoHash, deleteErr)
			return nil, fmt.Errorf("failed to delete duplicate offline download task from add response: %w", deleteErr)
		}
		log.Infof("[115_open] delete duplicate task from add response success: info_hash=%s", item.InfoHash)
		waitForOfflineTaskRemoval(ctx, client, item.InfoHash)
		hashs, retryItems, retryRawResp, retryErr := offlineDownloadWithDetails(ctx, client, url, parentDir)
		log.Infof("[115_open] retry add after add-response delete: hashes=%v err=%v add_items=%d", hashs, retryErr, len(retryItems))
		if retryRawResp != "" {
			log.Infof("[115_open] retry add raw response after add-response delete: %s", retryRawResp)
		}
		err = retryErr
		if err != nil {
			return nil, fmt.Errorf("failed to add offline download task after removing duplicate: %w", err)
		}
		return hashs, nil
	}
	if err := waitOfflineTaskLimit(ctx, client); err != nil {
		return nil, err
	}
	taskList, listErr := client.OfflineList(ctx)
	if listErr != nil || taskList == nil {
		return nil, fmt.Errorf("failed to add offline download task: %w", err)
	}
	log.Infof("[115_open] offline list returned %d tasks across %d pages", len(taskList.Tasks), taskList.PageCount)
	for _, task := range taskList.Tasks {
		matched, reason := offlineTaskMatchReason(task, url)
		log.Infof("[115_open] duplicate candidate: info_hash=%s status=%d size=%d name=%q url=%q matched=%v reason=%s", task.InfoHash, task.Status, task.Size, task.Name, task.URL, matched, reason)
		logOfflineURLDetails("[115_open] duplicate candidate url", task.URL)
		if !matched {
			continue
		}
		log.Infof("[115_open] matched duplicate offline task: info_hash=%s, name=%s", task.InfoHash, task.Name)
		log.Infof("[115_open] deleting matched duplicate offline task: info_hash=%s status=%d size=%d", task.InfoHash, task.Status, task.Size)
		if err := waitOfflineTaskLimit(ctx, client); err != nil {
			return nil, err
		}
		if deleteErr := client.DeleteOfflineTask(ctx, task.InfoHash, false); deleteErr != nil {
			log.Errorf("[115_open] delete matched duplicate offline task failed: info_hash=%s err=%v", task.InfoHash, deleteErr)
			return nil, fmt.Errorf("failed to delete duplicate offline download task: %w", deleteErr)
		}
		log.Infof("[115_open] delete matched duplicate offline task success: info_hash=%s", task.InfoHash)
		waitForOfflineTaskRemoval(ctx, client, task.InfoHash)
		hashs, retryItems, retryRawResp, retryErr := offlineDownloadWithDetails(ctx, client, url, parentDir)
		log.Infof("[115_open] retry add after matched delete: hashes=%v err=%v add_items=%d", hashs, retryErr, len(retryItems))
		if retryRawResp != "" {
			log.Infof("[115_open] retry add raw response after matched delete: %s", retryRawResp)
		}
		err = retryErr
		if err != nil {
			return nil, fmt.Errorf("failed to add offline download task after removing duplicate: %w", err)
		}
		return hashs, nil
	}
	log.Warnf("[115_open] duplicate offline task detected but no matching task found in offline list")
	return nil, fmt.Errorf("failed to add offline download task: %w", err)
}

func preCleanDuplicateOfflineTasks(ctx context.Context, client offlineTaskClient, url string) error {
	if err := waitOfflineTaskLimit(ctx, client); err != nil {
		return err
	}
	taskList, listErr := client.OfflineList(ctx)
	if listErr != nil || taskList == nil {
		log.Warnf("[115_open] pre-add offline list failed: err=%v", listErr)
		return nil
	}
	log.Infof("[115_open] pre-add offline list returned %d tasks across %d pages", len(taskList.Tasks), taskList.PageCount)
	deleted := 0
	for _, task := range taskList.Tasks {
		matched, reason := offlineTaskMatchReason(task, url)
		log.Infof("[115_open] pre-add duplicate candidate: info_hash=%s status=%d size=%d name=%q url=%q matched=%v reason=%s", task.InfoHash, task.Status, task.Size, task.Name, task.URL, matched, reason)
		logOfflineURLDetails("[115_open] pre-add duplicate candidate url", task.URL)
		if !matched {
			continue
		}
		log.Infof("[115_open] pre-add deleting matched duplicate offline task: info_hash=%s status=%d size=%d", task.InfoHash, task.Status, task.Size)
		if err := waitOfflineTaskLimit(ctx, client); err != nil {
			return err
		}
		if deleteErr := client.DeleteOfflineTask(ctx, task.InfoHash, false); deleteErr != nil {
			log.Errorf("[115_open] pre-add delete matched duplicate offline task failed: info_hash=%s err=%v", task.InfoHash, deleteErr)
			return fmt.Errorf("failed to delete duplicate offline download task: %w", deleteErr)
		}
		deleted++
		log.Infof("[115_open] pre-add delete matched duplicate offline task success: info_hash=%s", task.InfoHash)
		waitForOfflineTaskRemoval(ctx, client, task.InfoHash)
	}
	if deleted == 0 {
		log.Infof("[115_open] pre-add duplicate scan found no matches")
	}
	return nil
}

func offlineDownloadWithDetails(ctx context.Context, client offlineTaskClient, url string, parentDir model.Obj) ([]string, []sdk.AddOfflineTaskURIsResp, string, error) {
	if err := waitOfflineTaskLimit(ctx, client); err != nil {
		return nil, nil, "", err
	}
	if detailClient, ok := client.(offlineTaskDetailClient); ok {
		return detailClient.OfflineDownloadWithDetails(ctx, []string{url}, parentDir)
	}
	hashs, err := client.OfflineDownload(ctx, []string{url}, parentDir)
	return hashs, nil, "", err
}

func isDuplicateOfflineTaskError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "10008") ||
		strings.Contains(errStr, "重复") ||
		strings.Contains(errStr, "已存在") ||
		strings.Contains(errStr, "duplicate")
}

func offlineTaskURLMatches(taskURL string, rawURL string) bool {
	taskVariants := normalizedOfflineTaskURLVariants(taskURL)
	rawVariants := normalizedOfflineTaskURLVariants(rawURL)
	for candidate := range taskVariants {
		if _, ok := rawVariants[candidate]; ok {
			return true
		}
	}
	return false
}

func offlineTaskMatches(task sdk.OfflineTask, rawURL string) bool {
	matched, _ := offlineTaskMatchReason(task, rawURL)
	return matched
}

func offlineTaskMatchReason(task sdk.OfflineTask, rawURL string) (bool, string) {
	if offlineTaskURLMatches(task.URL, rawURL) {
		return true, "url variants matched"
	}
	if httpURLMatches(task.URL, rawURL) {
		return true, "http url host+path matched"
	}
	rawMagnet := parseMagnetBTIH(rawURL)
	if rawMagnet != "" {
		taskHash := normalizeInfoHash(task.InfoHash)
		if taskHash != "" && taskHash == rawMagnet {
			return true, "task info_hash matched raw magnet"
		}
		taskURLHash := parseMagnetBTIH(task.URL)
		if taskURLHash != "" && taskURLHash == rawMagnet {
			return true, "task url magnet hash matched"
		}
		return false, fmt.Sprintf("task magnet hash mismatch: task_info_hash=%q task_url_hash=%q raw_hash=%q", taskHash, taskURLHash, rawMagnet)
	}
	taskED2K, rawED2K := parseED2KLink(task.URL), parseED2KLink(rawURL)
	if taskED2K != nil && rawED2K != nil {
		if taskED2K.Hash == rawED2K.Hash {
			if taskED2K.Size == rawED2K.Size {
				return true, "task url ed2k hash matched"
			}
			return true, "task url ed2k hash matched despite size mismatch"
		}
		return false, fmt.Sprintf("task url ed2k mismatch: task=%s raw=%s", taskED2K.String(), rawED2K.String())
	}
	if rawED2K == nil {
		return false, "raw url is not ed2k and url variants did not match"
	}
	if normalizeOfflineTaskURL(task.InfoHash) == rawED2K.Hash {
		if task.Size == rawED2K.Size {
			return true, "task info_hash matched raw ed2k"
		}
		return true, "task info_hash matched raw ed2k despite size mismatch"
	}
	taskName := normalizeOfflineTaskURL(task.Name)
	if taskName == normalizeOfflineTaskURL(rawED2K.Name) && task.Size == rawED2K.Size {
		return true, "task name and size matched raw ed2k"
	}
	return false, fmt.Sprintf("task name/hash/size mismatch: task_name=%q raw_name=%q task_info_hash=%q raw_hash=%q task_size=%d raw_size=%d", taskName, normalizeOfflineTaskURL(rawED2K.Name), normalizeOfflineTaskURL(task.InfoHash), rawED2K.Hash, task.Size, rawED2K.Size)
}

func normalizedOfflineTaskURLVariants(raw string) map[string]struct{} {
	variants := map[string]struct{}{}
	queue := []string{raw}
	for len(queue) > 0 {
		current := normalizeOfflineTaskURL(queue[0])
		queue = queue[1:]
		if current == "" {
			continue
		}
		if _, ok := variants[current]; ok {
			continue
		}
		variants[current] = struct{}{}
		if decoded, err := url.QueryUnescape(current); err == nil && decoded != current {
			queue = append(queue, decoded)
		}
		if decoded, err := url.PathUnescape(current); err == nil && decoded != current {
			queue = append(queue, decoded)
		}
	}
	return variants
}

func httpURLMatches(taskURL, rawURL string) bool {
	taskNormalized := normalizeHTTPURL(taskURL)
	rawNormalized := normalizeHTTPURL(rawURL)
	if taskNormalized == "" || rawNormalized == "" {
		return false
	}
	return taskNormalized == rawNormalized
}

func normalizeHTTPURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Host)
	if host == "" {
		return ""
	}
	path := strings.ToLower(parsed.Path)
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, path)
}

func normalizeOfflineTaskURL(raw string) string {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return ""
	}
	normalized = strings.TrimSuffix(normalized, "/")
	return strings.ToLower(normalized)
}

type ed2kLink struct {
	Name string
	Size int64
	Hash string
}

func parseED2KLink(raw string) *ed2kLink {
	normalized := strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(normalized), "ed2k://|file|") {
		return nil
	}
	parts := strings.Split(normalized, "|")
	if len(parts) < 6 {
		return nil
	}
	name, err := url.PathUnescape(parts[2])
	if err != nil {
		name = parts[2]
	}
	size, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return nil
	}
	return &ed2kLink{
		Name: normalizeOfflineTaskURL(name),
		Size: size,
		Hash: normalizeOfflineTaskURL(parts[4]),
	}
}

func parseMagnetBTIH(raw string) string {
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	idx := strings.Index(lower, "btih:")
	if idx == -1 {
		return ""
	}
	candidate := raw[idx+len("btih:"):]
	if candidate == "" {
		return ""
	}
	for i, ch := range candidate {
		if ch == '&' || ch == '#' || ch == '/' {
			candidate = candidate[:i]
			break
		}
	}
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	if decoded, err := url.QueryUnescape(candidate); err == nil {
		candidate = decoded
	}
	return normalizeInfoHash(candidate)
}

func normalizeInfoHash(raw string) string {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return ""
	}
	normalized = strings.TrimPrefix(strings.ToLower(normalized), "urn:btih:")
	if normalized == "" {
		return ""
	}
	if len(normalized) == 40 && isHexString(normalized) {
		return normalized
	}
	if len(normalized) == 32 && isBase32String(normalized) {
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(normalized))
		if err == nil && len(decoded) == 20 {
			return hex.EncodeToString(decoded)
		}
	}
	return normalized
}

func isHexString(value string) bool {
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}

func isBase32String(value string) bool {
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '2' && ch <= '7') {
			continue
		}
		return false
	}
	return true
}

func (e *ed2kLink) Equal(other *ed2kLink) bool {
	if e == nil || other == nil {
		return false
	}
	return e.Name == other.Name && e.Size == other.Size && e.Hash == other.Hash
}

func (e *ed2kLink) String() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("name=%q size=%d hash=%q", e.Name, e.Size, e.Hash)
}

func logOfflineURLDetails(prefix string, raw string) {
	if raw == "" {
		log.Infof("%s details: raw is empty", prefix)
		return
	}
	variants := normalizedOfflineTaskURLVariants(raw)
	log.Infof("%s details: raw=%q normalized_variants=%v", prefix, raw, mapKeys(variants))
	if parsedMagnet := parseMagnetBTIH(raw); parsedMagnet != "" {
		log.Infof("%s details: parsed_magnet_hash=%s", prefix, parsedMagnet)
	}
	if parsed := parseED2KLink(raw); parsed != nil {
		log.Infof("%s details: parsed_ed2k=%s", prefix, parsed.String())
	}
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func waitForOfflineTaskRemoval(ctx context.Context, client offlineTaskClient, infoHash string) {
	const maxChecks = 3
	for attempt := 1; attempt <= maxChecks; attempt++ {
		if err := waitOfflineTaskLimit(ctx, client); err != nil {
			log.Warnf("[115_open] post-delete wait limit failed: info_hash=%s attempt=%d err=%v", infoHash, attempt, err)
			return
		}
		taskList, err := client.OfflineList(ctx)
		if err != nil {
			log.Warnf("[115_open] post-delete check failed: info_hash=%s attempt=%d err=%v", infoHash, attempt, err)
			return
		}
		stillExists := false
		taskStatus := -999
		taskName := ""
		for _, task := range taskList.Tasks {
			if normalizeOfflineTaskURL(task.InfoHash) != normalizeOfflineTaskURL(infoHash) {
				continue
			}
			stillExists = true
			taskStatus = task.Status
			taskName = task.Name
			break
		}
		log.Infof("[115_open] post-delete check: info_hash=%s attempt=%d exists=%v status=%d name=%q task_count=%d", infoHash, attempt, stillExists, taskStatus, taskName, len(taskList.Tasks))
		if !stillExists {
			return
		}
		if attempt < maxChecks {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (o *Open115) Remove(task *tool.DownloadTask) error {
	storage, _, err := op.GetStorageAndActualPath(task.TempDir)
	if err != nil {
		return err
	}
	driver115Open, ok := storage.(*_115_open.Open115)
	if !ok {
		return fmt.Errorf("unsupported storage driver for offline download, only 115 Open is supported")
	}

	ctx := context.Background()
	if err := driver115Open.DeleteOfflineTask(ctx, task.GID, false); err != nil {
		return err
	}
	return nil
}

func (o *Open115) Status(task *tool.DownloadTask) (*tool.Status, error) {
	storage, _, err := op.GetStorageAndActualPath(task.TempDir)
	if err != nil {
		return nil, err
	}
	driver115Open, ok := storage.(*_115_open.Open115)
	if !ok {
		return nil, fmt.Errorf("unsupported storage driver for offline download, only 115 Open is supported")
	}

	tasks, err := driver115Open.OfflineList(context.Background())
	if err != nil {
		return nil, err
	}

	s := &tool.Status{
		Progress:  0,
		NewGID:    "",
		Completed: false,
		Status:    "the task has been deleted",
		Err:       nil,
	}

	for _, t := range tasks.Tasks {
		if t.InfoHash == task.GID {
			s.Progress = float64(t.PercentDone)
			s.Status = t.GetStatus()
			s.Completed = t.IsDone()
			s.TotalBytes = t.Size
			if t.IsFailed() {
				s.Err = errors.New(t.GetStatus())
			}
			return s, nil
		}
	}
	// 任务不在列表中，可能已完成或被删除
	s.Progress = 100
	s.Completed = true
	return s, nil
}

var _ tool.Tool = (*Open115)(nil)

func init() {
	tool.Tools.Add(&Open115{})
}
