package _115_open

import "testing"

func TestIsPartAlreadyExistError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic error", errStr("some random error"), false},
		{"timeout", errStr("net/http: timeout awaiting response headers"), false},
		{"part already exist", errStr(`oss: service returned error: StatusCode=409, ErrorCode=PartAlreadyExist, ErrorMessage="For sequential multipart upload, you can't overwrite uploaded parts."`), true},
		{"partial match", errStr("PartAlreadyExist"), true},
		{"case sensitive", errStr("partalreadyexist"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPartAlreadyExistError(tt.err); got != tt.want {
				t.Errorf("isPartAlreadyExistError() = %v, want %v", got, tt.want)
			}
		})
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }
