package main

import (
	"testing"

	"github.com/glenjbarber/apiary/internal/manager"
)

func TestParseRoleMap_Empty(t *testing.T) {
	roleMap, err := parseRoleMap("")
	if err != nil {
		t.Fatalf("parseRoleMap() error: %v", err)
	}
	if len(roleMap) != 0 {
		t.Errorf("parseRoleMap(\"\") = %v, want empty", roleMap)
	}
}

func TestParseRoleMap_MultipleRolesAndUsers(t *testing.T) {
	roleMap, err := parseRoleMap("admin:alice;operator:bob,carol;viewer:dave")
	if err != nil {
		t.Fatalf("parseRoleMap() error: %v", err)
	}
	want := map[string]manager.Role{
		"alice": manager.RoleAdmin,
		"bob":   manager.RoleOperator,
		"carol": manager.RoleOperator,
		"dave":  manager.RoleViewer,
	}
	if len(roleMap) != len(want) {
		t.Fatalf("parseRoleMap() = %v, want %v", roleMap, want)
	}
	for user, role := range want {
		if roleMap[user] != role {
			t.Errorf("parseRoleMap()[%q] = %q, want %q", user, roleMap[user], role)
		}
	}
}

func TestParseRoleMap_InvalidRoleRejected(t *testing.T) {
	if _, err := parseRoleMap("superuser:alice"); err == nil {
		t.Error("parseRoleMap() error = nil, want a rejection for an unrecognized role")
	}
}

func TestParseRoleMap_MalformedEntryRejected(t *testing.T) {
	if _, err := parseRoleMap("admin-alice"); err == nil {
		t.Error("parseRoleMap() error = nil, want a rejection for a missing colon")
	}
}

func TestParseRoleMap_DuplicateUserAcrossRolesRejected(t *testing.T) {
	if _, err := parseRoleMap("admin:alice;operator:alice"); err == nil {
		t.Error("parseRoleMap() error = nil, want a rejection for the same user listed under two roles")
	}
}

func TestParseRoleMap_WhitespaceIsTrimmed(t *testing.T) {
	roleMap, err := parseRoleMap(" admin : alice , bob ; viewer : carol ")
	if err != nil {
		t.Fatalf("parseRoleMap() error: %v", err)
	}
	if roleMap["alice"] != manager.RoleAdmin || roleMap["bob"] != manager.RoleAdmin || roleMap["carol"] != manager.RoleViewer {
		t.Errorf("parseRoleMap() = %v, want alice/bob=admin carol=viewer", roleMap)
	}
}
