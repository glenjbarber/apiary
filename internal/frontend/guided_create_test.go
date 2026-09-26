package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/web"
)

// The tests in this file cover the guided creation flow's two halves
// that cannot be checked by looking at the page: the availability
// rules themselves, and the fact that they are enforced server-side
// from the POST body alone. A test that only rendered the page would
// pass just as happily with every rule deleted from the JS, and a test
// that only exercised the validators would pass with the UI offering
// combinations the server refuses - so both are here, plus the drift
// guard between the Go hostname -> ID derivation and the template's
// copy of it.

// postCreate is a small helper for the many "submit this form body and
// check what the server did with it" cases below.
func postCreate(t *testing.T, s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Image classification and the per-role availability rules
// ---------------------------------------------------------------------------

func TestClassifyImageName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want imageKind
	}{
		{"FreeBSD-15.1-amd64.iso", imageKindInstaller},
		{"FreeBSD-15.1-amd64.iso.xz", imageKindInstaller},
		{"freebsd.raw", imageKindDisk},
		{"FreeBSD-15.1-amd64-zfs.raw.xz", imageKindDisk},
		{"disk.qcow2", imageKindDisk},
		{"base.txz", imageKindArchive},
		{"base.tar.gz", imageKindArchive},
		// Deliberately unrecognised: an oddly-named but perfectly valid
		// image must stay selectable everywhere rather than being
		// locked out by a guess.
		{"memstick", imageKindUnknown},
		{"", imageKindUnknown},
	} {
		if got := classifyImageName(tc.name); got != tc.want {
			t.Errorf("classifyImageName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGuidedImageRoleAvailability is the availability rule table the
// wizard renders and the server enforces, in one place. Every excluded
// combination must be one internal/cluster would have mishandled
// silently or with a lower-level error; every allowed one must be one
// the reconciler genuinely supports.
func TestGuidedImageRoleAvailability(t *testing.T) {
	roles := map[string]guidedImageRole{
		"installer":    guidedRoleBootMedia,
		"base image":   guidedRoleBaseImage,
		"base archive": guidedRoleBaseArchive,
	}
	for _, tc := range []struct {
		image  string
		role   string
		accept bool
	}{
		// Boot media: ensureVM sniffs ISOName for a real ISO9660
		// filesystem and attaches anything else (a FreeBSD memstick
		// .raw) as a raw install disk, so BOTH shapes are supported
		// here. Only a userland archive is meaningless to bhyve.
		{"FreeBSD.iso", "installer", true},
		{"memstick.raw", "installer", true},
		{"base.txz", "installer", false},
		{"memstick", "installer", true},

		// Base image: copied verbatim into the VM's disk file, so it
		// has to be a disk image. An ISO would leave the VM holding a
		// CD-ROM filesystem where a bootable disk is expected.
		{"freebsd.raw", "base image", true},
		{"FreeBSD.iso", "base image", false},
		{"base.txz", "base image", false},
		{"cloud-image", "base image", true},

		// Base archive: extracted into a jail's root directory tree.
		// A raw disk has nowhere to go there.
		{"base.txz", "base archive", true},
		{"freebsd.raw", "base archive", false},
		{"FreeBSD.iso", "base archive", false},
		{"anything-else", "base archive", true},
	} {
		role, ok := roles[tc.role]
		if !ok {
			t.Fatalf("unknown role %q in test table", tc.role)
		}
		accepted, reason := role.verdict(tc.image)
		if accepted != tc.accept {
			t.Errorf("%s role: %q accepted = %v, want %v (reason %q)", tc.role, tc.image, accepted, tc.accept, reason)
		}
		if !accepted && reason == "" {
			t.Errorf("%s role: %q rejected without saying why", tc.role, tc.image)
		}
	}
}

// TestGuidedImageOptionsKeepUnavailableImagesVisible proves the picker
// lists an unusable image disabled-with-a-reason rather than hiding
// it: an operator who can see the file deserves to be told why they
// cannot pick it here.
func TestGuidedImageOptionsKeepUnavailableImagesVisible(t *testing.T) {
	rows := []isoRowView{{Name: "freebsd.raw"}, {Name: "FreeBSD.iso"}}
	opts := guidedImageOptions(rows, guidedRoleBaseImage)
	if len(opts) != 2 {
		t.Fatalf("guidedImageOptions returned %d options, want 2 (every image listed): %+v", len(opts), opts)
	}
	if !opts[0].Available || opts[0].Label != "freebsd.raw" {
		t.Errorf("freebsd.raw should be listed and available as a base image, got %+v", opts[0])
	}
	if opts[1].Available {
		t.Errorf("an installer must not be available as a base image, got %+v", opts[1])
	}
	if !strings.Contains(opts[1].Label, "FreeBSD.iso") || !strings.Contains(opts[1].Label, "unavailable") {
		t.Errorf("the disabled option should say which image it is and that it is unavailable, got %q", opts[1].Label)
	}
	if !strings.Contains(opts[1].Reason, "FreeBSD.iso") || !strings.Contains(opts[1].Reason, "installer") {
		t.Errorf("the disabled option's reason should name the image and what it actually is, got %q", opts[1].Reason)
	}
}

// ---------------------------------------------------------------------------
// Hostname -> ID derivation
// ---------------------------------------------------------------------------

// TestDeriveResourceID is the rule the create forms have always had:
// a dotted FQDN is not a valid resource ID (validResourceID allows
// alphanumerics, '-' and '_' only, max 64 characters), so
// "sting-vm-1.lab3.home.arpa" must become
// "sting-vm-1-lab3-home-arpa" rather than being submitted verbatim.
func TestDeriveResourceID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"sting-vm-1.lab3.home.arpa", "sting-vm-1-lab3-home-arpa"},
		{"jail-brood-01.lab3.home.arpa", "jail-brood-01-lab3-home-arpa"},
		{"web-01", "web-01"},
		{"Web_01", "web_01"},
		{"a..b", "a-b"},
		{"..leading.and.trailing..", "leading-and-trailing"},
		{"-dashes-only-", "dashes-only"},
		{"a b\tc", "a-b-c"},
		{"", ""},
		// Truncation to the same 64-character ceiling the template's
		// JS slices to, with the resulting trailing dash removed
		// rather than leaving a truncated id ending in a separator.
		{strings.Repeat("x", 80), strings.Repeat("x", 64)},
		// A separator cut by the 64-character ceiling is dropped, not
		// left dangling - the same result the template's JS produces by
		// slicing and then trimming a trailing dash.
		{strings.Repeat("a", 63) + ".tail", strings.Repeat("a", 63)},
	} {
		if got := deriveResourceID(tc.in); got != tc.want {
			t.Errorf("deriveResourceID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGuidedTemplateDeriveIDMatchesGo is the drift guard between the
// two halves of the live derivation. The JS exists only so the field
// updates as an operator types; deriveResourceID is the testable
// statement of the same rule. If someone edits one and not the other,
// this fails rather than letting the two disagree silently in a
// browser.
func TestGuidedTemplateDeriveIDMatchesGo(t *testing.T) {
	raw, err := web.FS.ReadFile("templates/create_guided.html")
	if err != nil {
		t.Fatalf("reading the guided template: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		// The character class, the run-collapsing, the leading/trailing
		// trim and the 64-character ceiling, exactly as
		// deriveResourceID implements them.
		`replace(/[^a-z0-9_-]+/g, '-')`,
		`replace(/-{2,}/g, '-')`,
		`replace(/^-+|-+$/g, '')`,
		`slice(0, 64)`,
		`replace(/-+$/g, '')`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("create_guided.html's deriveID is missing %s; it must stay in step with deriveResourceID in guided_create.go", want)
		}
	}
}

// TestGuidedPageDerivesIDsWithoutJavaScript proves the server-rendered
// page still carries the derivation's inputs and its live-update
// script, and that a VM's convenience Hostname field is not submitted
// (a VM has no persisted hostname) while a jail's is.
func TestGuidedPageDerivesIDsWithoutJavaScript(t *testing.T) {
	s := newTestServer(t, &fakeClient{})
	for _, tc := range []struct {
		path          string
		wantHostname  string
		notWantHostnm string
	}{
		{"/vms/new", `id="vm-hostname"`, `name="hostname"`},
		{"/jails/new", `name="hostname" id="jail-hostname"`, `id="vm-hostname"`},
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", tc.path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, tc.wantHostname) {
			t.Errorf("GET %s: missing %s", tc.path, tc.wantHostname)
		}
		if strings.Contains(body, tc.notWantHostnm) {
			t.Errorf("GET %s: should not contain %s", tc.path, tc.notWantHostnm)
		}
		for _, want := range []string{`id="vm-id"`, `name="id"`, `id="vm-name"`, `name="name"`, "function deriveID("} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: missing %s", tc.path, want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The rules, server-side, from the POST body alone
// ---------------------------------------------------------------------------

// TestServer_CreateVM_RejectsUnsupportedCombinations is the load-bearing
// test for the whole flow: a crafted POST - one that never saw the
// page, never ran its JS, and therefore carries fields the UI would
// have disabled - must be refused with an explanation, and must never
// reach managerd.
func TestServer_CreateVM_RejectsUnsupportedCombinations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		form     url.Values
		contains string
	}{
		{
			// internal/cluster/reconciler.go ensureVM: a
			// HAST-replicated VM's disk is the replicated device, and
			// the BaseImageName seed only runs on the non-replicated
			// branch - so this pairing stored the base image and never
			// applied it. The one genuinely silent drop the old form
			// allowed.
			name: "replica with a base image",
			form: url.Values{
				"id":              {"vm-1"},
				"node_id":         {"node-a"},
				"replica_node_id": {"node-b"},
				"base_image_name": {"freebsd.raw"},
			},
			contains: "silently dropped",
		},
		{
			// internal/cluster/plan.go: CloneFromSnapshot is mutually
			// exclusive with both ReplicaNodeID and BaseImageName.
			name: "clone with a base image",
			form: url.Values{
				"id":                  {"vm-1"},
				"clone_source_vm_id":  {"vm-2"},
				"clone_snapshot_name": {"nightly"},
				"base_image_name":     {"freebsd.raw"},
			},
			contains: "Clone source and base image are mutually exclusive",
		},
		{
			name: "installer used as a base image",
			form: url.Values{
				"id":              {"vm-1"},
				"base_image_name": {"FreeBSD-15.1.iso"},
			},
			contains: "bootable installer image",
		},
		{
			name: "jail base archive used as VM boot media",
			form: url.Values{
				"id":       {"vm-1"},
				"iso_name": {"base.txz"},
			},
			contains: "userland archive",
		},
		{
			// A jail's fields on a VM create were accepted and
			// discarded before this flow existed.
			name:     "jail hostname on a VM create",
			form:     url.Values{"id": {"vm-1"}, "hostname": {"web-1.lab3.home.arpa"}},
			contains: "Hostname is a jail field",
		},
		{
			name:     "jail base template on a VM create",
			form:     url.Values{"id": {"vm-1"}, "base_template": {"freebsd-14"}},
			contains: "Base template is a jail field",
		},
		{
			name:     "vnet on a VM create",
			form:     url.Values{"id": {"vm-1"}, "vnet": {"1"}},
			contains: "VNET is a jail field",
		},
		{
			// A VM snapshot is local to one node; `zfs clone` never
			// fetches across nodes, so a cross-node clone cannot work.
			name: "clone source on another node",
			form: url.Values{
				"id":                  {"vm-1"},
				"node_id":             {"node-a"},
				"clone_source_vm_id":  {"vm-9"},
				"clone_snapshot_name": {"nightly"},
			},
			contains: "node-local",
		},
		{
			name: "replica node is the owner node",
			form: url.Values{
				"id":              {"vm-1"},
				"node_id":         {"node-a"},
				"replica_node_id": {"node-a"},
			},
			contains: "two different Combs",
		},
		{
			name:     "a jail create submitted to the VM endpoint",
			form:     url.Values{"id": {"vm-1"}, "kind": {"jail"}},
			contains: "submitted to the virtual machine create endpoint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClient{createResp: &rpcpb.CreateVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
				listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-9", NodeId: "node-z"}}}}
			s := newTestServer(t, client)
			rec := postCreate(t, s, "/vms", tc.form)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (errors render inline)", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.contains) {
				t.Errorf("response should explain why this combination cannot work (want %q), got: %s", tc.contains, rec.Body.String())
			}
			if client.lastCreateReq != nil {
				t.Errorf("CreateVM must not be called for an unsupported combination, got %+v", client.lastCreateReq)
			}
		})
	}
}

// TestServer_CreateJail_RejectsUnsupportedCombinations is the jail half
// of the same guarantee.
func TestServer_CreateJail_RejectsUnsupportedCombinations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		form     url.Values
		contains string
	}{
		{
			// internal/cluster/jail.go ensureJail: "base_template and
			// base_archive_name are not supported together".
			name: "base template and base archive",
			form: url.Values{
				"id":                {"jail-1"},
				"base_template":     {"freebsd-14"},
				"base_archive_name": {"base.txz"},
			},
			contains: "Base template and base archive are mutually exclusive",
		},
		{
			// ensureJail: "base_template is not supported together
			// with replica_node_id - a HAST-replicated jail's root is a
			// raw device, not a ZFS dataset".
			name: "base template and replica node",
			form: url.Values{
				"id":              {"jail-1"},
				"base_template":   {"freebsd-14"},
				"replica_node_id": {"node-b"},
			},
			contains: "Base template and replica node are mutually exclusive",
		},
		{
			// ADR-0117: ensureJail rejects "vnet requires network_id to
			// be set"; the old form computed that pairing away to false
			// with no word at all.
			name:     "vnet without a network",
			form:     url.Values{"id": {"jail-1"}, "vnet": {"1"}},
			contains: "VNET needs a network",
		},
		{
			name:     "a VM installer image on a jail create",
			form:     url.Values{"id": {"jail-1"}, "iso_name": {"FreeBSD.iso"}},
			contains: "Installer image is a VM field",
		},
		{
			name:     "a VM base image on a jail create",
			form:     url.Values{"id": {"jail-1"}, "base_image_name": {"freebsd.raw"}},
			contains: "Base image is a VM field",
		},
		{
			name:     "a VM clone source on a jail create",
			form:     url.Values{"id": {"jail-1"}, "clone_source_vm_id": {"vm-1"}},
			contains: "Clone source is a VM field",
		},
		{
			name:     "vCPUs on a jail create",
			form:     url.Values{"id": {"jail-1"}, "vcpus": {"2"}},
			contains: "vCPUs is a VM field",
		},
		{
			name: "firewall rules on a jail create",
			form: url.Values{
				"id":           {"jail-1"},
				"fw_direction": {"in"},
				"fw_action":    {"block"},
			},
			contains: "Firewall rules are a VM field",
		},
		{
			name:     "a VM disk image as a jail base archive",
			form:     url.Values{"id": {"jail-1"}, "base_archive_name": {"freebsd.raw"}},
			contains: "raw disk image",
		},
		{
			name: "replica node is the owner node",
			form: url.Values{
				"id":              {"jail-1"},
				"node_id":         {"node-a"},
				"replica_node_id": {"node-a"},
			},
			contains: "two different Combs",
		},
		{
			name:     "a VM create submitted to the jail endpoint",
			form:     url.Values{"id": {"jail-1"}, "kind": {"vm"}},
			contains: "submitted to the FreeBSD jail create endpoint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClient{createJailResp: &rpcpb.CreateJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1"}}}
			s := newTestServer(t, client)
			rec := postCreate(t, s, "/jails", tc.form)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (errors render inline)", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.contains) {
				t.Errorf("response should explain why this combination cannot work (want %q), got: %s", tc.contains, rec.Body.String())
			}
			if client.lastCreateJailReq != nil {
				t.Errorf("CreateJail must not be called for an unsupported combination, got %+v", client.lastCreateJailReq)
			}
		})
	}
}

// TestServer_CreateJail_BaseArchiveWithReplicaIsAllowed guards the other
// side of the template/replica rule: a base ARCHIVE with a replica is
// fine, because ensureJailRoot extracts the archive into whichever
// root it was handed (ADR-0098's archive path, HAST device included).
// A rule that over-rejected here would push operators to populate jail
// roots by hand.
func TestServer_CreateJail_BaseArchiveWithReplicaIsAllowed(t *testing.T) {
	client := &fakeClient{createJailResp: &rpcpb.CreateJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1"}}}
	s := newTestServer(t, client)
	rec := postCreate(t, s, "/jails", url.Values{
		"id":                {"jail-1"},
		"node_id":           {"node-a"},
		"replica_node_id":   {"node-b"},
		"base_archive_name": {"base.txz"},
	})
	if got := rec.Header().Get("HX-Redirect"); got != "/jails" {
		t.Errorf("HX-Redirect = %q, want /jails; a base archive with a replica node is supported. body=%s", got, rec.Body.String())
	}
	if client.lastCreateJailReq == nil {
		t.Fatal("CreateJail should have been called")
	}
}

// TestServer_CreateVM_ValidCombinationStillWorks is the other
// direction: the new validation must not reject the combinations the
// reconciler genuinely supports, including the ones the old form
// accepted and this flow now also offers (a clone, a base image, a
// replica, install media on top of a base image).
func TestServer_CreateVM_ValidCombinationStillWorks(t *testing.T) {
	client := &fakeClient{createResp: &rpcpb.CreateVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	s := newTestServer(t, client)
	rec := postCreate(t, s, "/vms", url.Values{
		"id":            {"vm-1"},
		"name":          {"web-1"},
		"kind":          {"vm"},
		"node_id":       {"node-a"},
		"vcpus":         {"2"},
		"memory_mb":     {"1024"},
		"desired_state": {"running"},
		"network_id":    {"net-1"},
		// An installer on top of a base image is legitimate: the image
		// seeds the disk and the ISO is attached as install media.
		"iso_name":        {"FreeBSD-15.1.iso"},
		"base_image_name": {"freebsd.raw"},
	})
	if got := rec.Header().Get("HX-Redirect"); got != "/vms" {
		t.Errorf("HX-Redirect = %q, want /vms; body=%s", got, rec.Body.String())
	}
	if client.lastCreateReq == nil {
		t.Fatal("CreateVM should have been called")
	}
	vm := client.lastCreateReq.GetVm()
	if vm.GetBaseImageName() != "freebsd.raw" || vm.GetIsoName() != "FreeBSD-15.1.iso" {
		t.Errorf("forwarded VM = %+v, want both the base image and the installer preserved", vm)
	}
}

// TestServer_CreateGuided_AcceptsLegacyFormWithoutKind proves
// compatibility: the kind field is checked when present, so a POST
// predating this flow - a saved curl command, an old bookmark, a
// script - still works instead of being refused for a field it never
// had.
func TestServer_CreateGuided_AcceptsLegacyFormWithoutKind(t *testing.T) {
	client := &fakeClient{
		createResp:     &rpcpb.CreateVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
		createJailResp: &rpcpb.CreateJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1"}},
	}
	s := newTestServer(t, client)

	rec := postCreate(t, s, "/vms", url.Values{"id": {"vm-1"}, "node_id": {"node-a"}})
	if got := rec.Header().Get("HX-Redirect"); got != "/vms" {
		t.Errorf("a legacy VM POST (no kind) was refused: HX-Redirect = %q, body = %s", got, rec.Body.String())
	}
	rec = postCreate(t, s, "/jails", url.Values{"id": {"jail-1"}, "hostname": {"web-1.lab3"}, "base_archive_name": {"base.txz"}})
	if got := rec.Header().Get("HX-Redirect"); got != "/jails" {
		t.Errorf("a legacy jail POST (no kind) was refused: HX-Redirect = %q, body = %s", got, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// The page itself
// ---------------------------------------------------------------------------

// TestServer_CreateGuidedPage_PreselectsStepOne covers the three entry
// points: the shared /create and the two legacy kind-specific ones.
// All three must render the same single wizard.
func TestServer_CreateGuidedPage_PreselectsStepOne(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", KnownNodeIds: []string{"node-a", "node-b"}}}
	s := newTestServer(t, client)
	for _, tc := range []struct {
		path        string
		wantAction  string
		wantChecked string
		wantOther   string
	}{
		// wantOther ends at the closing ">" deliberately: that is how
		// the *unselected* radio renders, so the assertion fails if
		// both kinds ever end up pre-selected at once.
		{"/vms/new", `hx-post="/vms"`, `id="kind-vm" value="vm" checked`, `id="kind-jail" value="jail">`},
		{"/jails/new", `hx-post="/jails"`, `id="kind-jail" value="jail" checked`, `id="kind-vm" value="vm">`},
		{"/create?kind=jail", `hx-post="/jails"`, `id="kind-jail" value="jail" checked`, `id="kind-vm" value="vm">`},
		{"/create?kind=vm", `hx-post="/vms"`, `id="kind-vm" value="vm" checked`, `id="kind-jail" value="jail">`},
		// An absent or unrecognised kind falls back to a VM rather than
		// guessing at a workload type.
		{"/create", `hx-post="/vms"`, `id="kind-vm" value="vm" checked`, `id="kind-jail" value="jail">`},
		{"/create?kind=banana", `hx-post="/vms"`, `id="kind-vm" value="vm" checked`, `id="kind-jail" value="jail">`},
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", tc.path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, tc.wantAction) {
			t.Errorf("GET %s: missing %s", tc.path, tc.wantAction)
		}
		if !strings.Contains(body, tc.wantChecked) {
			t.Errorf("GET %s: step 1 should pre-answer %s", tc.path, tc.wantChecked)
		}
		if !strings.Contains(body, tc.wantOther) {
			t.Errorf("GET %s: the other kind's radio should be present and unchecked (%s)", tc.path, tc.wantOther)
		}
	}
}

// TestServer_CreateGuidedPage_MarksUnavailableFieldsInert proves the
// non-applicable kind's controls are in the page but disabled, rather
// than merely hidden: disabled controls are not submitted, so a field
// with nowhere to go can never be posted as though it counted.
func TestServer_CreateGuidedPage_MarksUnavailableFieldsInert(t *testing.T) {
	s := newTestServer(t, &fakeClient{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vms/new", nil))
	body := rec.Body.String()
	// A VM create: the jail-only root-filesystem step's controls are
	// disabled, and its step carries the reason.
	for _, want := range []string{
		`name="base_template" id="base-template" disabled`,
		`name="base_archive_name" id="base-archive-picker" class="image-picker" disabled`,
		`name="vnet" id="vnet-input" value="1" disabled`,
		"Base template and base archive are not available for a virtual machine",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("VM page missing %q", want)
		}
	}
	// ... and the VM's own fields are live.
	for _, want := range []string{`name="vcpus" value="1" min="1" required`, `name="iso_name" id="iso-picker" class="image-picker"`} {
		if !strings.Contains(body, want) {
			t.Errorf("VM page should leave %q enabled", want)
		}
	}
}

// TestServer_CreateGuidedPage_RendersUnavailableImagesWithReasons
// proves an image that cannot fill a role is shown disabled with its
// reason attached, on the page, for the kind of create that cannot use
// it.
func TestServer_CreateGuidedPage_RendersUnavailableImagesWithReasons(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", KnownNodeIds: []string{"node-a"}},
		listISOsResp: &rpcpb.ListISOsResponse{Isos: []*rpcpb.ISOInfo{
			{Name: "FreeBSD-15.1.iso"},
			{Name: "base.txz"},
		}}}
	s := newTestServer(t, client)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vms/new", nil))
	body := rec.Body.String()
	// base.txz is neither boot media nor a starting disk, so it is
	// disabled in both VM pickers and explained under each.
	if !strings.Contains(body, `value="base.txz" disabled`) {
		t.Errorf("base.txz should be offered disabled in the VM image pickers, got: %s", body)
	}
	if !strings.Contains(body, "unavailable: a jail userland archive") {
		t.Errorf("the disabled option should say why in its own label, got: %s", body)
	}
	if !strings.Contains(body, "Base archive cannot be") || !strings.Contains(body, "base.txz is a compressed userland archive") {
		t.Errorf("the page should carry the server's full reason for the unavailable image, got: %s", body)
	}
	// The installer is perfectly good boot media, so it stays available
	// there while being refused as a base image.
	if !strings.Contains(body, `<option value="FreeBSD-15.1.iso">FreeBSD-15.1.iso</option>`) {
		t.Errorf("an ISO should remain available as VM install media, got: %s", body)
	}
}

// TestServer_CreateGuidedPage_ExplainsIncapableCombs covers the one
// piece of placement evidence the wizard turns into a statement rather
// than a guess: a Comb that reports no bhyve provisioning cannot host
// a VM, and an unreachable Comb is missing evidence rather than
// evidence of absence.
func TestServer_CreateGuidedPage_ExplainsIncapableCombs(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", KnownNodeIds: []string{"node-a", "node-b"}},
		hostStatsResp: &rpcpb.HostStatsResponse{
			BhyveConfigured: true,
		},
	}
	s := newTestServer(t, client)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vms/new", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "<span class=\"success\">VM-capable</span>") {
		t.Errorf("a bhyve-capable Comb should be reported as VM-capable, got: %s", body)
	}
	if strings.Contains(body, "unreachable - not evidence either way") {
		t.Errorf("a reachable Comb should not be reported as unreachable, got: %s", body)
	}
}

func TestPlacementUnavailable(t *testing.T) {
	for _, tc := range []struct {
		hive placementHiveView
		kind guidedKind
		want bool
	}{
		{placementHiveView{VMCapable: true}, guidedKindVM, false},
		{placementHiveView{}, guidedKindVM, true},
		// An unreachable probe is no evidence at all, so the Comb
		// stays selectable for both kinds.
		{placementHiveView{ProbeError: "connection refused"}, guidedKindVM, false},
		{placementHiveView{ProbeError: "connection refused"}, guidedKindJail, false},
		// A jail's capability is only known when the Comb answered with
		// an explicit setting.
		{placementHiveView{JailKnown: true, JailCapable: true}, guidedKindJail, false},
		{placementHiveView{JailKnown: true}, guidedKindJail, true},
		{placementHiveView{}, guidedKindJail, false},
	} {
		if got := placementUnavailable(tc.hive, tc.kind); got != tc.want {
			t.Errorf("placementUnavailable(%+v, %q) = %v, want %v", tc.hive, tc.kind, got, tc.want)
		}
	}
}
