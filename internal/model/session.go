package model

// Session is a login of a user on one device: created when a JWT is issued,
// carried in the token as its jti. Protocol access (WebDAV, S3, FTP, ...) and
// guests have none.
type Session struct {
	SessionID  string `json:"session_id" gorm:"primaryKey;size:64"`
	UserID     uint   `json:"user_id" gorm:"index"`
	ClientID   string `json:"client_id" gorm:"size:64;index"` // device id the client reported (Client-Id), may be empty
	UserAgent  string `json:"user_agent" gorm:"size:255"`
	IP         string `json:"ip" gorm:"size:64"` // masked
	CreatedAt  int64  `json:"created_at"`
	LastActive int64  `json:"last_active"`
	Status     int    `json:"status"`
}

const (
	SessionActive = iota
	SessionInactive
)
