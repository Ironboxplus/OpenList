package model

import (
	"crypto/subtle"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils/random"
)

type SharingDB struct {
	ID          string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	FilesRaw    string     `json:"-" gorm:"type:text"`
	Expires     *time.Time `json:"expires"`
	// Pwd is the password as the creator typed it; phase 1 of the hash migration
	// keeps it so the UI can still show it. Verification uses PasswordHash.
	Pwd          string `json:"pwd"`
	PasswordHash string `json:"-" gorm:"size:64"`
	PasswordSalt string `json:"-" gorm:"size:32"`
	Accessed     int    `json:"accessed"`
	// MaxAccessed is the number of distinct visitors (share, IP, 30 min window)
	// that may download; 0 = unlimited. BurnAfterRead means 1 when MaxAccessed is 0.
	MaxAccessed   int        `json:"max_accessed"`
	BurnAfterRead bool       `json:"burn_after_read"`
	ConsumedAt    *time.Time `json:"consumed_at"` // when the limit was first reached
	// DenyPreview / DenyDownload switch the two uses of the file content off. They
	// are stored negated so that rows and code that never heard of them allow both.
	// The preview/download split is a policy for the UI and honest clients: the
	// client says which it is (preview=1 on the link), the bytes are the same.
	DenyPreview   bool       `json:"-"`
	DenyDownload  bool       `json:"-"`
	ViewCount     int64      `json:"view_count"`
	DownloadCount int64      `json:"download_count"`
	LastAccessAt  *time.Time `json:"last_access_at"`
	CreatorId     uint       `json:"-"`
	Disabled      bool       `json:"disabled"`
	Remark        string     `json:"remark"`
	Readme        string     `json:"readme" gorm:"type:text"`
	Header        string     `json:"header" gorm:"type:text"`
	Sort
}

func (s *SharingDB) AllowPreview() bool  { return !s.DenyPreview }
func (s *SharingDB) AllowDownload() bool { return !s.DenyDownload }

// HasPassword reports whether visitors must present a password.
func (s *SharingDB) HasPassword() bool { return s.PasswordHash != "" || s.Pwd != "" }

// SharingPasswordHash is the stored form of a share password: the same salted
// two-step hash as user passwords.
func SharingPasswordHash(pwd, salt string) string { return TwoHashPwd(pwd, salt) }

// SetPassword sets (or, for "", clears) the password: plaintext for display,
// salted hash for verification. A fresh salt means tokens issued for the old
// password stop working.
func (s *SharingDB) SetPassword(pwd string) {
	s.Pwd = pwd
	if pwd == "" {
		s.PasswordHash, s.PasswordSalt = "", ""
		return
	}
	s.PasswordSalt = random.String(16)
	s.PasswordHash = SharingPasswordHash(pwd, s.PasswordSalt)
}

// VerifyPassword checks a presented password in constant time. The hash decides
// when there is one; a row without a hash (written by an older binary) falls
// back to the plaintext, and the next start converts it.
func (s *SharingDB) VerifyPassword(pwd string) bool {
	if s.PasswordHash != "" {
		return subtle.ConstantTimeCompare([]byte(SharingPasswordHash(pwd, s.PasswordSalt)), []byte(s.PasswordHash)) == 1
	}
	return s.Pwd == "" || subtle.ConstantTimeCompare([]byte(s.Pwd), []byte(pwd)) == 1
}

// EffectiveMaxAccessed is the download-slot limit in force (0 = none).
func (s *SharingDB) EffectiveMaxAccessed() int {
	if s.MaxAccessed > 0 {
		return s.MaxAccessed
	}
	if s.BurnAfterRead {
		return 1
	}
	return 0
}

type Sharing struct {
	*SharingDB
	Files   []string `json:"files"`
	Creator *User    `json:"-"`
}

func (s *Sharing) Valid() bool {
	return s.Usable() && !s.LimitReached()
}

func (s *SharingDB) LimitReached() bool {
	limit := s.EffectiveMaxAccessed()
	return limit > 0 && s.Accessed >= limit
}

func (s *Sharing) Usable() bool {
	if s.Disabled {
		return false
	}
	if len(s.Files) == 0 {
		return false
	}
	if s.Creator == nil || !s.Creator.CanShare() {
		return false
	}
	if s.Expires != nil && !s.Expires.IsZero() && s.Expires.Before(time.Now()) {
		return false
	}
	return true
}

func (s *Sharing) Verify(pwd string) bool {
	return s.VerifyPassword(pwd)
}
