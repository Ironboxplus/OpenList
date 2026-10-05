package rclonefs

import (
	"context"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/rclone/rclone/fs/hash"
)

// 115 lists SHA1 in upper case; rclone compares hex strings literally against
// its own lower-case output, so an unnormalised hash would make every file
// look changed and every run re-upload everything.
func TestHashIsLowerCasedAndBlankWhenUnknown(t *testing.T) {
	ctx := context.Background()
	o := &Object{hashes: utils.NewHashInfo(utils.SHA1, "A9993E364706816ABA3E25717850C26C9CD0D89D")}
	if got, _ := o.Hash(ctx, hash.SHA1); got != "a9993e364706816aba3e25717850c26c9cd0d89d" {
		t.Fatalf("SHA1 = %q, want lower case", got)
	}
	if got, err := o.Hash(ctx, hash.MD5); err != nil || got != "" {
		t.Fatalf("missing MD5 = %q, %v; want blank and no error so rclone falls back to size", got, err)
	}
	if _, err := o.Hash(ctx, hash.CRC32); err != hash.ErrUnsupported {
		t.Fatalf("unsupported hash err = %v", err)
	}
}
