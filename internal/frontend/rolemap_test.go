package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/loginconfig"
	"github.com/glenjbarber/apiary/internal/manager"
)

// fakeRoleMapStore records Save calls without touching any real file,
// mirroring fakePasswordSetter's own reasoning above.
type fakeRoleMapStore struct {
	lastSave loginconfig.Config
	saves    int
	err      error
}

func (f *fakeRoleMapStore) Save(cfg loginconfig.Config) error {
	if f.err != nil {
		return f.err
	}
	f.lastSave = cfg
	f.saves++
	return nil
}

func TestServer_AddUser_AdminCanGrantANewRole(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin}
	store := &fakeRoleMapStore{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	s.SetRoleMapStore(store)
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("username=carol&role=viewer"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "carol") || !strings.Contains(rec.Body.String(), "added with role viewer") {
		t.Errorf("expected carol added as viewer, got: %s", rec.Body.String())
	}
	if store.saves != 1 || store.lastSave.RoleMap["carol"] != "viewer" || store.lastSave.RoleMap["admin"] != "admin" {
		t.Errorf("persisted role map = %+v (saves=%d), want carol:viewer alongside the existing admin", store.lastSave.RoleMap, store.saves)
	}
}

func TestServer_AddUser_RejectsInvalidRole(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("username=carol&role=superuser"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "invalid role") {
		t.Errorf("expected an invalid-role rejection, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "carol") {
		t.Errorf("carol should not appear anywhere after a rejected add, got: %s", rec.Body.String())
	}
}

func TestServer_AddUser_RejectsDuplicateUsername(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "carol": manager.RoleViewer}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("username=carol&role=operator"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "already has a role assigned") {
		t.Errorf("expected a duplicate-username rejection, got: %s", rec.Body.String())
	}
}

func TestServer_AddUser_RejectsEmptyUsername(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("username=+&role=viewer"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "must not be empty") {
		t.Errorf("expected an empty-username rejection, got: %s", rec.Body.String())
	}
}

func TestServer_AddUser_ForbiddenForNonAdmin(t *testing.T) {
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "ops", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("username=carol&role=viewer"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (Operator must not be able to add a role-map entry)", rec.Code, http.StatusForbidden)
	}
}

func TestServer_SetUserRole_ChangesExistingAccount(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "bob": manager.RoleViewer}
	store := &fakeRoleMapStore{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	s.SetRoleMapStore(store)
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodPost, "/users/bob/role", strings.NewReader("role=operator"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "role for") || !strings.Contains(rec.Body.String(), "set to operator") {
		t.Errorf("expected a role-updated confirmation, got: %s", rec.Body.String())
	}
	if store.lastSave.RoleMap["bob"] != "operator" {
		t.Errorf("persisted role map = %+v, want bob:operator", store.lastSave.RoleMap)
	}
}

func TestServer_RemoveUser_DeletesFromRoleMap(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "bob": manager.RoleViewer}
	store := &fakeRoleMapStore{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	s.SetRoleMapStore(store)
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodDelete, "/users/bob", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "removed from the role map") {
		t.Errorf("expected a removal confirmation, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), ">bob<") {
		t.Errorf("bob should no longer be listed after removal, got: %s", rec.Body.String())
	}
	if _, stillThere := store.lastSave.RoleMap["bob"]; stillThere {
		t.Errorf("persisted role map still has bob: %+v", store.lastSave.RoleMap)
	}
}

// TestServer_RemoveUser_RefusesToRemoveLastAdmin is the direct
// regression test for updateRoleMap's own lockout-prevention rule.
func TestServer_RemoveUser_RefusesToRemoveLastAdmin(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "bob": manager.RoleViewer}
	store := &fakeRoleMapStore{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	s.SetRoleMapStore(store)
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodDelete, "/users/admin", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "no admin account") {
		t.Errorf("expected a refusal citing the no-admin-left rule, got: %s", rec.Body.String())
	}
	if store.saves != 0 {
		t.Errorf("Save should never have been called, got %d calls", store.saves)
	}
	s.roleMapMu.RLock()
	_, stillAdmin := s.roleMap["admin"]
	s.roleMapMu.RUnlock()
	if !stillAdmin {
		t.Errorf("admin should still be in the in-memory role map after a refused removal")
	}
}

// TestServer_SetUserRole_RefusesToDemoteLastAdmin covers the same rule
// via a demotion rather than an outright removal.
func TestServer_SetUserRole_RefusesToDemoteLastAdmin(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "bob": manager.RoleViewer}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodPost, "/users/admin/role", strings.NewReader("role=viewer"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "no admin account") {
		t.Errorf("expected a refusal citing the no-admin-left rule, got: %s", rec.Body.String())
	}
}

// TestServer_UpdateRoleMap_SaveFailureLeavesInMemoryMapUnchanged proves
// the validate-then-persist-then-apply ordering actually holds: a
// failing store must never let the in-memory map drift from what's on
// disk.
func TestServer_UpdateRoleMap_SaveFailureLeavesInMemoryMapUnchanged(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "bob": manager.RoleViewer}
	store := &fakeRoleMapStore{err: errSaveBoom}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	s.SetRoleMapStore(store)
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodPost, "/users/bob/role", strings.NewReader("role=operator"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "saving role map") {
		t.Errorf("expected the save error to be surfaced, got: %s", rec.Body.String())
	}
	s.roleMapMu.RLock()
	got := s.roleMap["bob"]
	s.roleMapMu.RUnlock()
	if got != manager.RoleViewer {
		t.Errorf("in-memory role for bob = %q, want it unchanged at viewer after a save failure", got)
	}
}

func TestServer_RemoveUser_ForbiddenForNonAdmin(t *testing.T) {
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator, "bob": manager.RoleViewer}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "ops", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodDelete, "/users/bob", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (Operator must not be able to remove a role-map entry)", rec.Code, http.StatusForbidden)
	}
}

var errSaveBoom = &saveBoomError{}

type saveBoomError struct{}

func (*saveBoomError) Error() string { return "disk full" }
