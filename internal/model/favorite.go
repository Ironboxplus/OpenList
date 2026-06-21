package model

type Favorite struct {
	ID        uint   `json:"id" gorm:"primaryKey"`
	UserID    uint   `json:"user_id" gorm:"index"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	Tag       string `json:"tag"` // optional free-text label
	CreatedAt int64  `json:"created_at"`
}
