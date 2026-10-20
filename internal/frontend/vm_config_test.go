package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
)

func postVMConfig(t *testing.T, s *Server, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func vmConfigClient(resp *rpcpb.SetVMConfigResponse) *fakeClient {
	return &fakeClient{
		getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
			Id: "vm-1", Name: "web-1", NodeId: "node-a", Vcpus: 2, MemoryMb: 1024,
		}},
		setVMConfigResp: resp,
	}
}

func TestHandleSetVMConfig_ForwardsOnlyComputeFields(t *testing.T) {
	client := vmConfigClient(&rpcpb.SetVMConfigResponse{})
	s := newTestServer(t, client)

	rec := postVMConfig(t, s, url.Values{"vcpus": {"4"}, "memory_mb": {"2048"}}, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastSetVMConfigReq
	if got.GetId() != "vm-1" || got.GetVcpus() != 4 || got.GetMemoryMb() != 2048 {
		t.Fatalf("forwarded request = %+v, want Id=vm-1 vcpus=4 memory_mb=2048", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Restart required") || !strings.Contains(body, "Saved: 4 vCPU and 2048 MB") {
		t.Errorf("success page must say the change is restart-required; body did not contain that notice")
	}
}

func TestHandleSetVMConfig_NonNumericInputIsNotSubmitted(t *testing.T) {
	client := vmConfigClient(&rpcpb.SetVMConfigResponse{})
	s := newTestServer(t, client)

	rec := postVMConfig(t, s, url.Values{"vcpus": {"two"}, "memory_mb": {"2048"}}, nil)

	if client.lastSetVMConfigReq != nil {
		t.Errorf("RPC was called with %+v; a non-numeric vCPU count must never be submitted", client.lastSetVMConfigReq)
	}
	if !strings.Contains(rec.Body.String(), "vCPUs must be a whole number") {
		t.Errorf("expected a plain vCPU message, got: %s", rec.Body.String())
	}
}

// TestHandleSetVMConfig_RefusalKeepsOperatorInput checks that a refused
// edit shows the reason and re-renders what the operator typed, rather
// than silently reverting the form to the stored values.
func TestHandleSetVMConfig_RefusalKeepsOperatorInput(t *testing.T) {
	client := vmConfigClient(&rpcpb.SetVMConfigResponse{
		Error: "SetVMConfig: vcpus 17 is outside the supported range 1-16",
	})
	s := newTestServer(t, client)

	rec := postVMConfig(t, s, url.Values{"vcpus": {"17"}, "memory_mb": {"2048"}}, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "outside the supported range 1-16") {
		t.Errorf("refusal reason not rendered; body=%s", body)
	}
	if !strings.Contains(body, `name="vcpus"`) || !strings.Contains(body, `value="17"`) {
		t.Errorf("operator's rejected vCPU value 17 not re-rendered in the form")
	}
}

func TestHandleSetVMConfig_ViewerIsForbidden(t *testing.T) {
	client := vmConfigClient(&rpcpb.SetVMConfigResponse{})
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator, "bob": manager.RoleViewer}
	s, err := NewServer(client, fakeAuthenticator{user: "ops", pass: "secret"}, roleMap, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	token, _ := s.sessions.Create("bob", manager.RoleViewer)

	rec := postVMConfig(t, s, url.Values{"vcpus": {"4"}, "memory_mb": {"2048"}}, &http.Cookie{Name: sessionCookieName, Value: token})

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d for a Viewer", rec.Code, http.StatusForbidden)
	}
	if client.lastSetVMConfigReq != nil {
		t.Errorf("a Viewer's request reached SetVMConfig: %+v", client.lastSetVMConfigReq)
	}
}

// TestVMPage_ConfigPanelStatesRestartAndNonEditableFields checks the detail
// page for an Operator: the edit form is present with the current values,
// and the page says what applies live (nothing) and what is not editable.
func TestVMPage_ConfigPanelStatesRestartAndNonEditableFields(t *testing.T) {
	client := vmConfigClient(&rpcpb.SetVMConfigResponse{})
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`action="/vms/vm-1/config"`,
		`data-current="2"`,
		`data-current="1024"`,
		"restart-required",
		"never applied live",
		"Not editable here",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("VM page missing %q", want)
		}
	}
}

func TestVMPage_ConfigFormHiddenFromViewer(t *testing.T) {
	client := vmConfigClient(&rpcpb.SetVMConfigResponse{})
	roleMap := map[string]manager.Role{"ops": manager.RoleOperator, "bob": manager.RoleViewer}
	s, err := NewServer(client, fakeAuthenticator{user: "ops", pass: "secret"}, roleMap, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	token, _ := s.sessions.Create("bob", manager.RoleViewer)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `action="/vms/vm-1/config"`) {
		t.Error("a Viewer was shown the Edit configuration form")
	}
}
