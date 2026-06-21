package handles

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestApplyUserUpdatePolicyRejectsRoleChange(t *testing.T) {
	existing := &model.User{ID: 2, Role: model.GENERAL, Username: "bob"}
	req := &model.User{ID: 2, Role: model.ADMIN, Username: "bob"}
	if err := applyUserUpdatePolicy(existing, req, true); err == nil {
		t.Fatal("expected role change to be rejected even for admin")
	}
}

func TestApplyUserUpdatePolicyAdminCanEditPrivilegedFields(t *testing.T) {
	existing := &model.User{ID: 2, Role: model.GENERAL, Username: "bob", Permission: 0, BasePath: "/old"}
	req := &model.User{ID: 2, Role: model.GENERAL, Username: "bob2", Permission: 0xFF, BasePath: "/new", Disabled: true}
	if err := applyUserUpdatePolicy(existing, req, true); err != nil {
		t.Fatalf("admin edit should succeed: %v", err)
	}
	if req.Permission != 0xFF || req.BasePath != "/new" || !req.Disabled {
		t.Fatalf("admin's privileged changes must be preserved, got %+v", req)
	}
}

func TestApplyUserUpdatePolicyDelegatedCannotEditAdmin(t *testing.T) {
	existing := &model.User{ID: 1, Role: model.ADMIN, Username: "root"}
	req := &model.User{ID: 1, Role: model.ADMIN, Username: "root2"}
	if err := applyUserUpdatePolicy(existing, req, false); err == nil {
		t.Fatal("a delegated (non-admin) editor must not edit an admin user")
	}
}

func TestApplyUserUpdatePolicyDelegatedProfileOnly(t *testing.T) {
	existing := &model.User{
		ID:         2,
		Role:       model.GENERAL,
		Username:   "bob",
		Permission: 0b101,
		BasePath:   "/restricted",
		Disabled:   false,
		OtpSecret:  "secret",
	}
	// A delegated editor tries to escalate permission, change base path, disable.
	req := &model.User{
		ID:         2,
		Role:       model.GENERAL,
		Username:   "bobby",
		Password:   "newpass",
		Permission: 0xFFFF,
		BasePath:   "/",
		Disabled:   true,
		OtpSecret:  "",
	}
	if err := applyUserUpdatePolicy(existing, req, false); err != nil {
		t.Fatalf("delegated profile edit should succeed: %v", err)
	}
	// Username + password allowed through.
	if req.Username != "bobby" {
		t.Errorf("username should be editable, got %q", req.Username)
	}
	if req.Password != "newpass" {
		t.Errorf("password should be editable, got %q", req.Password)
	}
	// Every privileged field must be forced back to the existing value.
	if req.Permission != 0b101 {
		t.Errorf("permission must not change, got %d", req.Permission)
	}
	if req.BasePath != "/restricted" {
		t.Errorf("base path must not change, got %q", req.BasePath)
	}
	if req.Disabled {
		t.Errorf("disabled must not change")
	}
	if req.OtpSecret != "secret" {
		t.Errorf("otp secret must be preserved, got %q", req.OtpSecret)
	}
}
