package data

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestCarryRenamedSetting(t *testing.T) {
	tmdbItem := func() *model.SettingItem {
		return &model.SettingItem{Key: conf.MediaTMDBKey, Type: conf.TypeString}
	}
	langItem := func() *model.SettingItem {
		return &model.SettingItem{Key: conf.MediaScrapeLanguage, Value: "zh-CN", Type: conf.TypeSelect, Options: "zh-CN,en-US,ja-JP"}
	}
	old := func(k, v string) map[string]*model.SettingItem {
		return map[string]*model.SettingItem{k: {Key: k, Value: v, Flag: model.DEPRECATED}}
	}

	t.Run("token copied when the new key has no row", func(t *testing.T) {
		it := tmdbItem()
		carryRenamedSetting(it, nil, old("media_tmdb_token", "secret"))
		if it.Value != "secret" {
			t.Fatalf("value = %q", it.Value)
		}
	})
	t.Run("token copied when the new row is empty", func(t *testing.T) {
		it := tmdbItem()
		carryRenamedSetting(it, &model.SettingItem{Key: conf.MediaTMDBKey}, old("media_tmdb_token", "secret"))
		if it.Value != "secret" {
			t.Fatalf("value = %q", it.Value)
		}
	})
	t.Run("token idempotent and never overrides", func(t *testing.T) {
		it := tmdbItem()
		it.Value = "chosen"
		carryRenamedSetting(it, &model.SettingItem{Key: conf.MediaTMDBKey, Value: "chosen"}, old("media_tmdb_token", "secret"))
		if it.Value != "chosen" {
			t.Fatalf("value = %q", it.Value)
		}
	})
	t.Run("empty old value changes nothing", func(t *testing.T) {
		it := tmdbItem()
		carryRenamedSetting(it, nil, old("media_tmdb_token", ""))
		if it.Value != "" {
			t.Fatalf("value = %q", it.Value)
		}
		carryRenamedSetting(it, nil, map[string]*model.SettingItem{})
		if it.Value != "" {
			t.Fatalf("value = %q", it.Value)
		}
	})
	t.Run("language copied only when the new key is created", func(t *testing.T) {
		it := langItem()
		carryRenamedSetting(it, nil, old("media_language", "ja-JP"))
		if it.Value != "ja-JP" {
			t.Fatalf("value = %q", it.Value)
		}
		it = langItem()
		it.Value = "en-US"
		carryRenamedSetting(it, &model.SettingItem{Key: conf.MediaScrapeLanguage, Value: "zh-CN"}, old("media_language", "ja-JP"))
		if it.Value != "en-US" {
			t.Fatalf("existing row was overridden: %q", it.Value)
		}
	})
	t.Run("language outside the options is not copied", func(t *testing.T) {
		it := langItem()
		carryRenamedSetting(it, nil, old("media_language", "xx-XX"))
		if it.Value != "zh-CN" {
			t.Fatalf("value = %q", it.Value)
		}
	})
	t.Run("other keys untouched", func(t *testing.T) {
		it := &model.SettingItem{Key: "site_title", Value: "x"}
		carryRenamedSetting(it, nil, old("media_tmdb_token", "secret"))
		if it.Value != "x" {
			t.Fatalf("value = %q", it.Value)
		}
	})
}
