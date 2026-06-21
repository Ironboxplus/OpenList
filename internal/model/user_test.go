package model

import "testing"

func TestCanManageUserInfoBit(t *testing.T) {
	// Bit 16 is the delegated "manage user info" permission.
	const bit int32 = 1 << 16

	if CanManageUserInfo(0) {
		t.Fatal("no permission bits should mean no delegated user management")
	}
	if !CanManageUserInfo(bit) {
		t.Fatal("bit 16 set should grant delegated user management")
	}
	// Lower bits must not be mistaken for bit 16.
	if CanManageUserInfo(0xFFFF) {
		t.Fatal("bits 0-15 must not imply user management")
	}
}

func TestUserCanManageUserInfoAdminAlways(t *testing.T) {
	admin := &User{Role: ADMIN, Permission: 0}
	if !admin.CanManageUserInfo() {
		t.Fatal("admin must always be able to manage user info")
	}
	normal := &User{Role: GENERAL, Permission: 0}
	if normal.CanManageUserInfo() {
		t.Fatal("a normal user without the bit must not manage user info")
	}
	delegated := &User{Role: GENERAL, Permission: 1 << 16}
	if !delegated.CanManageUserInfo() {
		t.Fatal("a normal user with bit 16 must be able to manage user info")
	}
}
