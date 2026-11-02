package db

import (
	"gorm.io/gorm"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/pkg/errors"
)

// Sessions are looked up by their ID; "active since" arguments are unix
// seconds and exclude sessions idle since before that moment (0 = no limit).

func CreateSession(s *model.Session) error {
	return errors.WithStack(db.Create(s).Error)
}

func GetSession(sessionID string) (*model.Session, error) {
	var s model.Session
	if err := db.Where("session_id = ?", sessionID).First(&s).Error; err != nil {
		return nil, errors.Wrap(err, "failed find session")
	}
	return &s, nil
}

// UpdateSessionActivity records that the session was seen. ua and ip are
// written along because they are what the device list shows.
func UpdateSessionActivity(sessionID string, lastActive int64, ua, ip string) error {
	return errors.WithStack(db.Model(&model.Session{}).Where("session_id = ?", sessionID).
		Updates(map[string]any{"last_active": lastActive, "user_agent": ua, "ip": ip}).Error)
}

func activeSessions(userID uint, since int64) *gorm.DB {
	q := db.Model(&model.Session{}).Where("status = ?", model.SessionActive)
	if userID != 0 {
		q = q.Where("user_id = ?", userID)
	}
	if since > 0 {
		q = q.Where("last_active >= ?", since)
	}
	return q
}

func CountActiveSessionsByUser(userID uint, since int64) (int64, error) {
	var n int64
	err := activeSessions(userID, since).Count(&n).Error
	return n, errors.WithStack(err)
}

func GetOldestActiveSession(userID uint, since int64) (*model.Session, error) {
	var s model.Session
	if err := activeSessions(userID, since).Order("last_active ASC").First(&s).Error; err != nil {
		return nil, errors.Wrap(err, "failed get oldest active session")
	}
	return &s, nil
}

// ListSessions lists active sessions of userID, or of everyone when userID is 0.
func ListSessions(userID uint, since int64) ([]model.Session, error) {
	var out []model.Session
	err := activeSessions(userID, since).Order("last_active DESC").Find(&out).Error
	return out, errors.WithStack(err)
}

// MarkSessionInactive revokes one session. userID 0 means any owner.
func MarkSessionInactive(userID uint, sessionID string) (int64, error) {
	q := db.Model(&model.Session{}).Where("session_id = ? AND status = ?", sessionID, model.SessionActive)
	if userID != 0 {
		q = q.Where("user_id = ?", userID)
	}
	res := q.Update("status", model.SessionInactive)
	return res.RowsAffected, errors.WithStack(res.Error)
}

// MarkClientSessionsInactive revokes the active sessions the user already has
// on the device identified by clientID, and returns their IDs.
func MarkClientSessionsInactive(userID uint, clientID string) ([]string, error) {
	var ids []string
	if err := db.Model(&model.Session{}).
		Where("user_id = ? AND client_id = ? AND status = ?", userID, clientID, model.SessionActive).
		Pluck("session_id", &ids).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	err := db.Model(&model.Session{}).Where("session_id IN ?", ids).Update("status", model.SessionInactive).Error
	return ids, errors.WithStack(err)
}

// DeleteSessionsIdleBefore removes sessions not seen since ts, and revoked ones
// not seen since ts too (a revoked row is kept for a while so its token keeps
// answering 401 "revoked" rather than "unknown").
func DeleteSessionsIdleBefore(ts int64) (int64, error) {
	res := db.Where("last_active < ?", ts).Delete(&model.Session{})
	return res.RowsAffected, errors.WithStack(res.Error)
}
