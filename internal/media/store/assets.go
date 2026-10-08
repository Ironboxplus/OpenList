package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp" // decode webp posters
)

// ThumbWidth is the width of the generated thumbnail.
const ThumbWidth = 342

const defaultMaxAsset = 10 << 20

var mimeExt = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/gif":  "gif",
	"image/webp": "webp",
	"image/bmp":  "bmp",
	"image/tiff": "tiff",
}

var formatMime = map[string]string{
	"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif",
	"webp": "image/webp", "bmp": "image/bmp", "tiff": "image/tiff",
}

// AssetFile is where an asset's original (thumb=false) or thumbnail lives.
func (s *Store) AssetFile(a *model.MediaAsset, thumb bool) string {
	dir := filepath.Join(s.AssetDir, a.SHA256[:2])
	if thumb {
		return filepath.Join(dir, a.SHA256+".thumb.jpg")
	}
	ext := mimeExt[a.Mime]
	if ext == "" {
		ext = "img"
	}
	return filepath.Join(dir, a.SHA256+"."+ext)
}

// AddAsset stores an image, deduplicated by its SHA-256. The bytes go to disk
// (original plus a 342px thumbnail), never into the database. srcURL is only
// recorded.
func (s *Store) AddAsset(data []byte, srcURL string) (*model.MediaAsset, error) {
	limit := s.MaxAssetBytes
	if limit <= 0 {
		limit = defaultMaxAsset
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("image is larger than %d bytes", limit)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	if a, err := db.GetMediaAssetBySHA(sha); err != nil || a != nil {
		return a, err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not a supported image: %w", err)
	}
	mime := formatMime[format]
	if mime == "" {
		return nil, fmt.Errorf("unsupported image format %q", format)
	}
	a := &model.MediaAsset{SHA256: sha, Mime: mime, Width: cfg.Width, Height: cfg.Height, SourceURL: srcURL}
	if err := writeFileAtomic(s.AssetFile(a, false), data); err != nil {
		return nil, err
	}
	thumb, err := makeThumb(data)
	if err != nil {
		return nil, fmt.Errorf("thumbnail: %w", err)
	}
	if err := writeFileAtomic(s.AssetFile(a, true), thumb); err != nil {
		return nil, err
	}
	if err := db.CreateMediaAsset(a); err != nil {
		// A concurrent upload of the same bytes won the unique index.
		if existing, e2 := db.GetMediaAssetBySHA(sha); e2 == nil && existing != nil {
			return existing, nil
		}
		return nil, err
	}
	return a, nil
}

func makeThumb(data []byte) ([]byte, error) {
	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return nil, err
	}
	if img.Bounds().Dx() > ThumbWidth {
		img = imaging.Resize(img, ThumbWidth, 0, imaging.Lanczos)
	}
	// JPEG has no alpha: flatten onto white so transparent PNGs aren't black.
	flat := image.NewNRGBA(img.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, flat, imaging.JPEG, imaging.JPEGQuality(82)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
