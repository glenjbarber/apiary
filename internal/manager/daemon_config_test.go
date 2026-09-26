package manager

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/netif"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

func TestServer_UpdateNodeConfigRecordsLocalProvenance(t *testing.T) {
	store := &nodeconfig.Manager{Path: filepath.Join(t.TempDir(), "managerd.json")}
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	resp, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{
		Uplink: "em0", ChangeOrigin: "incident", ChangeRationale: "Restore service after switch replacement", ChangeEvidence: "INC-42",
	})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("UpdateNodeConfig() = (%+v, %v)", resp, err)
	}
	got, err := s.GetNodeConfig(context.Background(), &rpcpb.GetNodeConfigRequest{})
	if err != nil || got.GetError() != "" {
		t.Fatalf("GetNodeConfig() = (%+v, %v)", got, err)
	}
	if got.GetUplink() != "em0" || len(got.GetConfigChanges()) != 1 {
		t.Fatalf("config/history = uplink %q, changes %+v", got.GetUplink(), got.GetConfigChanges())
	}
	change := got.GetConfigChanges()[0]
	if change.GetField() != "uplink" || change.GetOrigin() != "incident" || change.GetRationale() != "Restore service after switch replacement" || change.GetEvidence() != "INC-42" {
		t.Fatalf("recorded change = %+v", change)
	}
}

func TestServer_UpdateNodeConfigCanAttestExistingValueWithoutChangingIt(t *testing.T) {
	store := &nodeconfig.Manager{Path: filepath.Join(t.TempDir(), "managerd.json")}
	if err := store.Save(nodeconfig.Config{BhyveBridge: "bridge0"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	resp, err := s.UpdateNodeConfig(context.Background(), &rpcpb.UpdateNodeConfigRequest{
		AnnotateField: "bhyve_bridge", ChangeOrigin: "migration", ChangeRationale: "Preserve the previous host bridge choice", ChangeEvidence: "MIG-9",
	})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("UpdateNodeConfig(attestation) = (%+v, %v)", resp, err)
	}
	got, err := store.Load()
	if err != nil || got.BhyveBridge != "bridge0" {
		t.Fatalf("saved setting = %q, %v; want unchanged bridge0", got.BhyveBridge, err)
	}
	changes, err := store.History()
	if err != nil || len(changes) != 1 || !changes[0].Attested || changes[0].Origin != nodeconfig.OriginMigration {
		t.Fatalf("history = %+v, %v; want one marked migration attestation", changes, err)
	}
}

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

func TestServer_UpdateManagerdBindAddress_PersistsLocalAddressOnly(t *testing.T) {
	store := &fakeNodeConfigStore{cfg: nodeconfig.Config{
		NodeID: "node-a", RPCAddr: "127.0.0.1:17700", RaftdSocket: "/var/run/apiary/raftd.sock",
	}}
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	s.SetNetworkInterfaceLister(func() ([]netif.Interface, error) {
		return []netif.Interface{{Name: "em0", Up: true, Addresses: []string{"10.90.0.12/24"}}}, nil
	})

	resp, err := s.UpdateManagerdBindAddress(context.Background(), &rpcpb.UpdateManagerdBindAddressRequest{RpcAddr: "10.90.0.12:17700"})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("UpdateManagerdBindAddress() = (%+v, %v)", resp, err)
	}
	if !resp.GetRestartRequired() {
		t.Fatal("UpdateManagerdBindAddress() RestartRequired = false, want true")
	}
	if store.lastSave.RPCAddr != "10.90.0.12:17700" || store.lastSave.NodeID != "node-a" || store.lastSave.RaftdSocket != "/var/run/apiary/raftd.sock" {
		t.Fatalf("saved config = %+v, want only RPCAddr changed", store.lastSave)
	}

	resp, err = s.UpdateManagerdBindAddress(context.Background(), &rpcpb.UpdateManagerdBindAddressRequest{RpcAddr: "10.90.0.99:17700"})
	if err != nil || !strings.Contains(resp.GetError(), "not assigned") {
		t.Fatalf("remote address = (%+v, %v), want local-address rejection", resp, err)
	}
}

// TestServer_UpdateManagerdBindAddressAcceptsThisCombsOwnName is the
// bind-address half of the bind-vs-dial rule. rpc_addr is where
// managerd's net.Listen address lives, so a DNS name is a legal - and on
// a colony, the only verifiable - value for it: this check used to
// demand a numeric address and therefore rejected the very address the
// cluster's certificates carry a DNS SAN for, while accepting the
// wildcard that the same guidance had documented.
//
// What it must keep doing is the anti-typo check on a NUMERIC
// non-loopback address, which is the one part of this that is genuinely
// about a property of the host rather than of the string.
func TestServer_UpdateManagerdBindAddressAcceptsThisCombsOwnName(t *testing.T) {
	newServer := func(t *testing.T) (*Server, *fakeNodeConfigStore) {
		t.Helper()
		store := &fakeNodeConfigStore{cfg: nodeconfig.Config{
			NodeID: "node-a", RPCAddr: "127.0.0.1:17700", RaftdSocket: "/var/run/apiary/raftd.sock",
		}}
		s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
		s.SetNetworkInterfaceLister(func() ([]netif.Interface, error) {
			return []netif.Interface{{Name: "em0", Up: true, Addresses: []string{"10.90.0.12/24"}}}, nil
		})
		return s, store
	}

	accepted := map[string]string{
		"this Comb's own FQDN":        "brood.lab3.home.arpa:17700",
		"this Comb's short name":      "brood:17700",
		"loopback":                    "127.0.0.1:17700",
		"IPv4 wildcard":               "0.0.0.0:17700",
		"IPv6 wildcard":               "[::]:17700",
		"an address in the inventory": "10.90.0.12:17700",
	}
	for name, addr := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			s, store := newServer(t)
			resp, err := s.UpdateManagerdBindAddress(context.Background(), &rpcpb.UpdateManagerdBindAddressRequest{RpcAddr: addr})
			if err != nil || resp.GetError() != "" {
				t.Fatalf("UpdateManagerdBindAddress(%q) = (%+v, %v), want it saved", addr, resp, err)
			}
			if !resp.GetRestartRequired() || store.lastSave.RPCAddr != addr {
				t.Fatalf("saved rpc_addr = %q (restart_required=%v), want %q", store.lastSave.RPCAddr, resp.GetRestartRequired(), addr)
			}
		})
	}

	rejected := map[string]string{
		"an empty host":             ":17700",
		"a typo in the last octet":  "10.90.0.999:17700",
		"a CIDR pasted whole":       "10.90.0.12/24:17700",
		"an underscore in the name": "brood_lab3:17700",
		"a leading-hyphen label":    "-brood:17700",
		"no port at all":            "brood.lab3.home.arpa",
		"an out-of-range port":      "brood.lab3.home.arpa:70000",
	}
	for name, addr := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			s, store := newServer(t)
			resp, err := s.UpdateManagerdBindAddress(context.Background(), &rpcpb.UpdateManagerdBindAddressRequest{RpcAddr: addr})
			if err != nil || resp.GetError() == "" {
				t.Fatalf("UpdateManagerdBindAddress(%q) = (%+v, %v), want a rejection", addr, resp, err)
			}
			if !strings.Contains(resp.GetError(), "rpc_addr") {
				t.Errorf("UpdateManagerdBindAddress(%q) error = %q, want it to name rpc_addr", addr, resp.GetError())
			}
			if store.lastSave.RPCAddr != "" {
				t.Errorf("UpdateManagerdBindAddress(%q) saved %q, want nothing saved", addr, store.lastSave.RPCAddr)
			}
		})
	}

	// The anti-typo guard itself, unchanged: a numeric address on another
	// machine is refused, and refusing it is the entire reason the
	// numeric branch consults the interface inventory at all.
	t.Run("still rejects a numeric address absent from the interface inventory", func(t *testing.T) {
		s, _ := newServer(t)
		resp, err := s.UpdateManagerdBindAddress(context.Background(), &rpcpb.UpdateManagerdBindAddressRequest{RpcAddr: "10.90.0.13:17700"})
		if err != nil || !strings.Contains(resp.GetError(), "not assigned") {
			t.Fatalf("UpdateManagerdBindAddress(10.90.0.13:17700) = (%+v, %v), want the unassigned-address rejection", resp, err)
		}
	})
}

func TestServer_UpdateManagerdBindAddressRecordsProvenance(t *testing.T) {
	store := &nodeconfig.Manager{Path: filepath.Join(t.TempDir(), "managerd.json")}
	if err := store.Save(nodeconfig.Config{RPCAddr: "127.0.0.1:17700"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, store, nil, 0, nil)
	s.SetNetworkInterfaceLister(func() ([]netif.Interface, error) {
		return []netif.Interface{{Name: "em0", Up: true, Addresses: []string{"10.90.0.12/24"}}}, nil
	})
	resp, err := s.UpdateManagerdBindAddress(context.Background(), &rpcpb.UpdateManagerdBindAddressRequest{
		RpcAddr: "10.90.0.12:17700", ChangeOrigin: "migration", ChangeRationale: "Move managerd onto the Colony network", ChangeEvidence: "MIG-7",
	})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("UpdateManagerdBindAddress() = (%+v, %v)", resp, err)
	}
	config, err := s.GetNodeConfig(context.Background(), &rpcpb.GetNodeConfigRequest{})
	if err != nil || config.GetError() != "" {
		t.Fatalf("GetNodeConfig() = (%+v, %v)", config, err)
	}
	if len(config.GetConfigChanges()) != 1 {
		t.Fatalf("config_changes = %+v, want one event", config.GetConfigChanges())
	}
	change := config.GetConfigChanges()[0]
	if change.GetField() != "rpc_addr" || change.GetOrigin() != "migration" || change.GetRationale() != "Move managerd onto the Colony network" {
		t.Fatalf("config change = %+v", change)
	}
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
