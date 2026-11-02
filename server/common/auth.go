package common

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/session"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
)

var SecretKey []byte

type UserClaims struct {
	Username string `json:"username"`
	PwdTS    int64  `json:"pwd_ts"`
	jwt.RegisteredClaims
}

// IssueSession starts a session for user and returns the JWT that carries its
// ID (jti). Every login path signs its token through here, so none of them can
// forget to register the device. The client may identify its device with a
// Client-Id header (or client_id query); a returning device replaces its old
// session. It fails with errs.TooManyDevices when max_devices refuses the login.
func IssueSession(c *gin.Context, user *model.User) (tokenString, sessionID string, err error) {
	clientID := c.GetHeader("Client-Id")
	if clientID == "" {
		clientID = c.Query("client_id")
	}
	if len(clientID) > 64 {
		clientID = clientID[:64]
	}
	sessionID, err = session.Create(user.ID, session.Meta{ClientID: clientID, UserAgent: c.Request.UserAgent(), IP: c.ClientIP()})
	if err != nil {
		return "", "", err
	}
	tokenString, err = signToken(user, sessionID)
	if err != nil {
		_, _ = session.Revoke(user.ID, sessionID)
		return "", "", err
	}
	return tokenString, sessionID, nil
}

// SessionErrCode is the HTTP status for an IssueSession failure.
func SessionErrCode(err error, def int) int {
	if errors.Is(err, errs.TooManyDevices) {
		return 403
	}
	return def
}

func signToken(user *model.User, sessionID string) (string, error) {
	claim := UserClaims{
		Username: user.Username,
		PwdTS:    user.PwdTS,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        sessionID,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(conf.Conf.TokenExpiresIn) * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		}}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claim)
	return token.SignedString(SecretKey)
}

func ParseToken(tokenString string) (*UserClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &UserClaims{}, func(token *jwt.Token) (interface{}, error) {
		return SecretKey, nil
	})
	if err != nil {
		if ve, ok := err.(*jwt.ValidationError); ok {
			if ve.Errors&jwt.ValidationErrorMalformed != 0 {
				return nil, errors.New("that's not even a token")
			} else if ve.Errors&jwt.ValidationErrorExpired != 0 {
				return nil, errors.New("token is expired")
			} else if ve.Errors&jwt.ValidationErrorNotValidYet != 0 {
				return nil, errors.New("token not active yet")
			} else {
				return nil, errors.New("couldn't handle this token")
			}
		}
	}
	if claims, ok := token.Claims.(*UserClaims); ok && token.Valid {
		return claims, nil
	}
	return nil, errors.New("couldn't handle this token")
}
