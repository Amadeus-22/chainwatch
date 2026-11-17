package domain

import (
	"fmt"
	"strings"
	"time"
)

// OrgID identifies an organization (a tenant). Every watched wallet and alert
// belongs to exactly one organization.
type OrgID int64

// Role is what a member may do in an organization.
type Role string

const (
	// RoleOwner manages members and everything a member can do.
	RoleOwner Role = "owner"
	// RoleMember reads the organization's data and manages its watch list.
	RoleMember Role = "member"
)

// ParseRole accepts "owner" or "member".
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleOwner, RoleMember:
		return r, nil
	}
	return "", fmt.Errorf("%w: role must be %q or %q, got %q", ErrInvalidInput, RoleOwner, RoleMember, s)
}

// Organization is a tenant: a treasury team, a fund, a DAO.
type Organization struct {
	ID        OrgID
	Name      string
	CreatedAt time.Time
}

// MaxOrgNameLen bounds organization names (bytes).
const MaxOrgNameLen = 100

// ValidateOrgName trims and checks an organization name.
func ValidateOrgName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxOrgNameLen {
		return "", fmt.Errorf("%w: organization name must be 1 to %d bytes", ErrInvalidInput, MaxOrgNameLen)
	}
	return name, nil
}

// Membership is an account's role in one organization, with the organization.
type Membership struct {
	Org  Organization
	Role Role
}

// Member is one account of an organization.
type Member struct {
	Address   Address
	Role      Role
	CreatedAt time.Time
}

// Principal is the authenticated caller: the Ethereum account that signed in.
type Principal struct {
	Address Address
}

// Session is a signed-in browser session. Only the SHA-256 of the bearer token is
// stored, so a database leak does not leak usable sessions.
type Session struct {
	TokenHash [32]byte
	Address   Address
	ExpiresAt time.Time
}
