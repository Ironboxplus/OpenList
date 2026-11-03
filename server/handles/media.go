package handles

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	stdpath "path"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/media"
	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/media/store"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// scrapeConcurrency 刮削并发度（默认 5）
// 走 setting 读取，未配置时使用 5；远端 API 调用密集型任务，并发 5 在限流与吞吐间较平衡。
const defaultScrapeConcurrency = 5

// ==================== 配置管理 ====================

// ListMediaConfigs 获取所有媒体库配置
func ListMediaConfigs(c *gin.Context) {
	types := []model.MediaType{
		model.MediaTypeVideo,
		model.MediaTypeMusic,
		model.MediaTypeImage,
		model.MediaTypeBook,
	}
	cfgs := make([]*model.MediaConfig, 0, len(types))
	for _, t := range types {
		cfg, err := db.GetMediaConfig(t)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		cfgs = append(cfgs, cfg)
	}
	common.SuccessResp(c, cfgs)
}

// SaveMediaConfigReq 保存配置请求
type SaveMediaConfigReq struct {
	MediaType     model.MediaType `json:"media_type" binding:"required"`
	Enabled       bool            `json:"enabled"`
	ScraperConfig *string         `json:"scraper_config"`
}

// SaveMediaConfig 保存媒体库配置
func SaveMediaConfig(c *gin.Context) {
	var req SaveMediaConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	// 读取已有配置，避免用零值覆盖 last_scan_at / last_scrape_at
	cfg, err := db.GetMediaConfig(req.MediaType)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	cfg.Enabled = req.Enabled
	if req.ScraperConfig != nil {
		cfg.ScraperConfig = *req.ScraperConfig
	}
	if err := db.SaveMediaConfig(cfg); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// ==================== 扫描路径管理 ====================

// ListMediaScanPaths 获取指定媒体类型的扫描路径列表
func ListMediaScanPaths(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	paths, err := db.ListMediaScanPaths(mediaType)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, paths)
}

// CreateMediaScanPathReq 创建扫描路径请求
type CreateMediaScanPathReq struct {
	MediaType    model.MediaType `json:"media_type" binding:"required"`
	Name         string          `json:"name"`
	Path         string          `json:"path" binding:"required"`
	PathMerge    bool            `json:"path_merge"`
	TypeTag      string          `json:"type_tag"`
	ContentTags  string          `json:"content_tags"`
	EnableScrape bool            `json:"enable_scrape"`
}

// CreateMediaScanPath 创建扫描路径
func CreateMediaScanPath(c *gin.Context) {
	var req CreateMediaScanPathReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if req.Path == "" {
		req.Path = "/"
	}
	name := req.Name
	if name == "" {
		name = req.Path
	}
	sp := &model.MediaScanPath{
		MediaType:    req.MediaType,
		Name:         name,
		Path:         req.Path,
		PathMerge:    req.PathMerge,
		TypeTag:      req.TypeTag,
		ContentTags:  req.ContentTags,
		EnableScrape: req.EnableScrape,
	}
	if err := db.CreateMediaScanPath(sp); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, sp)
}

// UpdateMediaScanPathReq 更新扫描路径请求
type UpdateMediaScanPathReq struct {
	ID           uint   `json:"id" binding:"required"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	PathMerge    bool   `json:"path_merge"`
	TypeTag      string `json:"type_tag"`
	ContentTags  string `json:"content_tags"`
	EnableScrape bool   `json:"enable_scrape"`
}

// UpdateMediaScanPath 更新扫描路径
func UpdateMediaScanPath(c *gin.Context) {
	var req UpdateMediaScanPathReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	sp, err := db.GetMediaScanPath(req.ID)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	sp.Name = req.Name
	sp.Path = req.Path
	sp.PathMerge = req.PathMerge
	sp.TypeTag = req.TypeTag
	sp.ContentTags = req.ContentTags
	sp.EnableScrape = req.EnableScrape
	if err := db.UpdateMediaScanPath(sp); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// DeleteMediaScanPath 删除扫描路径
func DeleteMediaScanPath(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.ErrorStrResp(c, "无效的ID", 400)
		return
	}
	if err := db.DeleteMediaScanPath(uint(id)); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// ClearMediaScanPathDB 清空指定扫描路径的媒体数据
func ClearMediaScanPathDB(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.ErrorStrResp(c, "无效的ID", 400)
		return
	}
	if err := db.ClearMediaItemsByScanPath(uint(id)); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// ==================== 媒体条目管理（后台） ====================

// clampMediaPage 限制分页参数，避免 page_size 过大或为负导致整表扫描 / 响应体爆炸
func clampMediaPage(page, pageSize, defaultSize, maxSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = defaultSize
	}
	if pageSize > maxSize {
		pageSize = maxSize
	}
	return page, pageSize
}

// ListMediaItemsAdmin 后台分页查询媒体条目
func ListMediaItemsAdmin(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	page, pageSize = clampMediaPage(page, pageSize, 20, 500)
	keyword := c.Query("keyword")
	orderBy := c.DefaultQuery("order_by", "name")
	orderDir := c.DefaultQuery("order_dir", "asc")
	scanPathIDStr := c.Query("scan_path_id")
	var scanPathID uint
	if scanPathIDStr != "" {
		if id, err := strconv.ParseUint(scanPathIDStr, 10, 64); err == nil {
			scanPathID = uint(id)
		}
	}

	q := db.MediaItemQuery{
		MediaType:  mediaType,
		ScanPathID: scanPathID,
		Keyword:    keyword,
		OrderBy:    orderBy,
		TitleSource: mediaTitleSource(),
		OrderDir:   orderDir,
		Page:       page,
		PageSize:   pageSize,
	}
	items, total, err := db.ListMediaItems(q)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, common.PageResp{Content: items, Total: total})
}

// UpdateMediaItemReq 更新媒体条目请求
type UpdateMediaItemReq struct {
	ID          uint    `json:"id" binding:"required"`
	ScrapedName string  `json:"scraped_name"`
	Description string  `json:"description"`
	Cover       string  `json:"cover"`
	ReleaseDate string  `json:"release_date"`
	Rating      float32 `json:"rating"`
	Genre       string  `json:"genre"`
	Authors     string  `json:"authors"`
	Plot        string  `json:"plot"`
	Reviews     string  `json:"reviews"`
	AlbumName   string  `json:"album_name"`
	AlbumArtist string  `json:"album_artist"`
	Publisher   string  `json:"publisher"`
	ISBN        string  `json:"isbn"`
	Hidden      bool    `json:"hidden"`
}

// UpdateMediaItemAdmin 后台更新媒体条目
func UpdateMediaItemAdmin(c *gin.Context) {
	var req UpdateMediaItemReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	item, err := db.GetMediaItemByID(req.ID)
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	item.ScrapedName = req.ScrapedName
	item.Description = req.Description
	item.Cover = req.Cover
	item.ReleaseDate = req.ReleaseDate
	item.Rating = req.Rating
	item.Genre = req.Genre
	item.Authors = req.Authors
	item.Plot = req.Plot
	item.Reviews = req.Reviews
	item.AlbumName = req.AlbumName
	item.AlbumArtist = req.AlbumArtist
	item.Publisher = req.Publisher
	item.ISBN = req.ISBN
	item.Hidden = req.Hidden

	if err := db.UpdateMediaItem(item); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// DeleteMediaItemAdmin 后台删除媒体条目
func DeleteMediaItemAdmin(c *gin.Context) {
	idStr := c.Query("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.ErrorStrResp(c, "无效的ID", 400)
		return
	}
	if err := db.DeleteMediaItem(uint(id)); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// ClearMediaDB 清空指定类型媒体数据库
func ClearMediaDB(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	if mediaType == "" {
		common.ErrorStrResp(c, "media_type 不能为空", 400)
		return
	}
	if err := db.ClearMediaItems(mediaType); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c)
}

// ClearMediaScrape 清空刮削数据（保留扫描记录，但清空所有刮削结果字段）
// 参数：media_type 可选，留空表示所有类型；item_id 暂不支持（统一为类型/全部）
func ClearMediaScrape(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	affected, err := db.ClearMediaScrapedData(mediaType)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// 同步清掉对应媒体配置中的最近刮削时间
	if mediaType != "" {
		if cfg, e := db.GetMediaConfig(mediaType); e == nil && cfg != nil {
			cfg.LastScrapeAt = nil
			_ = db.SaveMediaConfig(cfg)
		}
	} else {
		if cfgs, e := db.GetAllMediaConfigs(); e == nil {
			for i := range cfgs {
				cfgs[i].LastScrapeAt = nil
				_ = db.SaveMediaConfig(&cfgs[i])
			}
		}
	}
	common.SuccessResp(c, gin.H{"affected": affected})
}

// DeleteInvalidMedia 删除已失效的媒体条目（即对应文件 / 文件夹在存储中已不存在）
// 参数：media_type 可选，留空表示扫描所有类型
// 返回：检测总数、被删除条目数
func DeleteInvalidMedia(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))

	items, err := db.ListAllValidMediaItemsForCheck(mediaType)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var invalidIDs []uint
	var skipped int
	for i := range items {
		it := &items[i]
		// 拼接文件 / 文件夹的完整 VFS 路径
		// - 文件夹模式 (is_folder=true): folder_path 即文件夹自身（扫描根 + 文件夹名 视情况而定）；这里直接使用 folder_path/file_name
		// - 普通文件: folder_path 是所在目录，file_name 是文件名
		fullPath := stdpath.Join(it.FolderPath, it.FileName)
		if fullPath == "" || fullPath == "/" {
			continue
		}
		if _, gerr := fs.Get(ctx, fullPath, &fs.GetArgs{NoLog: true}); gerr != nil {
			// 只有「确认不存在」才算失效条目；驱动故障、超时、鉴权失败等可能是临时问题，
			// 一律跳过（保留记录），否则一次网络抖动就会把有效的媒体记录硬删除。
			if errs.IsObjectNotFound(gerr) {
				invalidIDs = append(invalidIDs, it.ID)
				continue
			}
			skipped++
			log.Warnf("跳过无法确认状态的媒体条目 %s（%s）: %v", fullPath, it.FileName, gerr)
		}
	}

	if len(invalidIDs) > 0 {
		if err := db.DeleteMediaItemsByIDs(invalidIDs); err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	log.Infof("删除已失效媒体条目：检测 %d 条，删除 %d 条，跳过 %d 条 (media_type=%q)", len(items), len(invalidIDs), skipped, mediaType)
	common.SuccessResp(c, gin.H{
		"checked": len(items),
		"deleted": len(invalidIDs),
		"skipped": skipped,
	})
}

// ==================== 扫描与刮削 ====================

// ScanMediaReq 扫描请求
type ScanMediaReq struct {
	MediaType  model.MediaType `json:"media_type" binding:"required"`
	ScanPathID uint            `json:"scan_path_id"` // 0 表示扫描全部路径
}

// StartMediaScan 开始扫描
func StartMediaScan(c *gin.Context) {
	var req ScanMediaReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	cfg, err := db.GetMediaConfig(req.MediaType)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if !cfg.Enabled {
		common.ErrorStrResp(c, "该媒体库未启用", 400)
		return
	}

	if req.ScanPathID > 0 {
		// 扫描单个路径
		sp, err := db.GetMediaScanPath(req.ScanPathID)
		if err != nil {
			common.ErrorResp(c, err, 404)
			return
		}
		media.ScanMediaPath(sp)
	} else {
		// 扫描全部路径
		media.ScanMedia(cfg)
	}
	common.SuccessResp(c)
}

// GetMediaScanProgress 获取扫描进度
func GetMediaScanProgress(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	if mediaType == "" {
		common.ErrorStrResp(c, "media_type 不能为空", 400)
		return
	}
	progress := media.GetProgress(mediaType)
	common.SuccessResp(c, progress)
}

// MediaScrapeStatsResp 刮削统计响应
// 包含 TMDB 视频刮削的累计指标，用于运维观测与命中率评估
type MediaScrapeStatsResp struct {
	TMDB VideoScrapeStats `json:"tmdb"`
}

// VideoScrapeStats 视频刮削统计（原子计数，进程级累计值，重启清零）
//   - TotalScraped       : 累计刮削次数
//   - HitCount           : 强命中（已取回详情）次数
//   - LowConfidenceCount : 只有低置信度候选、留待人工挑选的次数
//   - NoMatchCount       : 完全未命中次数
//   - TotalAttempts      : 保留字段，恒为 0（搜索请求数由各 provider 内部限流，不再单独计数）
//   - TotalSearchMillis  : 累计刮削耗时（毫秒）
type VideoScrapeStats struct {
	TotalScraped       int64
	HitCount           int64
	NoMatchCount       int64
	LowConfidenceCount int64
	TotalAttempts      int64
	TotalSearchMillis  int64
}

var videoScrapeStats VideoScrapeStats

func (s *VideoScrapeStats) snapshot() VideoScrapeStats {
	return VideoScrapeStats{
		TotalScraped:       atomic.LoadInt64(&s.TotalScraped),
		HitCount:           atomic.LoadInt64(&s.HitCount),
		NoMatchCount:       atomic.LoadInt64(&s.NoMatchCount),
		LowConfidenceCount: atomic.LoadInt64(&s.LowConfidenceCount),
		TotalSearchMillis:  atomic.LoadInt64(&s.TotalSearchMillis),
	}
}

func (s *VideoScrapeStats) record(res *apply.ScrapeResult, took time.Duration) {
	atomic.AddInt64(&s.TotalScraped, 1)
	atomic.AddInt64(&s.TotalSearchMillis, took.Milliseconds())
	switch {
	case res != nil && res.Matched && res.Provider != "":
		atomic.AddInt64(&s.HitCount, 1)
	case res != nil && len(res.Candidates) > 0:
		atomic.AddInt64(&s.LowConfidenceCount, 1)
	default:
		atomic.AddInt64(&s.NoMatchCount, 1)
	}
}

// GetMediaScrapeStats 获取视频刮削统计
func GetMediaScrapeStats(c *gin.Context) {
	common.SuccessResp(c, MediaScrapeStatsResp{
		TMDB: videoScrapeStats.snapshot(),
	})
}

// ScrapeMediaReq 刮削请求
type ScrapeMediaReq struct {
	MediaType model.MediaType `json:"media_type" binding:"required"`
	ItemID    uint            `json:"item_id"` // 0 表示刮削全部未刮削的
}

// StartMediaScrape 开始刮削
func StartMediaScrape(c *gin.Context) {
	var req ScrapeMediaReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	cfg, err := db.GetMediaConfig(req.MediaType)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	// 从系统设置中读取刮削配置
	discogsToken := setting.GetStr(conf.MediaDiscogsToken)
	discogsAPIURL := setting.GetStr(conf.MediaDiscogsAPIURL, "https://api.discogs.com")
	thumbnailMode := setting.GetStr(conf.MediaThumbnailMode, "runtime")
	thumbnailPath := setting.GetStr(conf.MediaThumbnailPath, "/imgs")
	storeThumbnail := setting.GetBool(conf.MediaStoreThumbnail)
	bookSource := strings.ToLower(setting.GetStr(conf.MediaBookSource, "douban"))
	googleBooksKey := setting.GetStr(conf.MediaGoogleBooksKey)
	downloadCover := setting.GetBool(conf.MediaDownloadCover)

	// 该媒体类型如果单独配置过缩略图，覆盖全局设置
	if cfg.ScraperConfig != "" {
		var sc struct {
			StoreThumbnail string `json:"store_thumbnail"`
			ThumbnailMode  string `json:"thumbnail_mode"`
			ThumbnailPath  string `json:"thumbnail_path"`
		}
		if json.Unmarshal([]byte(cfg.ScraperConfig), &sc) == nil {
			if sc.ThumbnailMode != "" {
				thumbnailMode = sc.ThumbnailMode
			}
			if sc.ThumbnailPath != "" {
				thumbnailPath = sc.ThumbnailPath
			}
			if sc.StoreThumbnail != "" {
				storeThumbnail = sc.StoreThumbnail == "true"
			}
		}
	}

	log.Infof("[media] scrape %s: thumbnail mode=%s, path=%s, runtime dir=%s",
		req.MediaType, thumbnailMode, thumbnailPath, scraper.RuntimeThumbRoot(thumbnailPath))

	var preloadedItems []model.MediaItem
	if req.ItemID > 0 {
		item, e := db.GetMediaItemByID(req.ItemID)
		if e != nil {
			common.ErrorResp(c, e, 404)
			return
		}
		preloadedItems = []model.MediaItem{*item}
	}

	go func() {
		var items []model.MediaItem
		var err error

		if req.ItemID > 0 {
			items = preloadedItems
		} else {
			// limit 传 -1，GORM 不会拼接 LIMIT 子句，从而一次性取出全部未刮削条目
			items, err = db.GetUnscrappedItems(req.MediaType, -1)
			if err != nil {
				log.Errorf("获取未刮削条目失败: %v", err)
				return
			}
		}

		// 读取并发度（默认5），并启动 worker pool
		concurrency := setting.GetInt(conf.MediaScrapeConcurrency, defaultScrapeConcurrency)
		if concurrency <= 0 {
			concurrency = defaultScrapeConcurrency
		}
		if concurrency > len(items) && len(items) > 0 {
			concurrency = len(items)
		}

		var videoOrch *apply.Orchestrator
		if req.MediaType == model.MediaTypeVideo && len(items) > 0 {
			set, e := mediaScraperSet()
			if e != nil {
				log.Errorf("刮削失败：%v", e)
				return
			}
			var dl apply.Downloader
			if downloadCover {
				if dl, e = mediaDownloader(); e != nil {
					log.Errorf("刮削失败：%v", e)
					return
				}
			}
			videoOrch = &apply.Orchestrator{
				Lister: apply.LocalLister{}, Writer: apply.LocalWriter{Store: store.New(thumbnailMode, thumbnailPath)},
				Providers: set, Download: dl,
			}
		}

		var wg sync.WaitGroup
		jobs := make(chan int, len(items))
		// 落库串行化：所有 worker 把刮削成功的 item 推到 saveCh，由单协程负责落库，
		// 这样既能并发抓 API、又避免多 goroutine 同时写 sqlite/mysql 引发锁竞争。
		saveCh := make(chan *model.MediaItem, concurrency*2)
		var saveWg sync.WaitGroup

		// 单写入协程：刮一个写一个
		saveWg.Add(1)
		go func() {
			defer saveWg.Done()
			for it := range saveCh {
				if err := db.UpdateMediaItem(it); err != nil {
					log.Warnf("保存刮削结果失败 [id=%d, %s/%s]: %v", it.ID, it.FolderPath, it.FileName, err)
				}
			}
		}()

		worker := func() {
			defer wg.Done()
			for idx := range jobs {
				item := &items[idx]
				var scrapeErr error

				switch req.MediaType {
				case model.MediaTypeVideo:
					// 视频走 scraper.Set + apply.ScrapeOne：识别、搜索、取详情、落库都在里面，
					// 手动选定（source=manual）和锁定的条目不碰。
					if item.Source == model.MediaSourceManual || item.Locked {
						continue
					}
					began := time.Now()
					res, e := videoOrch.ScrapeOne(context.Background(), db.MediaItemPath(item), apply.ScrapeOpts{Existing: true})
					videoScrapeStats.record(res, time.Since(began))
					if e != nil && !errors.Is(e, apply.ErrLocked) && !errors.Is(e, apply.ErrGone) {
						log.Warnf("刮削失败 [%s] %s: %v", req.MediaType, item.FolderPath, e)
					}
					continue
				case model.MediaTypeMusic:
					// 音乐：刮削前先用文件本身的 ID3/Vorbis tag + .lrc 做本地兜底，
					// 仅填空字段；这样用户清空刮削后重新刮削也能恢复封面/歌词等。
					localCtx, localCancel := context.WithTimeout(context.Background(), 30*time.Second)
					media.FillMusicFromLocal(localCtx, item)
					localCancel()

					s := scraper.NewDiscogsScraper(discogsToken, discogsAPIURL)
					scrapeErr = s.ScrapeMusic(item)
				case model.MediaTypeBook:
					var bookErr error
					switch bookSource {
					case "google":
						bookErr = scraper.NewGoogleBooksScraper(
							googleBooksKey,
							thumbnailMode,
							thumbnailPath,
						).ScrapeBook(item)
					case "openlibrary":
						bookErr = scraper.NewOpenLibraryScraper(
							thumbnailMode,
							thumbnailPath,
						).ScrapeBook(item)
					default:
						bookErr = scraper.NewDoubanScraperWithConfig(
							thumbnailMode,
							thumbnailPath,
						).ScrapeBook(item)
					}
					if bookErr != nil {
						log.Debugf("书籍刮削失败 [%s/%s]: %v，将尝试本地提取封面", item.FolderPath, item.FileName, bookErr)
					}

					if item.Cover == "" {
						bookCtx, bookCancel := context.WithTimeout(context.Background(), 60*time.Second)
						bookFilePath := item.FolderPath + "/" + item.FileName
						bookReader := media.FetchFileReader(bookCtx, bookFilePath)
						if bookReader != nil {
							localScraper := scraper.NewBookLocalScraperWithConfig(
								thumbnailMode,
								thumbnailPath,
							)
							if localCover := localScraper.ExtractLocalCover(item.FileName, bookFilePath, bookReader); localCover != "" {
								item.Cover = localCover
							}
							_ = bookReader.Close()
						}
						bookCancel()
					}

					if bookErr != nil && item.Cover == "" {
						scrapeErr = bookErr
					}
				case model.MediaTypeImage:
					imgCtx, imgCancel := context.WithTimeout(context.Background(), 30*time.Second)
					imgFilePath := item.FolderPath + "/" + item.FileName
					imgReader := media.FetchFileReader(imgCtx, imgFilePath)
					s := scraper.NewImageScraperWithConfig(
						storeThumbnail,
						thumbnailMode,
						thumbnailPath,
					)
					scrapeErr = s.ScrapeImage(item, imgReader)
					if imgReader != nil {
						_ = imgReader.Close()
					}
					imgCancel()
				}

				// 远程封面按需下载到本地存储（或 base64），避免直接保存图片链接
				if downloadCover && strings.HasPrefix(item.Cover, "http") {
					rawPath := strings.TrimSuffix(item.FolderPath, "/") + "/" + item.FileName
					if cover := scraper.DownloadCoverToStorage(
						context.Background(),
						item.Cover,
						rawPath,
						thumbnailMode,
						thumbnailPath,
					); cover != "" {
						item.Cover = cover
					}
				}

				if scrapeErr != nil {
					log.Warnf("刮削失败 [%s] %s: %v", req.MediaType, item.FolderPath, scrapeErr)
					// 即使远端刮削失败，只要本地兜底已经写入了任意有效字段，
					// 也保存一次（不标记 ScrapedAt，便于下次还能重试），
					// 避免 cover/lyrics 等被丢弃。
					if hasAnyScrapedField(item) {
						saveCh <- item
					}
					continue
				}
				now := time.Now()
				item.ScrapedAt = &now
				saveCh <- item
			}
		}

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go worker()
		}

		for i := range items {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(saveCh)
		saveWg.Wait()
		if req.MediaType == model.MediaTypeVideo {
			folders := make([]string, 0, len(items))
			for i := range items {
				folders = append(folders, items[i].FolderPath)
			}
			media.ConsolidateVideoFolders(context.Background(), folders)
		}

		log.Infof("刮削完成 [%s]，共处理 %d 条（并发=%d）", req.MediaType, len(items), concurrency)
	}()

	_ = cfg
	common.SuccessResp(c)
}

// hasAnyScrapedField 判断 item 是否已经有任意一个有意义的刮削字段被填充
// 用于：远端刮削失败但本地兜底已经写入了内容时，仍然落库保留这些信息
func hasAnyScrapedField(item *model.MediaItem) bool {
	if item == nil {
		return false
	}
	return item.Cover != "" ||
		item.Lyrics != "" ||
		item.Description != "" ||
		item.Plot != "" ||
		item.Genre != "" ||
		item.Authors != "" ||
		item.AlbumArtist != "" ||
		item.ReleaseDate != "" ||
		item.Rating > 0 ||
		item.Publisher != "" ||
		item.ISBN != "" ||
		item.ExternalID != ""
}

// ==================== 公开API（前端媒体库浏览） ====================

// PublicListMedia 公开媒体列表（前端浏览用）
//
// 除上游的筛选外：path（相对用户 base_path，同 fs 路径）只列该路径下一层的条目，
// kind（逗号分隔的 movie/tv/anime/collection）按视频类型筛选。列表先按权限过滤再分页，
// total 是过滤后的数量。条目带评分、封面地址（cover_url）和合集的海报拼图（child_posters）。
func PublicListMedia(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "40"))
	page, pageSize = clampMediaPage(page, pageSize, 40, 200)
	orderBy := c.DefaultQuery("order_by", "name")
	orderDir := c.DefaultQuery("order_dir", "asc")
	folderPath := c.Query("folder_path")
	var kinds []string
	for _, k := range strings.Split(c.Query("kind"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			kinds = append(kinds, k)
		}
	}
	keyword := c.Query("keyword")
	typeTag := c.Query("type_tag")
	contentTag := c.Query("content_tag")
	scanPathIDStr := c.Query("scan_path_id")
	var scanPathID uint
	if scanPathIDStr != "" {
		if id, err := strconv.ParseUint(scanPathIDStr, 10, 64); err == nil {
			scanPathID = uint(id)
		}
	}

	hidden := false
	user := mediaUser(c)
	// The video library lists its top-level works; what sits inside a collection or
	// a show is listed under it (path=) or found by keyword. flat=1 lists every work,
	// and so does a kind filter: kind=movie must list the films inside collections too
	// (Jellyfin lists movies individually unless boxsets are grouped).
	topLevel := listsTopLevel(mediaType, folderPath, keyword, c.Query("path"), c.Query("flat"), kinds)
	if p := c.Query("path"); p != "" {
		if user == nil {
			common.SuccessResp(c, common.PageResp{Content: []MediaItemView{}, Total: 0})
			return
		}
		joined, err := user.JoinPath(p)
		if err != nil {
			// 越出 base_path：和没有权限一样，返回空而不是报错
			common.SuccessResp(c, common.PageResp{Content: []MediaItemView{}, Total: 0})
			return
		}
		folderPath = joined
	}
	q := db.MediaItemQuery{
		MediaType:  mediaType,
		ScanPathID: scanPathID,
		FolderPath: folderPath,
		Kinds:      kinds,
		TypeTag:    typeTag,
		ContentTag: contentTag,
		PathPrefix: userPathPrefix(user),
		Hidden:     &hidden,
		Keyword:    keyword,
		TopLevel:   topLevel,
		OrderBy:    orderBy,
		TitleSource: mediaTitleSource(),
		OrderDir:   orderDir,
		Page:       page,
		PageSize:   pageSize,
	}
	// 元信息(meta) 级别的读权限无法下推到 SQL，所以先过滤再分页
	items, total, err := db.ListMediaItemsFiltered(q, func(it *model.MediaItem) bool { return CanUserAccessItem(user, it) })
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	v, err := newMediaViewer(user, items)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, common.PageResp{Content: v.views(items), Total: total})
}

// PublicGetMedia 公开获取媒体详情：除条目本身外带评分、分集列表（episode_list）和封面地址；
// 候选（candidates）只给管理员。
func PublicGetMedia(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.ErrorStrResp(c, "无效的ID", 400)
		return
	}
	item, err := db.GetMediaItemByID(uint(id))
	if err != nil {
		common.ErrorResp(c, err, 404)
		return
	}
	respondMediaDetail(c, item)
}

// PublicGetMediaByPath 按路径获取媒体详情：path 相对用户 base_path（同 list?path=），
// 返回和 item/:id 相同的内容、受相同的权限约束；路径上没有条目或无权访问都是同一个 404。
func PublicGetMediaByPath(c *gin.Context) {
	user := mediaUser(c)
	var item *model.MediaItem
	if p := c.Query("path"); p != "" && user != nil {
		if joined, err := user.JoinPath(p); err == nil {
			if item, err = db.GetVideoItemByPath(joined); err != nil {
				common.ErrorResp(c, err, 500)
				return
			}
		}
	}
	if item == nil {
		common.ErrorStrResp(c, "资源不存在", 404)
		return
	}
	respondMediaDetail(c, item)
}

// respondMediaDetail 写出条目详情；隐藏的或用户无权访问的条目答 404。
func respondMediaDetail(c *gin.Context, item *model.MediaItem) {
	if item.Hidden || !CanUserAccessItem(mediaUser(c), item) {
		common.ErrorStrResp(c, "资源不存在", 404)
		return
	}
	v, err := newMediaViewer(mediaUser(c), []model.MediaItem{*item})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	out, err := v.detail(item)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, out)
}

// PublicListAlbums 公开专辑列表（音乐）
func PublicListAlbums(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "40"))
	page, pageSize = clampMediaPage(page, pageSize, 40, 200)
	keyword := c.Query("keyword")
	scanPathIDStr := c.Query("scan_path_id")
	var scanPathID uint
	if scanPathIDStr != "" {
		if id, err := strconv.ParseUint(scanPathIDStr, 10, 64); err == nil {
			scanPathID = uint(id)
		}
	}

	hidden := false
	q := db.MediaItemQuery{
		MediaType:  model.MediaTypeMusic,
		ScanPathID: scanPathID,
		PathPrefix: userPathPrefix(mediaUser(c)),
		Hidden:     &hidden,
		Keyword:    keyword,
		Page:       page,
		PageSize:   pageSize,
	}
	albums, total, err := db.ListAlbums(q)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, common.PageResp{Content: mediaAlbumViews(albums), Total: total})
}

// PublicGetAlbum 公开获取专辑详情及曲目
func PublicGetAlbum(c *gin.Context) {
	albumName := c.Query("album_name")
	albumArtist := c.Query("album_artist")
	if albumName == "" && albumArtist == "" {
		common.ErrorStrResp(c, "album_name 和 album_artist 不能同时为空", 400)
		return
	}
	tracks, err := db.GetAlbumTracks(albumName, albumArtist)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	user := mediaUser(c)
	filtered := make([]model.MediaItem, 0, len(tracks))
	for i := range tracks {
		if CanUserAccessItem(user, &tracks[i]) {
			filtered = append(filtered, tracks[i])
		}
	}
	v, err := newMediaViewer(user, filtered)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, v.views(filtered))
}

// PublicListFolders 公开文件夹列表（目录浏览模式）
func PublicListFolders(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	if mediaType == "" {
		common.ErrorStrResp(c, "media_type 不能为空", 400)
		return
	}
	paths, err := db.ListFolderPaths(mediaType)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	user := mediaUser(c)
	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		if CanUserAccessPath(user, p) {
			filtered = append(filtered, p)
		}
	}
	common.SuccessResp(c, filtered)
}

// PublicListScanPaths 公开获取扫描路径列表（前端筛选用）
func PublicListScanPaths(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	paths, err := db.ListMediaScanPaths(mediaType)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	user := mediaUser(c)
	filtered := make([]model.MediaScanPath, 0, len(paths))
	for _, p := range paths {
		if CanUserAccessPath(user, p.Path) {
			filtered = append(filtered, p)
		}
	}
	common.SuccessResp(c, filtered)
}

// ==================== 导入 / 导出 ====================

// MediaExportData 导出数据结构
// 包含版本号便于后续兼容性升级；导出粒度由 scope 字段说明：
//   - "all"        ：所有类型的全部数据
//   - "media_type" ：单个媒体类型（video/music/image/book）的全部数据
//   - "scan_path"  ：单个扫描路径下的数据（仅 1 个路径 + 该路径下的条目）
type MediaExportData struct {
	Version   int                   `json:"version"`
	ExportAt  time.Time             `json:"export_at"`
	Scope     string                `json:"scope"`
	MediaType model.MediaType       `json:"media_type,omitempty"`
	ScanPaths []model.MediaScanPath `json:"scan_paths"`
	Items     []model.MediaItem     `json:"items"`
}

const mediaExportVersion = 1

// ExportMediaDB 导出媒体数据
// query: media_type（可选，为空表示全部类型）, scan_path_id（可选，单路径导出）
// 返回 JSON 文件下载流
func ExportMediaDB(c *gin.Context) {
	mediaType := model.MediaType(c.Query("media_type"))
	scanPathIDStr := c.Query("scan_path_id")

	export := MediaExportData{
		Version:  mediaExportVersion,
		ExportAt: time.Now(),
	}

	if scanPathIDStr != "" {
		// 按扫描路径导出
		id, err := strconv.ParseUint(scanPathIDStr, 10, 64)
		if err != nil {
			common.ErrorStrResp(c, "无效的 scan_path_id", 400)
			return
		}
		sp, err := db.GetMediaScanPath(uint(id))
		if err != nil {
			common.ErrorResp(c, err, 404)
			return
		}
		items, err := db.ListMediaItemsByScanPath(uint(id))
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		export.Scope = "scan_path"
		export.MediaType = sp.MediaType
		export.ScanPaths = []model.MediaScanPath{*sp}
		export.Items = items
	} else if mediaType != "" {
		// 按媒体类型导出
		paths, err := db.ListMediaScanPaths(mediaType)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		items, err := db.ListAllMediaItems(mediaType)
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		export.Scope = "media_type"
		export.MediaType = mediaType
		export.ScanPaths = paths
		export.Items = items
	} else {
		// 全部导出
		paths, err := db.ListAllMediaScanPaths()
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		items, err := db.ListAllMediaItems("")
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		export.Scope = "all"
		export.ScanPaths = paths
		export.Items = items
	}

	// 直接以 JSON 流下载
	filename := buildExportFilename(export)
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(c.Writer).Encode(export); err != nil {
		log.Errorf("导出媒体数据失败: %v", err)
	}
}

// buildExportFilename 根据导出范围生成下载文件名
func buildExportFilename(d MediaExportData) string {
	ts := d.ExportAt.Format("20060102_150405")
	switch d.Scope {
	case "scan_path":
		spName := ""
		if len(d.ScanPaths) > 0 {
			spName = d.ScanPaths[0].Name
			if spName == "" {
				spName = d.ScanPaths[0].Path
			}
		}
		// 去掉路径中的斜杠等字符，避免文件名非法
		safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "?", "_", "*", "_", "\"", "_", "<", "_", ">", "_", "|", "_").Replace(spName)
		return "media_export_" + string(d.MediaType) + "_" + safe + "_" + ts + ".json"
	case "media_type":
		return "media_export_" + string(d.MediaType) + "_" + ts + ".json"
	default:
		return "media_export_all_" + ts + ".json"
	}
}

// ImportMediaDB 导入媒体数据
// 支持两种调用方式：
//  1. multipart/form-data：file 字段携带导出生成的 JSON 文件
//  2. application/json：直接 POST 整个 MediaExportData 结构
//
// 可选 query 参数 scan_path_id：当导入"scan_path"范围数据时，
// 把所有条目重定向到该扫描路径ID（覆盖文件中原有的 scan_path_id）。
func ImportMediaDB(c *gin.Context) {
	var data MediaExportData

	contentType := c.GetHeader("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		fileHeader, err := c.FormFile("file")
		if err != nil {
			common.ErrorStrResp(c, "缺少 file 文件参数", 400)
			return
		}
		f, err := fileHeader.Open()
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		defer f.Close()
		if err := json.NewDecoder(f).Decode(&data); err != nil {
			common.ErrorStrResp(c, "导入文件解析失败: "+err.Error(), 400)
			return
		}
	} else {
		if err := c.ShouldBindJSON(&data); err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
	}

	if data.Version <= 0 || data.Version > mediaExportVersion {
		common.ErrorStrResp(c, "不支持的导入文件版本", 400)
		return
	}

	// 1) 先导入扫描路径，建立 oldID -> newID 的映射
	idMap, spCreated, spUpdated, err := db.ImportMediaScanPaths(data.ScanPaths)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	// 2) 再导入条目，根据 idMap 把 scan_path_id 重定向到新ID
	// 修正每个条目的 scan_path_id：
	//   - 如果 query 指定了 scan_path_id，全部覆盖为该值（按扫描路径重定向导入）
	//   - 否则按 idMap 自动重定向；找不到映射的保留原值（通常发生在该路径未一并导入时）
	scanPathIDStr := c.Query("scan_path_id")
	var override *uint
	if scanPathIDStr != "" {
		if id, err := strconv.ParseUint(scanPathIDStr, 10, 64); err == nil {
			v := uint(id)
			override = &v
		}
	}
	if override == nil {
		for i := range data.Items {
			if newID, ok := idMap[data.Items[i].ScanPathID]; ok {
				data.Items[i].ScanPathID = newID
			}
		}
	}

	itemCreated, itemUpdated, err := db.ImportMediaItems(data.Items, override)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	common.SuccessResp(c, gin.H{
		"scan_paths_created": spCreated,
		"scan_paths_updated": spUpdated,
		"items_created":      itemCreated,
		"items_updated":      itemUpdated,
	})
}

// listsTopLevel reports whether a library listing shows only the top-level
// works: the unfiltered video view. A search, a folder, a flat request or a kind
// filter lists every matching work.
func listsTopLevel(mediaType model.MediaType, folderPath, keyword, path, flat string, kinds []string) bool {
	return mediaType == model.MediaTypeVideo && folderPath == "" && keyword == "" && path == "" && flat == "" && len(kinds) == 0
}

// mediaTitleSource is the media_title_source setting: "file" (default) or "scraped".
// mediaTitleSourceHook lets tests set the source without a settings table.
var mediaTitleSourceHook string

func mediaTitleSource() string {
	if mediaTitleSourceHook != "" {
		return mediaTitleSourceHook
	}
	if setting.GetStr(conf.MediaTitleSource, model.TitleSourceFile) == model.TitleSourceScraped {
		return model.TitleSourceScraped
	}
	return model.TitleSourceFile
}
