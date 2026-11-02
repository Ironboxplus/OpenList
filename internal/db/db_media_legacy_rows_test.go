package db

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// The production binary (e81e43f9) left x_media_items with one row per curated
// directory and no media_type column. Starting the new code on it must keep
// those rows: SQLite refuses to add a NOT NULL column without a default to a
// non-empty table, which used to end startup.
func TestInitKeepsLegacyMediaRows(t *testing.T) {
	t.Setenv(MediaMigrateWipeEnv, "")
	d, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy.db")), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: "x_"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // Windows cannot remove an open database file
		if sqlDB, err := d.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	conf.Conf = conf.DefaultConfig(t.TempDir())

	// the e81e43f9 model, as gorm created it
	for _, q := range []string{
		"CREATE TABLE x_media_items (id integer primary key autoincrement, path varchar(700), kind text, title text, original_title text, year integer, external_ids text, ratings text, genres text, overview text, poster_id integer, backdrop_id integer, source text, confidence real, locked numeric, candidates text, scraped_at datetime, updated_at datetime, created_at datetime)",
		"CREATE UNIQUE INDEX idx_x_media_items_path ON x_media_items(path)",
		"INSERT INTO x_media_items (path, kind, title, year, source, locked) VALUES ('/115/MOV/A', 'movie', 'A', 2001, 'manual', 1)",
		"INSERT INTO x_media_items (path, kind, title, year, source, locked) VALUES ('/115/MOV/B', 'tv', 'B', 2002, 'scraper', 0)",
		"INSERT INTO x_media_items (path, kind, title, year, source, locked) VALUES ('/115/MOV/C', 'anime', 'C', 0, 'rule', 0)",
	} {
		if err := d.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}

	Init(d) // log.Fatalf ends the test binary if the migration fails

	var rows []struct {
		Path, Kind, Title, MediaType string
		Locked                       bool
	}
	if err := d.Raw("SELECT path, kind, title, media_type, locked FROM x_media_items ORDER BY id").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("legacy rows after start: %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.MediaType != string(model.MediaTypeVideo) {
			t.Errorf("%s: media_type = %q, want video", r.Path, r.MediaType)
		}
	}
	if rows[0].Title != "A" || !rows[0].Locked || rows[0].Kind != "movie" {
		t.Errorf("legacy row changed: %+v", rows[0])
	}

	// the new media code writes next to them, and a second start changes nothing
	if err := d.Create(&model.MediaItem{MediaType: model.MediaTypeMusic, FolderPath: "/m", FileName: "a.mp3"}).Error; err != nil {
		t.Fatalf("insert into migrated media_items: %v", err)
	}
	Init(d)
	var n int64
	d.Model(&model.MediaItem{}).Count(&n)
	if n != 4 {
		t.Fatalf("rows after second start: %d, want 4", n)
	}
}
