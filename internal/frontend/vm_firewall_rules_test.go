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

func TestHandleSetVMFirewallRules_ForwardsParsedRows(t *testing.T) {
	client := &fakeClient{
		getVMResp:              &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1"}},
		setVMFirewallRulesResp: &rpcpb.SetVMFirewallRulesResponse{},
	}
	s := newTestServer(t, client)

	form := url.Values{
		"fw_direction": {"in", "out"}, "fw_action": {"pass", "block"},
		"fw_protocol": {"tcp", ""}, "fw_port": {"443", ""}, "fw_priority": {"0", "5"},
	}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/firewall-rules", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastSetVMFirewallRulesReq
	if got.GetId() != "vm-1" || len(got.GetFirewallRules()) != 2 {
		t.Fatalf("forwarded request = %+v, want Id=vm-1 and 2 rules", got)
	}
	if got.GetFirewallRules()[0].GetPortRange() != "443" || got.GetFirewallRules()[1].GetPriority() != 5 {
		t.Errorf("forwarded rules = %+v, want the parsed field values preserved", got.GetFirewallRules())
	}
}

func TestHandleSetVMFirewallRules_EmptySubmissionClearsRules(t *testing.T) {
	client := &fakeClient{
		getVMResp:              &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
		setVMFirewallRulesResp: &rpcpb.SetVMFirewallRulesResponse{},
	}
	s := newTestServer(t, client)

	// Every row removed client-side before submit - no fw_* fields at all.
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/firewall-rules", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(client.lastSetVMFirewallRulesReq.GetFirewallRules()) != 0 {
		t.Errorf("forwarded rules = %+v, want empty (clears all rules)", client.lastSetVMFirewallRulesReq.GetFirewallRules())
	}
}

func TestHandleSetVMFirewallRules_RPCErrorRendersFirewallFormErrorNotCloudflare(t *testing.T) {
	client := &fakeClient{
		getVMResp:              &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
		setVMFirewallRulesResp: &rpcpb.SetVMFirewallRulesResponse{Error: "vm \"vm-1\" does not exist"},
	}
	s := newTestServer(t, client)

	form := url.Values{"fw_direction": {"in"}, "fw_action": {"pass"}}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/firewall-rules", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "does not exist") {
		t.Fatalf("expected the RPC's own error to render, got: %s", body)
	}
}

func TestHandleVMPage_RendersExistingFirewallRulesAsEditableForm(t *testing.T) {
	client := &fakeClient{getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
		Id: "vm-1", Name: "web-1",
		FirewallRules: []*rpcpb.FirewallRule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Priority: 3}},
	}}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `action="/vms/vm-1/firewall-rules"`) {
		t.Fatalf("expected a firewall-rules edit form, got: %s", body)
	}
	if !strings.Contains(body, `value="22"`) || !strings.Contains(body, `value="3"`) {
		t.Errorf("expected the existing rule's port/priority pre-filled into the edit form, got: %s", body)
	}
	if !strings.Contains(body, `<option value="in" selected>in</option>`) {
		t.Errorf("expected the existing rule's direction pre-selected, got: %s", body)
	}
}

func TestHandleVMPage_ViewerSeesReadOnlyFirewallTableNotEditForm(t *testing.T) {
	client := &fakeClient{getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
		Id: "vm-1", Name: "web-1",
		FirewallRules: []*rpcpb.FirewallRule{{Direction: "in", Action: "block", PortRange: "22"}},
	}}}
	roleMap := map[string]manager.Role{"viewer": manager.RoleViewer}
	s, err := NewServer(client, fakeAuthenticator{user: "viewer", pass: "secret"}, roleMap, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("viewer", manager.RoleViewer)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `action="/vms/vm-1/firewall-rules"`) {
		t.Errorf("viewer should not see the firewall-rules edit form, got: %s", body)
	}
	if !strings.Contains(body, "22") {
		t.Errorf("viewer should still see the read-only firewall table, got: %s", body)
	}
}
