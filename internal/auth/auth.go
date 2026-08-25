package auth

type Role string

const (
	RoleNone     Role = "none"
	RoleEmployee Role = "employee"
	RoleAdmin    Role = "admin"
)

type Authorizer struct {
	allowedGroups map[string]struct{}
	employees     map[string]struct{}
	admins        map[string]struct{}
}

func New(allowedGroupIDs, employeeIDs, adminIDs []string) Authorizer {
	return Authorizer{
		allowedGroups: idSet(allowedGroupIDs),
		employees:     idSet(employeeIDs),
		admins:        idSet(adminIDs),
	}
}

func (a Authorizer) AllowedGroup(groupID string) bool {
	_, ok := a.allowedGroups[groupID]
	return ok
}

func (a Authorizer) Role(userID string) Role {
	if userID == "" {
		return RoleNone
	}
	if _, ok := a.admins[userID]; ok {
		return RoleAdmin
	}
	if _, ok := a.employees[userID]; ok {
		return RoleEmployee
	}
	return RoleNone
}

func (a Authorizer) CanOperate(userID, creatorID string) bool {
	switch a.Role(userID) {
	case RoleAdmin:
		return true
	case RoleEmployee:
		return userID == creatorID
	default:
		return false
	}
}

func (a Authorizer) CanApprove(userID string) bool {
	return a.Role(userID) == RoleAdmin
}

func idSet(ids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		set[id] = struct{}{}
	}
	return set
}
