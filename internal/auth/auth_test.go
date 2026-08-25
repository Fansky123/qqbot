package auth

import "testing"

func TestAuthorizer(t *testing.T) {
	t.Parallel()

	a := New([]string{"group-1"}, []string{"employee", "admin"}, []string{"admin"})

	if !a.AllowedGroup("group-1") {
		t.Error("AllowedGroup(group-1) = false, want true")
	}
	if a.AllowedGroup("group-2") {
		t.Error("AllowedGroup(group-2) = true, want false")
	}
	if got := a.Role("employee"); got != RoleEmployee {
		t.Errorf("Role(employee) = %q, want %q", got, RoleEmployee)
	}
	if got := a.Role("admin"); got != RoleAdmin {
		t.Errorf("Role(admin) = %q, want %q", got, RoleAdmin)
	}
	if got := a.Role("unknown"); got != RoleNone {
		t.Errorf("Role(unknown) = %q, want %q", got, RoleNone)
	}
	if got := a.Role(""); got != RoleNone {
		t.Errorf("Role(empty) = %q, want %q", got, RoleNone)
	}
	if !a.CanOperate("employee", "employee") {
		t.Error("CanOperate(employee, employee) = false, want true")
	}
	if a.CanOperate("employee", "other") {
		t.Error("CanOperate(employee, other) = true, want false")
	}
	if !a.CanOperate("admin", "other") {
		t.Error("CanOperate(admin, other) = false, want true")
	}
	if a.CanOperate("unknown", "unknown") || a.CanOperate("", "") {
		t.Error("CanOperate() allowed an unknown user")
	}
	if a.CanApprove("employee") || !a.CanApprove("admin") || a.CanApprove("unknown") {
		t.Error("CanApprove() returned unexpected authorization")
	}
}

func TestNewCopiesInputSlices(t *testing.T) {
	t.Parallel()

	groups := []string{"group-1"}
	employees := []string{"employee"}
	admins := []string{"admin"}
	a := New(groups, employees, admins)

	groups[0] = "changed-group"
	employees[0] = "changed-employee"
	admins[0] = "changed-admin"

	if !a.AllowedGroup("group-1") || a.AllowedGroup("changed-group") {
		t.Error("AllowedGroup() changed after caller mutation")
	}
	if a.Role("employee") != RoleEmployee || a.Role("changed-employee") != RoleNone {
		t.Error("employee role changed after caller mutation")
	}
	if a.Role("admin") != RoleAdmin || a.Role("changed-admin") != RoleNone {
		t.Error("admin role changed after caller mutation")
	}
}
