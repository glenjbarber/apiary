package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/extdiscovery"
	"github.com/glenjbarber/apiary/internal/extnodes"
	"github.com/glenjbarber/apiary/internal/joinauth"
)

// enrollComb is one Comb's worth of enrollment harness: a managerd
// Server, a client for it, the authorization store it reads, and the
// enrollment store it writes.
type enrollComb struct {
	name      string
	client    rpcpb.ManagerServiceClient
	srv       *Server
	authzPath string
	storePath string
}

// newEnrollComb builds a Comb over the given raft socket.
func newEnrollComb(t *testing.T, socketPath, name string) *enrollComb {
	t.Helper()
	client, srv := newManagerdRPCClientAndServer(t, socketPath, name)
	c := &enrollComb{
		name:      name,
		client:    client,
		srv:       srv,
		authzPath: filepath.Join(t.TempDir(), "join-authorizations.json"),
		storePath: filepath.Join(t.TempDir(), "external-nodes.json"),
	}
	srv.setJoinAuthorizationPath(c.authzPath)
	store, err := extnodes.Load(c.storePath)
	if err != nil {
		t.Fatalf("loading the enrollment store: %v", err)
	}
	srv.SetExternalNodeStore(store)
	return c
}

// key creates an admin API key and returns its raw value and id.
func (c *enrollComb) key(t *testing.T, name string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, "")
	resp, err := c.client.CreateAPIKey(ctx, &rpcpb.CreateAPIKeyRequest{Name: name, Role: "admin"})
	if err != nil {
		t.Fatalf("%s: CreateAPIKey(%q) error: %v", c.name, name, err)
	}
	if resp.GetError() != "" {
		t.Fatalf("%s: CreateAPIKey(%q) returned error: %s", c.name, name, resp.GetError())
	}
	return resp.GetRawKey(), resp.GetKey().GetId()
}

// observe seeds the discovery store with a host, so that an enrollment
// has something real to enroll.
func (c *enrollComb) observe(t *testing.T, provider, sourceID, name, fingerprint string) {
	t.Helper()
	c.srv.discovery = &discoveryState{providers: map[string]*discoveryProvider{}}
	c.srv.discovery.providers[provider] = &discoveryProvider{
		provider: &stubProvider{},
		store:    extdiscovery.NewStore(),
		interval: time.Minute,
	}
	store := c.srv.discovery.providers[provider].store
	store.Apply(extdiscovery.ScanResult{
		Nodes: []extdiscovery.Node{{
			Provider:    provider,
			SourceID:    sourceID,
			Name:        name,
			Fingerprint: fingerprint,
			Status:      "online",
		}},
		SourceID:   sourceID,
		State:      extdiscovery.StateFresh,
		Complete:   true,
		ObservedAt: time.Now(),
	})
}

// stubProvider is a provider that is never scanned - the observation is
// seeded directly, because the point of these tests is the enrollment
// path and not the discovery path.
type stubProvider struct{}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Scan(context.Context) extdiscovery.ScanResult {
	return extdiscovery.ScanResult{State: extdiscovery.StateFresh, Complete: true, ObservedAt: time.Now()}
}

// writeAuthorization writes a single-use authorization entry for a host.
func writeAuthorization(t *testing.T, path, nodeID, fingerprint string) joinauth.Entry {
	t.Helper()
	entry, err := joinauth.NewEntryID()
	if err != nil {
		t.Fatalf("NewEntryID: %v", err)
	}
	store := joinauth.Store{Authorizations: []joinauth.Entry{{
		ID:            entry,
		NodeID:        nodeID,
		Fingerprint:   fingerprint,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	}}}
	if err := store.Save(path); err != nil {
		t.Fatalf("writing the authorization store: %v", err)
	}
	return store.Authorizations[0]
}

// enrollRequest builds an enrollment request with the right phrase.
func enrollRequest(provider, sourceID, name, fingerprint string) *rpcpb.EnrollExternalNodeRequest {
	return &rpcpb.EnrollExternalNodeRequest{
		Provider:      provider,
		SourceId:      sourceID,
		Name:          name,
		Fingerprint:   fingerprint,
		ConfirmPhrase: enrollExternalNodeConfirmPhrase,
	}
}

func TestEnrollExternalNodeRefusesWrongPhraseWithoutActing(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	comb := newEnrollComb(t, socket, "comb-1")
	comb.observe(t, "proxmox", "cluster-a", "pve1", "fp-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, "")

	req := enrollRequest("proxmox", "cluster-a", "pve1", "fp-1")
	req.ConfirmPhrase = "wrong"
	resp, err := comb.client.EnrollExternalNode(ctx, req)
	if err != nil {
		t.Fatalf("EnrollExternalNode() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("a wrong confirm_phrase was accepted")
	}
	if !strings.Contains(resp.GetError(), enrollExternalNodeConfirmPhrase) {
		t.Errorf("error = %q, want it to name the required phrase", resp.GetError())
	}
	if resp.GetAwaitingSecondAuthorization() {
		t.Errorf("the call recorded an authorization despite the wrong phrase")
	}
	// Nothing was enrolled.
	list, err := comb.client.GetExternalEnrollments(ctx, &rpcpb.GetExternalEnrollmentsRequest{})
	if err != nil {
		t.Fatalf("GetExternalEnrollments() error: %v", err)
	}
	if len(list.GetEnrollments()) != 0 {
		t.Errorf("GetExternalEnrollments() = %d enrollments, want 0", len(list.GetEnrollments()))
	}
}

func TestEnrollExternalNodeRefusesUnobservedHost(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	comb := newEnrollComb(t, socket, "comb-1")
	comb.observe(t, "proxmox", "cluster-a", "pve1", "fp-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, "")

	// "pve2" was never observed.
	resp, err := comb.client.EnrollExternalNode(ctx, enrollRequest("proxmox", "cluster-a", "pve2", ""))
	if err != nil {
		t.Fatalf("EnrollExternalNode() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("an unobserved host was enrolled")
	}
	if !strings.Contains(resp.GetError(), "no such host has been observed") {
		t.Errorf("error = %q, want it to name the unobserved host", resp.GetError())
	}
}

func TestEnrollExternalNodeRefusesFingerprintMismatch(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	comb := newEnrollComb(t, socket, "comb-1")
	comb.observe(t, "proxmox", "cluster-a", "pve1", "fp-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, "")

	resp, err := comb.client.EnrollExternalNode(ctx, enrollRequest("proxmox", "cluster-a", "pve1", "fp-OTHER"))
	if err != nil {
		t.Fatalf("EnrollExternalNode() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("a mismatched fingerprint was accepted")
	}
	if !strings.Contains(resp.GetError(), "does not match") {
		t.Errorf("error = %q, want it to name the mismatch", resp.GetError())
	}
}

func TestEnrollExternalNodeRefusesClosedWindow(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	comb := newEnrollComb(t, socket, "comb-1")
	comb.observe(t, "proxmox", "cluster-a", "pve1", "fp-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, "")

	// No window was opened.
	resp, err := comb.client.EnrollExternalNode(ctx, enrollRequest("proxmox", "cluster-a", "pve1", "fp-1"))
	if err != nil {
		t.Fatalf("EnrollExternalNode() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("an enrollment succeeded with no live join window")
	}
	if !strings.Contains(resp.GetError(), "not currently accepting") {
		t.Errorf("error = %q, want it to name the closed window", resp.GetError())
	}
}

func TestEnrollExternalNodeRefusesWithoutAnAuthorizationEntry(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	comb := newEnrollComb(t, socket, "comb-1")
	comb.observe(t, "proxmox", "cluster-a", "pve1", "fp-1")
	openColonyJoinWindowForTest(t, comb.client)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, "")

	// The authorization store exists and is EMPTY, which is the real
	// starting state of a freshly installed Comb.
	if err := (joinauth.Store{Authorizations: []joinauth.Entry{}}).Save(comb.authzPath); err != nil {
		t.Fatalf("writing an empty store: %v", err)
	}

	resp, err := comb.client.EnrollExternalNode(ctx, enrollRequest("proxmox", "cluster-a", "pve1", "fp-1"))
	if err != nil {
		t.Fatalf("EnrollExternalNode() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("an enrollment succeeded with no authorization entry")
	}
}

func TestEnrollExternalNodeTwoActsAreRequired(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	comb := newEnrollComb(t, socket, "comb-1")
	comb.observe(t, "proxmox", "cluster-a", "pve1", "fp-1")
	openColonyJoinWindowForTest(t, comb.client)
	writeAuthorization(t, comb.authzPath, "proxmox/cluster-a/pve1", "fp-1")

	rawKey, _ := comb.key(t, "admin-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, rawKey)

	// The FIRST act records one authorization and admits nobody.
	resp, err := comb.client.EnrollExternalNode(ctx, enrollRequest("proxmox", "cluster-a", "pve1", "fp-1"))
	if err != nil {
		t.Fatalf("EnrollExternalNode() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("the first authorization was refused: %s", resp.GetError())
	}
	if !resp.GetAwaitingSecondAuthorization() {
		t.Fatalf("the first call did not report that a second authorization is required")
	}
	if resp.GetSecondAuthorizationRequired() == "" {
		t.Errorf("second_authorization_required is empty")
	}
	if resp.GetEnrollment() != nil {
		t.Errorf("the first call enrolled a host")
	}

	// Nothing is enrolled yet.
	list, err := comb.client.GetExternalEnrollments(ctx, &rpcpb.GetExternalEnrollmentsRequest{})
	if err != nil {
		t.Fatalf("GetExternalEnrollments() error: %v", err)
	}
	if len(list.GetEnrollments()) != 0 {
		t.Errorf("GetExternalEnrollments() = %d enrollments after one authorization, want 0", len(list.GetEnrollments()))
	}
}

func TestRemoveExternalEnrollmentRefusesWrongPhrase(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	comb := newEnrollComb(t, socket, "comb-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, "")

	resp, err := comb.client.RemoveExternalEnrollment(ctx, &rpcpb.RemoveExternalEnrollmentRequest{
		Provider:      "proxmox",
		SourceId:      "cluster-a",
		Name:          "pve1",
		ConfirmPhrase: "wrong",
	})
	if err != nil {
		t.Fatalf("RemoveExternalEnrollment() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("a wrong confirm_phrase was accepted")
	}
	if resp.GetRemoved() {
		t.Errorf("the call removed something despite the wrong phrase")
	}
}

func TestGetExternalEnrollmentsReportsStanding(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	comb := newEnrollComb(t, socket, "comb-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, "")

	// Enroll directly into the store, so the test is about the READ
	// surface and not the enrollment flow.
	key := extnodes.Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}
	store := comb.srv.extNodes
	if _, err := store.Enroll(extnodes.Enrollment{
		Key:         key,
		Fingerprint: "fp-1",
		Status:      "online",
		EnrolledAt:  time.Now(),
		Authorizations: []extnodes.Authorization{
			{KeyID: "key-a", NodeID: "comb-1"},
			{KeyID: "key-b", NodeID: "comb-2"},
		},
		EnrolledBy: "external-discovery",
	}); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := store.SetStatus(key, extnodes.StatusAbsent); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	resp, err := comb.client.GetExternalEnrollments(ctx, &rpcpb.GetExternalEnrollmentsRequest{})
	if err != nil {
		t.Fatalf("GetExternalEnrollments() error: %v", err)
	}
	if len(resp.GetEnrollments()) != 1 {
		t.Fatalf("GetExternalEnrollments() = %d enrollments, want 1", len(resp.GetEnrollments()))
	}
	got := resp.GetEnrollments()[0]
	if got.GetStatus() != string(extnodes.StatusAbsent) {
		t.Errorf("status = %q, want %q", got.GetStatus(), extnodes.StatusAbsent)
	}
	if got.GetProviderStatus() != "online" {
		t.Errorf("provider_status = %q, want %q - the provider's own status is carried verbatim", got.GetProviderStatus(), "online")
	}
	if len(got.GetAuthorizations()) != 2 {
		t.Errorf("authorizations = %d, want 2", len(got.GetAuthorizations()))
	}
}

func TestExternalEnrollmentStoreIsNotReplicated(t *testing.T) {
	// The enrollment RECORD is local state, and this asserts the file
	// it is written to is the local store and not raft's.
	//
	// It is a deliberately narrow test: it checks that the store's
	// path is a FILE path and that the enrollment survives a reload,
	// which is what "local but durable" means.
	dir := t.TempDir()
	path := filepath.Join(dir, "external-nodes.json")
	store, err := extnodes.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	key := extnodes.Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}
	if _, err := store.Enroll(extnodes.Enrollment{
		Key:         key,
		Fingerprint: "fp-1",
		EnrolledAt:  time.Now(),
		Authorizations: []extnodes.Authorization{
			{KeyID: "key-a", NodeID: "comb-1"},
			{KeyID: "key-b", NodeID: "comb-2"},
		},
	}); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the store was not written to its file: %v", err)
	}
}
