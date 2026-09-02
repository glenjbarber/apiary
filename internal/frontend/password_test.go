package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/manager"
	"github.com/glenjbarber/apiary/internal/pam"
)

// TestCanChangePassword verifies all nine (actor, target) pairs against
// the authorization matrix confirmed directly for this feature
// (ADR-0039): Admin can change anyone's password, Operator can change
// its own and Viewer's but never Admin's, and Viewer can change no
// one's, not even its own.
func TestCanChangePassword(t *testing.T) {
	cases := []struct {
		actor, target manager.Role
		want          bool
	}{
		{manager.RoleAdmin, manager.RoleAdmin, true},
		{manager.RoleAdmin, manager.RoleOperator, true},
		{manager.RoleAdmin, manager.RoleViewer, true},
		{manager.RoleOperator, manager.RoleAdmin, false},
		{manager.RoleOperator, manager.RoleOperator, true},
		{manager.RoleOperator, manager.RoleViewer, true},
		{manager.RoleViewer, manager.RoleAdmin, false},
		{manager.RoleViewer, manager.RoleOperator, false},
		{manager.RoleViewer, manager.RoleViewer, false},
	}
	for _, c := range cases {
		if got := canChangePassword(c.actor, c.target); got != c.want {
			t.Errorf("canChangePassword(%s, %s) = %v, want %v", c.actor, c.target, got, c.want)
		}
	}
}

// fakePasswordSetter records SetPassword calls without touching real
// system accounts.
type fakePasswordSetter struct {
	calls []struct{ username, password string }
	err   error
}

func (f *fakePasswordSetter) SetPassword(username, newPassword string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct{ username, password string }{username, newPassword})
	return nil
}

func newTestServerWithRoles(t *testing.T, roleMap map[string]manager.Role, auth pam.Authenticator, passwords PasswordSetter) *Server {
	t.Helper()
	s, err := NewServer(&fakeClient{}, auth, roleMap, nil, "", "", passwords)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	return s
}

func TestServer_UsersPage_ListsEveryKnownAccount(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "ops": manager.RoleOperator, "viewer": manager.RoleViewer}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"admin", "ops", "viewer"} {
		if !strings.Contains(body, want) {
			t.Errorf("Users page missing %q, got: %s", want, body)
		}
	}
}

func TestServer_UsersPage_AdminSeesChangeActionForEveryRow(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "ops": manager.RoleOperator, "viewer": manager.RoleViewer}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "admin", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got := strings.Count(rec.Body.String(), "<details>"); got != 3 {
		t.Errorf("Admin should see 3 change-password actions (one per row), got %d", got)
	}
}

func TestServer_UsersPage_ViewerSeesNoChangeAction(t *testing.T) {
	roleMap := map[string]manager.Role{"admin": manager.RoleAdmin, "ops": manager.RoleOperator, "viewer": manager.RoleViewer}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "viewer", pass: "secret"}, &fakePasswordSetter{})
	token, _ := s.sessions.Create("viewer", manager.RoleViewer)

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "Change password") {
		t.Errorf("Viewer should see no change-password action at all, got: %s", rec.Body.String())
	}
}

func TestServer_ChangePassword_OperatorCanChangeViewer(t *testing.T) {
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator, "viewer": manager.RoleViewer}
	setter := &fakePasswordSetter{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "ops", pass: "secret"}, setter)
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/users/viewer/password", strings.NewReader(
		"current_password=secret&new_password=newpassword1&confirm_password=newpassword1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if len(setter.calls) != 1 || setter.calls[0].username != "viewer" || setter.calls[0].password != "newpassword1" {
		t.Errorf("SetPassword calls = %+v, want one call for viewer/newpassword1", setter.calls)
	}
	if !strings.Contains(rec.Body.String(), "changed successfully") {
		t.Errorf("expected a success message, got: %s", rec.Body.String())
	}
}

func TestServer_ChangePassword_OperatorCannotChangeAdmin(t *testing.T) {
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator, "admin": manager.RoleAdmin}
	setter := &fakePasswordSetter{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "ops", pass: "secret"}, setter)
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/users/admin/password", strings.NewReader(
		"current_password=secret&new_password=newpassword1&confirm_password=newpassword1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if len(setter.calls) != 0 {
		t.Errorf("SetPassword should not have been called, got: %+v", setter.calls)
	}
	if !strings.Contains(rec.Body.String(), "does not permit") {
		t.Errorf("expected a permission-denied message, got: %s", rec.Body.String())
	}
}

func TestServer_ChangePassword_ViewerBlockedByRouteGate(t *testing.T) {
	roleMap := map[string]manager.Role{"viewer": manager.RoleViewer}
	setter := &fakePasswordSetter{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "viewer", pass: "secret"}, setter)
	token, _ := s.sessions.Create("viewer", manager.RoleViewer)

	req := httptest.NewRequest(http.MethodPost, "/users/viewer/password", strings.NewReader(
		"current_password=secret&new_password=newpassword1&confirm_password=newpassword1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (forbidden by the route's own requireRole gate)", rec.Code, http.StatusForbidden)
	}
	if len(setter.calls) != 0 {
		t.Errorf("SetPassword should not have been called, got: %+v", setter.calls)
	}
}

func TestServer_ChangePassword_WrongCurrentPasswordIsRejected(t *testing.T) {
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator, "viewer": manager.RoleViewer}
	setter := &fakePasswordSetter{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "ops", pass: "secret"}, setter)
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/users/viewer/password", strings.NewReader(
		"current_password=wrong&new_password=newpassword1&confirm_password=newpassword1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if len(setter.calls) != 0 {
		t.Errorf("SetPassword should not have been called with a wrong current password, got: %+v", setter.calls)
	}
	if !strings.Contains(rec.Body.String(), "current password is incorrect") {
		t.Errorf("expected an incorrect-current-password message, got: %s", rec.Body.String())
	}
}

func TestServer_ChangePassword_MismatchedConfirmationIsRejected(t *testing.T) {
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator, "viewer": manager.RoleViewer}
	setter := &fakePasswordSetter{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "ops", pass: "secret"}, setter)
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/users/viewer/password", strings.NewReader(
		"current_password=secret&new_password=newpassword1&confirm_password=different1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if len(setter.calls) != 0 {
		t.Errorf("SetPassword should not have been called with mismatched confirmation, got: %+v", setter.calls)
	}
	if !strings.Contains(rec.Body.String(), "do not match") {
		t.Errorf("expected a mismatch message, got: %s", rec.Body.String())
	}
}

func TestServer_ChangePassword_TooShortIsRejected(t *testing.T) {
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator, "viewer": manager.RoleViewer}
	setter := &fakePasswordSetter{}
	s := newTestServerWithRoles(t, roleMap, fakeAuthenticator{user: "ops", pass: "secret"}, setter)
	token, _ := s.sessions.Create("ops", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/users/viewer/password", strings.NewReader(
		"current_password=secret&new_password=short&confirm_password=short"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if len(setter.calls) != 0 {
		t.Errorf("SetPassword should not have been called with a too-short password, got: %+v", setter.calls)
	}
	if !strings.Contains(rec.Body.String(), "at least 8 characters") {
		t.Errorf("expected a too-short message, got: %s", rec.Body.String())
	}
}
