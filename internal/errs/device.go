package errs

import "errors"

var (
	TooManyDevices  = errors.New("too many active devices")
	SessionInactive = errors.New("session inactive")

	// Share access (sharing_access.go in op).
	TooManyShareAttempts  = errors.New("too many wrong share passwords, try again later")
	ShareActionNotAllowed = errors.New("this share does not allow that")
)
