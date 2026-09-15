package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

// waitForRestart polls fake's restartName for up to a second -
// scheduleServiceRestart deliberately restarts on a delayed goroutine
// (250ms, matching RestartNodeService's own flush-first pattern), so a
// test asserting on it must wait rather than check immediately.
func waitForRestart(t *testing.T, fake *fakeNodeServiceController) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if name := fake.lastRestartName(); name != "" {
			return name
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fake.lastRestartName()
}

// fakeFrontendConfigStore/fakeRestshimdConfigStore/fakeRaftdConfigStore
// are fakes for the new ADR-0102 config stores, without any real file
// I/O involved - same shape as fakeNodeConfigStore above.

type fakeFrontendConfigStore struct {
	cfg      frontendconfig.Config
	loadErr  error
	saveErr  error
	lastSave frontendconfig.Config
}

func (f *fakeFrontendConfigStore) Load() (frontendconfig.Config, error) {
	if f.loadErr != nil {
		return frontendconfig.Config{}, f.loadErr
	}
	return f.cfg, nil
}

func (f *fakeFrontendConfigStore) Save(cfg frontendconfig.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.lastSave = cfg
	return nil
}

type fakeRestshimdConfigStore struct {
	cfg      restshimdconfig.Config
	loadErr  error
	saveErr  error
	lastSave restshimdconfig.Config
}

func (f *fakeRestshimdConfigStore) Load() (restshimdconfig.Config, error) {
	if f.loadErr != nil {
		return restshimdconfig.Config{}, f.loadErr
	}
	return f.cfg, nil
}

func (f *fakeRestshimdConfigStore) Save(cfg restshimdconfig.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.lastSave = cfg
	return nil
}

type fakeRaftdConfigStore struct {
	cfg      raftdconfig.Config
	loadErr  error
	saveErr  error
	lastSave raftdconfig.Config
}

func (f *fakeRaftdConfigStore) Load() (raftdconfig.Config, error) {
	if f.loadErr != nil {
		return raftdconfig.Config{}, f.loadErr
	}
	return f.cfg, nil
}

func (f *fakeRaftdConfigStore) Save(cfg raftdconfig.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.lastSave = cfg
	return nil
}

func TestServer_GetFrontendConfig_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.GetFrontendConfig(context.Background(), &rpcpb.GetFrontendConfigRequest{})
	if err != nil {
		t.Fatalf("GetFrontendConfig() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Error("GetFrontendConfig() error field = empty, want a not-configured error")
	}
}

func TestServer_GetFrontendConfig(t *testing.T) {
	store := &fakeFrontendConfigStore{cfg: frontendconfig.Config{
		ManagerAddr: "10.50.0.9:17700", HTTPAddr: "0.0.0.0:8080", PeerTLS: true,
	}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetFrontendConfig(store)

	resp, err := s.GetFrontendConfig(context.Background(), &rpcpb.GetFrontendConfigRequest{})
	if err != nil {
		t.Fatalf("GetFrontendConfig() error: %v", err)
	}
	if resp.GetManagerAddr() != "10.50.0.9:17700" || resp.GetHttpAddr() != "0.0.0.0:8080" || !resp.GetPeerTls() {
		t.Errorf("GetFrontendConfig() = %+v, want the saved values", resp)
	}
}

// TestServer_GetFrontendConfig_NeverReturnsSecrets is the regression
// test for ADR-0102's write-only secret design, mirroring
// TestServer_GetNodeConfig_NeverReturnsSecrets: ManagerAPIKey must
// never appear anywhere in a GetFrontendConfig response, only whether
// one is currently set.
func TestServer_GetFrontendConfig_NeverReturnsSecrets(t *testing.T) {
	store := &fakeFrontendConfigStore{cfg: frontendconfig.Config{ManagerAPIKey: "apk_supersecret"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetFrontendConfig(store)

	resp, err := s.GetFrontendConfig(context.Background(), &rpcpb.GetFrontendConfigRequest{})
	if err != nil {
		t.Fatalf("GetFrontendConfig() error: %v", err)
	}
	if !resp.GetManagerApiKeySet() {
		t.Errorf("GetFrontendConfig() ManagerApiKeySet = false, want true")
	}
	body, err := (protojson.MarshalOptions{}).Marshal(resp)
	if err != nil {
		t.Fatalf("marshaling response: %v", err)
	}
	if strings.Contains(string(body), "supersecret") {
		t.Errorf("GetFrontendConfig() response leaked a secret value: %s", body)
	}
}

func TestServer_UpdateFrontendConfig_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.UpdateFrontendConfig(context.Background(), &rpcpb.UpdateFrontendConfigRequest{})
	if err != nil {
		t.Fatalf("UpdateFrontendConfig() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Error("UpdateFrontendConfig() error field = empty, want a not-configured error")
	}
}

func TestServer_UpdateFrontendConfig(t *testing.T) {
	store := &fakeFrontendConfigStore{}
	services := &fakeNodeServiceController{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetFrontendConfig(store)
	s.services = services

	resp, err := s.UpdateFrontendConfig(context.Background(), &rpcpb.UpdateFrontendConfigRequest{
		ManagerAddr: "10.50.0.9:17700", HttpAddr: "0.0.0.0:8080", PeerTls: true,
	})
	if err != nil {
		t.Fatalf("UpdateFrontendConfig() error: %v", err)
	}
	if resp.GetError() != "" || !resp.GetScheduled() {
		t.Fatalf("UpdateFrontendConfig() = %+v, want success and scheduled restart", resp)
	}
	if store.lastSave.ManagerAddr != "10.50.0.9:17700" || store.lastSave.HTTPAddr != "0.0.0.0:8080" || !store.lastSave.PeerTLS {
		t.Errorf("saved config = %+v, want ManagerAddr/HTTPAddr/PeerTLS as submitted", store.lastSave)
	}
}

// TestServer_UpdateFrontendConfig_SecretSemantics mirrors
// TestServer_UpdateNodeConfig_SecretSemantics for ManagerAPIKey.
func TestServer_UpdateFrontendConfig_SecretSemantics(t *testing.T) {
	t.Run("empty leaves current value unchanged", func(t *testing.T) {
		store := &fakeFrontendConfigStore{cfg: frontendconfig.Config{ManagerAPIKey: "apk_existing"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
		s.SetFrontendConfig(store)
		s.services = &fakeNodeServiceController{}
		if _, err := s.UpdateFrontendConfig(context.Background(), &rpcpb.UpdateFrontendConfigRequest{}); err != nil {
			t.Fatalf("UpdateFrontendConfig() error: %v", err)
		}
		if store.lastSave.ManagerAPIKey != "apk_existing" {
			t.Errorf("saved ManagerAPIKey = %q, want it left unchanged", store.lastSave.ManagerAPIKey)
		}
	})
	t.Run("non-empty sets a new value", func(t *testing.T) {
		store := &fakeFrontendConfigStore{cfg: frontendconfig.Config{ManagerAPIKey: "apk_old"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
		s.SetFrontendConfig(store)
		s.services = &fakeNodeServiceController{}
		if _, err := s.UpdateFrontendConfig(context.Background(), &rpcpb.UpdateFrontendConfigRequest{ManagerApiKey: "apk_new"}); err != nil {
			t.Fatalf("UpdateFrontendConfig() error: %v", err)
		}
		if store.lastSave.ManagerAPIKey != "apk_new" {
			t.Errorf("saved ManagerAPIKey = %q, want apk_new", store.lastSave.ManagerAPIKey)
		}
	})
	t.Run("explicit clear flag clears it", func(t *testing.T) {
		store := &fakeFrontendConfigStore{cfg: frontendconfig.Config{ManagerAPIKey: "apk_old"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
		s.SetFrontendConfig(store)
		s.services = &fakeNodeServiceController{}
		if _, err := s.UpdateFrontendConfig(context.Background(), &rpcpb.UpdateFrontendConfigRequest{ClearManagerApiKey: true}); err != nil {
			t.Fatalf("UpdateFrontendConfig() error: %v", err)
		}
		if store.lastSave.ManagerAPIKey != "" {
			t.Errorf("saved ManagerAPIKey = %q, want empty after an explicit clear", store.lastSave.ManagerAPIKey)
		}
	})
}

func TestServer_GetRestshimdConfig(t *testing.T) {
	store := &fakeRestshimdConfigStore{cfg: restshimdconfig.Config{ManagerAddr: "10.50.0.9:17700", HTTPAddr: "0.0.0.0:8081"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetRestshimdConfig(store)

	resp, err := s.GetRestshimdConfig(context.Background(), &rpcpb.GetRestshimdConfigRequest{})
	if err != nil {
		t.Fatalf("GetRestshimdConfig() error: %v", err)
	}
	if resp.GetManagerAddr() != "10.50.0.9:17700" || resp.GetHttpAddr() != "0.0.0.0:8081" {
		t.Errorf("GetRestshimdConfig() = %+v, want the saved values", resp)
	}
}

func TestServer_UpdateRestshimdConfig(t *testing.T) {
	store := &fakeRestshimdConfigStore{}
	services := &fakeNodeServiceController{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetRestshimdConfig(store)
	s.services = services

	resp, err := s.UpdateRestshimdConfig(context.Background(), &rpcpb.UpdateRestshimdConfigRequest{
		ManagerAddr: "10.50.0.9:17700", HttpAddr: "0.0.0.0:8081",
	})
	if err != nil {
		t.Fatalf("UpdateRestshimdConfig() error: %v", err)
	}
	if resp.GetError() != "" || !resp.GetScheduled() {
		t.Fatalf("UpdateRestshimdConfig() = %+v, want success and scheduled restart", resp)
	}
	if store.lastSave.ManagerAddr != "10.50.0.9:17700" || store.lastSave.HTTPAddr != "0.0.0.0:8081" {
		t.Errorf("saved config = %+v, want the submitted values", store.lastSave)
	}
	if got := waitForRestart(t, services); got != "apiary_restshimd" {
		t.Errorf("restart service = %q, want apiary_restshimd", got)
	}
}

func TestServer_GetRaftdConfig(t *testing.T) {
	store := &fakeRaftdConfigStore{cfg: raftdconfig.Config{
		DataDir: "/var/db/apiary/raftd", Socket: "/var/run/apiary/raftd.sock",
		NodeID: "apiverse", RaftBind: "10.50.0.9:17701",
		RaftTLSCert: "/a/cert.pem", InternalToken: "shh",
	}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetRaftdConfig(store)

	resp, err := s.GetRaftdConfig(context.Background(), &rpcpb.GetRaftdConfigRequest{})
	if err != nil {
		t.Fatalf("GetRaftdConfig() error: %v", err)
	}
	if resp.GetDataDir() != "/var/db/apiary/raftd" || resp.GetSocket() != "/var/run/apiary/raftd.sock" || resp.GetRaftBind() != "10.50.0.9:17701" {
		t.Errorf("GetRaftdConfig() display fields = %+v, want the saved values", resp)
	}
	if resp.GetRaftTlsCert() != "/a/cert.pem" {
		t.Errorf("GetRaftdConfig() RaftTlsCert = %q, want /a/cert.pem", resp.GetRaftTlsCert())
	}
	if !resp.GetInternalTokenSet() {
		t.Error("GetRaftdConfig() InternalTokenSet = false, want true")
	}
}

// TestServer_GetRaftdConfig_NeverReturnsSecretOrExcludedIdentity
// confirms InternalToken never appears raw, and that NodeID (never
// part of GetRaftdConfigResponse at all - unlike DataDir/Socket/
// RaftBind, which ARE returned read-only) doesn't leak either.
func TestServer_GetRaftdConfig_NeverReturnsSecretOrExcludedIdentity(t *testing.T) {
	store := &fakeRaftdConfigStore{cfg: raftdconfig.Config{NodeID: "apiverse-secret-name", InternalToken: "shh_supersecret"}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetRaftdConfig(store)

	resp, err := s.GetRaftdConfig(context.Background(), &rpcpb.GetRaftdConfigRequest{})
	if err != nil {
		t.Fatalf("GetRaftdConfig() error: %v", err)
	}
	body, err := (protojson.MarshalOptions{}).Marshal(resp)
	if err != nil {
		t.Fatalf("marshaling response: %v", err)
	}
	if strings.Contains(string(body), "supersecret") {
		t.Errorf("GetRaftdConfig() response leaked the internal_token value: %s", body)
	}
	if strings.Contains(string(body), "apiverse-secret-name") {
		t.Errorf("GetRaftdConfig() response leaked node_id, which must never be part of this message: %s", body)
	}
}

func TestServer_UpdateRaftdConfig_NotConfiguredIsError(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	resp, err := s.UpdateRaftdConfig(context.Background(), &rpcpb.UpdateRaftdConfigRequest{})
	if err != nil {
		t.Fatalf("UpdateRaftdConfig() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Error("UpdateRaftdConfig() error field = empty, want a not-configured error")
	}
}

func TestServer_UpdateRaftdConfig(t *testing.T) {
	store := &fakeRaftdConfigStore{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetRaftdConfig(store)

	resp, err := s.UpdateRaftdConfig(context.Background(), &rpcpb.UpdateRaftdConfigRequest{
		RaftTlsCert: "/a/cert.pem", RaftTlsKey: "/a/key.pem", RaftTlsCa: "/a/ca.pem",
	})
	if err != nil {
		t.Fatalf("UpdateRaftdConfig() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("UpdateRaftdConfig() returned error: %s", resp.GetError())
	}
	if store.lastSave.RaftTLSCert != "/a/cert.pem" || store.lastSave.RaftTLSKey != "/a/key.pem" || store.lastSave.RaftTLSCA != "/a/ca.pem" {
		t.Errorf("saved config = %+v, want the submitted TLS fields", store.lastSave)
	}
}

// TestServer_UpdateRaftdConfig_NeverAutoRestarts confirms raftd is
// never restarted through this path (consensus-critical, ADR-0102) -
// the response carries no scheduled field and the service controller
// is never invoked, unlike UpdateFrontendConfig/UpdateRestshimdConfig.
func TestServer_UpdateRaftdConfig_NeverAutoRestarts(t *testing.T) {
	store := &fakeRaftdConfigStore{}
	services := &fakeNodeServiceController{}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetRaftdConfig(store)
	s.services = services

	if _, err := s.UpdateRaftdConfig(context.Background(), &rpcpb.UpdateRaftdConfigRequest{RaftTlsCert: "/a/cert.pem"}); err != nil {
		t.Fatalf("UpdateRaftdConfig() error: %v", err)
	}
	if services.restartName != "" {
		t.Errorf("restart service = %q, want none - UpdateRaftdConfig must never auto-restart raftd", services.restartName)
	}
}

// TestServer_UpdateRaftdConfig_SecretSemantics mirrors
// TestServer_UpdateNodeConfig_SecretSemantics for InternalToken.
func TestServer_UpdateRaftdConfig_SecretSemantics(t *testing.T) {
	t.Run("empty leaves current value unchanged", func(t *testing.T) {
		store := &fakeRaftdConfigStore{cfg: raftdconfig.Config{InternalToken: "shh_existing"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
		s.SetRaftdConfig(store)
		if _, err := s.UpdateRaftdConfig(context.Background(), &rpcpb.UpdateRaftdConfigRequest{}); err != nil {
			t.Fatalf("UpdateRaftdConfig() error: %v", err)
		}
		if store.lastSave.InternalToken != "shh_existing" {
			t.Errorf("saved InternalToken = %q, want it left unchanged", store.lastSave.InternalToken)
		}
	})
	t.Run("non-empty sets a new value", func(t *testing.T) {
		store := &fakeRaftdConfigStore{cfg: raftdconfig.Config{InternalToken: "shh_old"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
		s.SetRaftdConfig(store)
		if _, err := s.UpdateRaftdConfig(context.Background(), &rpcpb.UpdateRaftdConfigRequest{InternalToken: "shh_new"}); err != nil {
			t.Fatalf("UpdateRaftdConfig() error: %v", err)
		}
		if store.lastSave.InternalToken != "shh_new" {
			t.Errorf("saved InternalToken = %q, want shh_new", store.lastSave.InternalToken)
		}
	})
	t.Run("explicit clear flag clears it", func(t *testing.T) {
		store := &fakeRaftdConfigStore{cfg: raftdconfig.Config{InternalToken: "shh_old"}}
		s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
		s.SetRaftdConfig(store)
		if _, err := s.UpdateRaftdConfig(context.Background(), &rpcpb.UpdateRaftdConfigRequest{ClearInternalToken: true}); err != nil {
			t.Fatalf("UpdateRaftdConfig() error: %v", err)
		}
		if store.lastSave.InternalToken != "" {
			t.Errorf("saved InternalToken = %q, want empty after an explicit clear", store.lastSave.InternalToken)
		}
	})
}

// TestServer_UpdateRaftdConfig_PreservesIdentityTopologyAndBootstrapFields
// is the single most important test in this file - directly targeting
// the bug class ADR-0100's own UpdateNodeConfig merge fix already
// found and fixed once, applied here to raftd's own excluded fields.
// Because UpdateRaftdConfig builds raftdconfig.Config fresh from the
// request rather than merging onto current, and NodeID/DataDir/Socket/
// RaftBind/Join/AwaitJoin have no proto counterpart at all, a call
// touching only raft_tls_cert must still leave every one of them
// exactly as they were - not just untouched fields in general, but
// specifically these six, which a naive implementation could easily
// zero out by omission.
func TestServer_UpdateRaftdConfig_PreservesIdentityTopologyAndBootstrapFields(t *testing.T) {
	store := &fakeRaftdConfigStore{cfg: raftdconfig.Config{
		DataDir:   "/var/db/apiary/raftd",
		Socket:    "/var/run/apiary/raftd.sock",
		NodeID:    "apiverse",
		RaftBind:  "10.50.0.9:17701",
		Join:      "/var/run/apiary/raftd.sock",
		AwaitJoin: true,
	}}
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	s.SetRaftdConfig(store)

	resp, err := s.UpdateRaftdConfig(context.Background(), &rpcpb.UpdateRaftdConfigRequest{RaftTlsCert: "/a/cert.pem"})
	if err != nil {
		t.Fatalf("UpdateRaftdConfig() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("UpdateRaftdConfig() returned error: %s", resp.GetError())
	}
	if store.lastSave.DataDir != "/var/db/apiary/raftd" {
		t.Errorf("saved DataDir = %q, want it preserved across an unrelated update", store.lastSave.DataDir)
	}
	if store.lastSave.Socket != "/var/run/apiary/raftd.sock" {
		t.Errorf("saved Socket = %q, want it preserved across an unrelated update", store.lastSave.Socket)
	}
	if store.lastSave.NodeID != "apiverse" {
		t.Errorf("saved NodeID = %q, want it preserved across an unrelated update", store.lastSave.NodeID)
	}
	if store.lastSave.RaftBind != "10.50.0.9:17701" {
		t.Errorf("saved RaftBind = %q, want it preserved across an unrelated update", store.lastSave.RaftBind)
	}
	if store.lastSave.Join != "/var/run/apiary/raftd.sock" {
		t.Errorf("saved Join = %q, want it preserved across an unrelated update", store.lastSave.Join)
	}
	if !store.lastSave.AwaitJoin {
		t.Error("saved AwaitJoin = false, want it preserved as true across an unrelated update")
	}
	if store.lastSave.RaftTLSCert != "/a/cert.pem" {
		t.Errorf("saved RaftTLSCert = %q, want /a/cert.pem (the field this update actually intended to change)", store.lastSave.RaftTLSCert)
	}
}
