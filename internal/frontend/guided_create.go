package frontend

import (
	"fmt"
	"net/http"
	"strings"
)

// This file is the server-side half of the guided creation flow - the
// single wizard that replaced the separate new-VM and new-Jail pages
// (web/templates/create_guided.html). The browser half of the same
// rules lives in that template's own JS; the two halves deliberately
// share one source of truth per rule: the image-role rules below are
// rendered into the page by this same code (guidedImageOptions), so
// they cannot disagree, and the cascade rules below are restated in the
// template's prose only as explanations - never as an independent
// decision the UI makes on its own. guided_create_test.go pins the
// shared hostname -> ID derivation against the template's copy of it.
//
// Why server-side validation exists at all when the form already
// disables unsupported controls: a disabled control is a UI
// affordance, not a guarantee. Every rule below is checkable from the
// POST body alone, so a crafted (or merely JS-less, or replayed) form
// submission is rejected with the same human-readable reason the UI
// would have shown, rather than reaching managerd and failing there
// with a lower-level reconciler message - or, worse, being silently
// accepted with a field quietly dropped.
//
// The rules themselves are not new policy. Each one restates a
// constraint already enforced further down the stack; the comment on
// each cites where it comes from, so nobody has to take the frontend's
// word for it.
//
//	internal/cluster/plan.go VMPlacement.CloneFromSnapshot:
//	  "Mutually exclusive with both ReplicaNodeID and
//	  BaseImageName", and ADR-0084/ADR-0090: a clone (like a base
//	  template) is node-local.
//	internal/cluster/reconciler.go ensureVM's diskPath choice: a
//	  HAST-replicated VM's disk is a raw device from
//	  hastDevicePaths, and the BaseImageName seed only runs on the
//	  non-HAST branch - so a base image named alongside a replica node
//	  is stored, shown, and never applied.
//	internal/cluster/reconciler.go ensureVM: a VM snapshot must
//	  exist on the VM's own node.
//	internal/cluster/plan.go JailPlacement.BaseArchiveName:
//	  "A jail names BaseTemplate or BaseArchiveName, not both".
//	internal/cluster/jail.go ensureJail: base_template is rejected
//	  with replica_node_id, and vnet is rejected without network_id
//	  (ADR-0117).
//	internal/cluster/plan.go JailPlacement: jails have "no
//	  vcpus/memory/ISO/network/firewall fields" - a VM create
//	  carrying jail-only fields (or vice versa) is likewise a
//	  silently dropped field.

// guidedKind is which of the two workload types a creation request is
// for. The flow is one page; this is step 1's answer, and it decides
// both which steps are reachable and which fields are legal.
type guidedKind string

const (
	guidedKindVM   guidedKind = "vm"
	guidedKindJail guidedKind = "jail"
)

// createAction is the existing POST endpoint this kind is created
// through. The guided flow changed the FORM, not the RPC contract or
// its HTTP surface: CreateVM/CreateJail and POST /vms, POST /jails are
// exactly as they were, so anything already scripted against them
// keeps working.
func (k guidedKind) createAction() string {
	if k == guidedKindJail {
		return "/jails"
	}
	return "/vms"
}

// other returns the other kind - used by the page to decide which
// step blocks to mark unavailable from the selected kind.
func (k guidedKind) other() guidedKind {
	if k == guidedKindJail {
		return guidedKindVM
	}
	return guidedKindJail
}

// parseGuidedKind maps a request's ?kind= value (or the kind field of
// a submitted form) to a kind. An absent or unrecognised value yields
// "", never a guess: the caller decides what to do with an unknown
// kind, and a POST carrying one is rejected rather than assumed.
func parseGuidedKind(s string) guidedKind {
	switch guidedKind(strings.ToLower(strings.TrimSpace(s))) {
	case guidedKindVM:
		return guidedKindVM
	case guidedKindJail:
		return guidedKindJail
	default:
		return ""
	}
}

// imageKind is the coarse classification of a stored image, derived
// from its name alone. Apiary's ISOInfo carries no type/kind field
// (only Name/SizeBytes/Sha256 - see api/rpc/manager.proto), so the
// name's extension is the only signal available at this layer; the
// same convention the rest of the tree already uses (the freebsdimg
// catalogue ships *.raw VM images alongside *.iso installers, and
// ADR-0098 calls a jail's base.txz a "base archive").
//
// unknown is a first-class, deliberately generous bucket: anything
// this classifier does not recognise stays selectable everywhere, so
// an oddly-named but perfectly valid image is never locked out. The
// rules only fire on an image this code positively recognises as the
// wrong kind.
type imageKind int

const (
	imageKindUnknown imageKind = iota
	imageKindInstaller
	imageKindDisk
	imageKindArchive
)

// String names a kind for the page's own prose.
func (k imageKind) String() string {
	switch k {
	case imageKindInstaller:
		return "installer image"
	case imageKindDisk:
		return "raw disk image"
	case imageKindArchive:
		return "userland archive"
	default:
		return "image of unrecognised type"
	}
}

// installerImageExtensions names a bootable installer/optical image.
// Only ".iso" is classified: other optical-ish extensions are left as
// unknown rather than guessed at.
var installerImageExtensions = []string{".iso"}

// diskImageExtensions names a raw disk image that can seed a VM's
// starting disk (bhyve base image, ADR-0031), and - see
// guidedRoleBootMedia - also a raw bootable image bhyve attaches as
// install media.
var diskImageExtensions = []string{".raw", ".qcow2", ".vmdk", ".vdi", ".vhd"}

// archiveImageExtensions names a compressed userland archive, the
// shape ADR-0098's jail base.txz takes. ".tar" rather than
// ".tar.gz"/".tar.xz": imageCompressionSuffixes is stripped first, so
// the bare extension is what a compressed tarball is left with.
var archiveImageExtensions = []string{".txz", ".tbz", ".tgz", ".tar"}

// imageCompressionSuffixes are stripped before the final extension is
// examined, so "FreeBSD-15.1-amd64-zfs.raw.xz" classifies as a disk
// image rather than falling through to unknown.
var imageCompressionSuffixes = []string{".xz", ".gz", ".bz2", ".zst"}

// classifyImageName returns the imageKind implied by name's final
// extension, ignoring any trailing compression suffix. Case is
// ignored. An unrecognised name is imageKindUnknown.
func classifyImageName(name string) imageKind {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return imageKindUnknown
	}
	for {
		matched := false
		for _, suffix := range imageCompressionSuffixes {
			if strings.HasSuffix(lower, suffix) && len(lower) > len(suffix) {
				lower = strings.TrimSuffix(lower, suffix)
				matched = true
				break
			}
		}
		if !matched {
			break
		}
	}
	switch {
	case hasAnySuffix(lower, installerImageExtensions):
		return imageKindInstaller
	case hasAnySuffix(lower, diskImageExtensions):
		return imageKindDisk
	case hasAnySuffix(lower, archiveImageExtensions):
		return imageKindArchive
	default:
		return imageKindUnknown
	}
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// guidedImageRole is one image picker's job in the flow. The same
// value drives both halves of the feature: guidedImageOptions renders
// the <option disabled> + reason into the page, and the POST
// validators reject the same choice with the same sentence. Adding a
// role therefore cannot produce a control the server disagrees with.
//
// allowed lists the kinds this role can accept; unknown is in every
// role's set on purpose (see imageKind's own comment). why is keyed by
// the excluded kind only, and each entry is a Go format string taking
// the image's name: cue is the short suffix appended to the disabled
// option's own label, why is the full sentence shown in the error
// banner when a POST carries that combination anyway.
type guidedImageRole struct {
	label   string
	allowed []imageKind
	cue     map[imageKind]string
	why     map[imageKind]string
}

var (
	// guidedRoleBootMedia is the VM's "Installer image" picker. A disk
	// image is deliberately allowed here: internal/cluster's ensureVM
	// sniffs ISOName for a real ISO9660 filesystem and, when there
	// isn't one, attaches the file as a raw install disk instead
	// (reconciler.go's "most likely a FreeBSD memstick image" branch) -
	// which is precisely the supported way to install from a .raw. Only
	// an archive is nonsense in this slot: bhyve has no way to boot or
	// attach a .txz userland tarball.
	guidedRoleBootMedia = guidedImageRole{
		label:   "Installer image",
		allowed: []imageKind{imageKindInstaller, imageKindDisk, imageKindUnknown},
		cue: map[imageKind]string{
			imageKindArchive: "a jail userland archive, not boot media",
		},
		why: map[imageKind]string{
			imageKindArchive: "%[1]s is a compressed userland archive (.txz-style): that is extracted into a jail's root filesystem, and bhyve has no way to boot or attach one. Use it as the base archive on a jail instead",
		},
	}

	// guidedRoleBaseImage is the VM's "Base image" picker, copied into
	// this VM's own starting disk (ADR-0031). An installer is excluded
	// because ensureDiskImage copies the file verbatim into the disk
	// slot: the VM would be handed a CD-ROM filesystem where a
	// bootable disk is expected, with no check that would catch it.
	guidedRoleBaseImage = guidedImageRole{
		label:   "Base image",
		allowed: []imageKind{imageKindDisk, imageKindUnknown},
		cue: map[imageKind]string{
			imageKindInstaller: "boot media, not a starting disk",
			imageKindArchive:   "a jail userland archive, not a disk",
		},
		why: map[imageKind]string{
			imageKindInstaller: "%[1]s is a bootable installer image. An installer is attached to a VM as install media, not copied in as its starting disk - a CD-ROM filesystem is not a disk bhyve can boot from. Leave the installer field set and the base image as None instead",
			imageKindArchive:   "%[1]s is a compressed userland archive (.txz-style), which is extracted into a jail's root filesystem rather than a VM's disk. Use it as the base archive on a jail instead",
		},
	}

	// guidedRoleBaseArchive is the jail's "Base archive" picker,
	// extracted into an empty jail root (ADR-0098). A disk image is
	// excluded because a jail's root is a directory tree, not a block
	// device: there is nothing to copy a .raw into.
	guidedRoleBaseArchive = guidedImageRole{
		label:   "Base archive",
		allowed: []imageKind{imageKindArchive, imageKindUnknown},
		cue: map[imageKind]string{
			imageKindInstaller: "installer media, not a userland archive",
			imageKindDisk:      "a VM disk image, not a userland archive",
		},
		why: map[imageKind]string{
			imageKindInstaller: "%[1]s is a bootable installer image. A jail's root is populated by extracting a userland archive into it, never by attaching install media. Use %[1]s as a VM's installer image instead",
			imageKindDisk:      "%[1]s is a raw disk image. A jail's root is a directory tree, not a block device, so there is nothing to copy it into. Use %[1]s as a VM's base image instead",
		},
	}
)

// accepts reports whether kind can fill this role.
func (r guidedImageRole) accepts(kind imageKind) bool {
	for _, ok := range r.allowed {
		if kind == ok {
			return true
		}
	}
	return false
}

// verdict reports whether name can fill this role and, when it cannot,
// the reason to show. An unrecognised kind is always accepted.
func (r guidedImageRole) verdict(name string) (bool, string) {
	kind := classifyImageName(name)
	if r.accepts(kind) {
		return true, ""
	}
	why, ok := r.why[kind]
	if !ok {
		return false, fmt.Sprintf("%s %q is a %s, which this field cannot use", r.label, name, kind)
	}
	return false, fmt.Sprintf("%s cannot be %q: %s.", r.label, name, fmt.Sprintf(why, name))
}

// guidedImageOption is one rendered <option> in an image picker.
// Available=false options are rendered disabled, with Reason as the
// label suffix and (via the picker's own field-help) the explanation -
// so an operator can see why an image they can see is not offered.
type guidedImageOption struct {
	Value     string
	Label     string
	Available bool
	Reason    string
}

// guidedImageOptions builds one picker's options from the cluster-wide
// image list, applying role to each entry. Images are listed even when
// unavailable: hiding them would leave an operator who knows the image
// is there with no explanation, which is the failure this flow exists
// to close.
func guidedImageOptions(rows []isoRowView, role guidedImageRole) []guidedImageOption {
	opts := make([]guidedImageOption, 0, len(rows))
	for _, row := range rows {
		ok, reason := role.verdict(row.Name)
		label := row.Name
		if !ok {
			label += " — unavailable: " + cueFor(row.Name, role)
		}
		opts = append(opts, guidedImageOption{Value: row.Name, Label: label, Available: ok, Reason: reason})
	}
	return opts
}

// cueFor is the short, label-sized form of the same verdict - the long
// sentence is for the error banner, where there is room to read it.
func cueFor(name string, role guidedImageRole) string {
	if cue, ok := role.cue[classifyImageName(name)]; ok {
		return cue
	}
	return "wrong type for this field"
}

// resourceIDMaxLen mirrors validResourceID's own 64-character ceiling
// (internal/raft/fsm.go), and is the number the template's JS
// derivation slices to. See deriveResourceID.
const resourceIDMaxLen = 64

// deriveResourceID is the Go half of the Hostname -> ID derivation
// every create form has always had. It is here, not only in the
// template, so the rule is unit-testable: the JS in
// create_guided.html exists only so the field updates as you type, and
// guided_create_test.go pins the two against each other by asserting
// the template's own copy carries this exact character class and this
// exact limit.
//
// A dotted FQDN is not a valid resource ID (validResourceID allows
// alphanumerics, '-' and '_' only, max 64 characters), so
// "sting-vm-1.lab3.home.arpa" must become "sting-vm-1-lab3-home-arpa"
// rather than being submitted verbatim and rejected deeper in the
// stack.
func deriveResourceID(hostname string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(hostname) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		case r == '-':
			// Collapse a run of separators into one dash, and drop
			// leading ones, exactly as the JS /-{2,}/ and /^-+/ passes
			// do; trailing ones are trimmed once at the end.
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
		// The loop stops at validResourceID's own ceiling, so no slug
		// longer than resourceIDMaxLen can come out of here - which is
		// what the JS's slice(0, 64) guarantees on its side.
		if b.Len() >= resourceIDMaxLen {
			break
		}
	}
	// A slice that landed on a separator is trimmed rather than left
	// dangling, exactly as the JS's trailing-dash replace does.
	return strings.TrimRight(b.String(), "-")
}

// vmCreateForm is a VM creation POST reduced to exactly the values the
// availability rules care about. Keeping it a plain struct (rather
// than validating url.Values in place) is what makes the rules
// unit-testable without an http.Request.
type vmCreateForm struct {
	// Kind is step 1's answer, submitted with the form. It is checked
	// against the endpoint rather than ignored, so the radio group can
	// never be quietly dropped.
	Kind              string
	NodeID            string
	ReplicaNodeID     string
	ISOName           string
	BaseImageName     string
	CloneSourceVMID   string
	CloneSnapshotName string

	// Jail-only fields. A VM create must never carry them; see
	// validateVMCreateForm.
	VnetValue       string
	HostnameValue   string
	BaseTemplate    string
	BaseArchiveName string
}

// vmCreateFormFromRequest extracts a vmCreateForm from an already
// ParseForm'd request.
func vmCreateFormFromRequest(r *http.Request) vmCreateForm {
	return vmCreateForm{
		Kind:              r.FormValue("kind"),
		NodeID:            r.FormValue("node_id"),
		ReplicaNodeID:     r.FormValue("replica_node_id"),
		ISOName:           r.FormValue("iso_name"),
		BaseImageName:     r.FormValue("base_image_name"),
		CloneSourceVMID:   r.FormValue("clone_source_vm_id"),
		CloneSnapshotName: r.FormValue("clone_snapshot_name"),
		VnetValue:         r.FormValue("vnet"),
		HostnameValue:     r.FormValue("hostname"),
		BaseTemplate:      r.FormValue("base_template"),
		BaseArchiveName:   r.FormValue("base_archive_name"),
	}
}

// cloneFromSnapshot combines the two cascading dropdowns exactly as
// handleCreateVM has always done: both or neither, so a half-filled
// pair (only reachable without JS) is treated as "not requested"
// rather than a malformed value.
func (f vmCreateForm) cloneFromSnapshot() string {
	if f.CloneSourceVMID == "" || f.CloneSnapshotName == "" {
		return ""
	}
	return f.CloneSourceVMID + "@" + f.CloneSnapshotName
}

// validateVMCreateForm applies every VM availability rule. The first
// failure wins; the message is written to be shown verbatim in the
// form's own error banner, so it names the offending field and says
// why the combination cannot work.
//
// cloneSourceNodeID is the node the source VM actually lives on,
// resolved by the caller from the cluster's own VM list. Empty means
// "could not be determined" - the node-locality rule is then skipped
// rather than guessed at, and internal/cluster's reconciler still
// rejects the clone at provisioning time with its own message.
func (f vmCreateForm) validateVMCreateForm(cloneSourceNodeID string) error {
	if err := checkSubmittedKind(f.Kind, guidedKindVM); err != nil {
		return err
	}

	// Jail-only fields on a VM create. Previously these were accepted
	// and silently discarded, which is exactly the "never silently drop
	// a field" failure the guided flow exists to close.
	if f.HostnameValue != "" {
		return fmt.Errorf("Hostname is a jail field: a VM has no persisted hostname field, so %q would be silently dropped. "+
			"Remove it, or create a jail instead (jail(8) sets the hostname inside the jail; a VM's is set by its image or DHCP)", f.HostnameValue)
	}
	if f.BaseTemplate != "" {
		return fmt.Errorf("Base template is a jail field: it names a ZFS template dataset on the owning Comb to clone a jail root from (ADR-0084), and a VM has no jail root. " +
			"Use Base image for a VM")
	}
	if f.BaseArchiveName != "" {
		return fmt.Errorf("Base archive is a jail field: a base.txz-style userland archive populates a jail root (ADR-0098), and a VM boots from a disk image instead. " +
			"Use Base image for a VM")
	}
	if f.VnetValue != "" {
		return fmt.Errorf("VNET is a jail field: a VM always gets its own tap interface on the selected network, so there is nothing to opt in to. " +
			"Remove it, or create a jail instead")
	}

	// HAST replication is between two distinct Combs; naming this
	// VM's own node as the replica target is not a redundancy choice,
	// it is a request to replicate a dataset onto itself.
	if f.ReplicaNodeID != "" && f.NodeID != "" && f.ReplicaNodeID == f.NodeID {
		return fmt.Errorf("Replica node %q is the same node as the owner node %q: HAST replication runs between two different Combs, so it cannot replicate a disk onto the node already holding it. "+
			"Pick a different replica node, or leave it as None", f.ReplicaNodeID, f.NodeID)
	}

	// ensureVM picks the VM's disk from ReplicaNodeID first: a
	// replicated VM's disk is the HAST device path, and the
	// BaseImageName seed (a verbatim copy of the image over the disk
	// file) only runs on the other, non-replicated branch. Nothing
	// rejects this pairing - the base image is stored, shown on the VM
	// page, and never applied. That is the exact failure the flow
	// exists to prevent, so it is rejected here with the reason.
	if f.ReplicaNodeID != "" && f.BaseImageName != "" {
		return fmt.Errorf("Replica node and base image are mutually exclusive: a HAST-replicated VM's disk is the replicated device itself, and the base image is only copied onto a local disk, so %q would be silently dropped. "+
			"Pick one way to seed this VM's disk - a HAST-replicated blank disk, or a local disk seeded from a base image", f.BaseImageName)
	}

	cloning := f.cloneFromSnapshot() != ""
	if cloning {
		// Wording preserved from the pre-existing check in
		// handleCreateVM (see TestServer_CreateVM_RejectsCloneWithReplica).
		if f.ReplicaNodeID != "" {
			return fmt.Errorf("clone source and replica node are mutually exclusive: a HAST-replicated VM cannot be cloned from a snapshot")
		}
		// internal/cluster/plan.go: CloneFromSnapshot is "Mutually
		// exclusive with both ReplicaNodeID and BaseImageName" - a
		// cloned dataset already has its contents, so a base image
		// could never be applied on top of it.
		if f.BaseImageName != "" {
			return fmt.Errorf("Clone source and base image are mutually exclusive: a clone already has the source snapshot's contents, so %q could never be copied in on top of it. "+
				"Pick one way to seed this VM's disk - a clone, a base image, or a blank disk", f.BaseImageName)
		}
		// ADR-0090/ADR-0095: a VM snapshot is node-local and
		// `zfs clone` is a local operation, so the source VM must
		// live on the same node as this one. Unlike the image
		// pickers, there is no cross-node fetch to fall back on.
		if f.NodeID != "" && cloneSourceNodeID != "" && cloneSourceNodeID != f.NodeID {
			return fmt.Errorf("Clone source %q lives on node %q but this VM is being created on %q: a VM snapshot is node-local (ADR-0090) and `zfs clone` runs on one node only, with no cross-node fetch. "+
				"Either create this VM on %q, or clear the clone source", f.CloneSourceVMID, cloneSourceNodeID, f.NodeID, cloneSourceNodeID)
		}
	}

	// The two image pickers take different roles from the same store
	// (ImageRoleISO vs ImageRoleBaseImage, internal/manager/server.go)
	// and are applied differently by ensureVM, so an image of the wrong
	// shape for its role is rejected here rather than provisioned into
	// something that cannot boot.
	if f.ISOName != "" {
		if _, reason := guidedRoleBootMedia.verdict(f.ISOName); reason != "" {
			return fmt.Errorf("%s", reason)
		}
	}
	if f.BaseImageName != "" {
		if _, reason := guidedRoleBaseImage.verdict(f.BaseImageName); reason != "" {
			return fmt.Errorf("%s", reason)
		}
	}
	return nil
}

// jailCreateForm is a jail creation POST reduced to the values the
// availability rules care about.
type jailCreateForm struct {
	// Kind is step 1's answer - see vmCreateForm.Kind.
	Kind             string
	Hostname         string
	NodeID           string
	ReplicaNodeID    string
	BaseTemplate     string
	BaseArchiveName  string
	NetworkID        string
	Vnet             string
	ISOName          string
	BaseImageName    string
	CloneSourceVMID  string
	DesiredState     string
	VCPUs            string
	MemoryMB         string
	FirewallRuleRows int
}

func jailCreateFormFromRequest(r *http.Request) jailCreateForm {
	return jailCreateForm{
		Kind:             r.FormValue("kind"),
		Hostname:         r.FormValue("hostname"),
		NodeID:           r.FormValue("node_id"),
		ReplicaNodeID:    r.FormValue("replica_node_id"),
		BaseTemplate:     r.FormValue("base_template"),
		BaseArchiveName:  r.FormValue("base_archive_name"),
		NetworkID:        r.FormValue("network_id"),
		Vnet:             r.FormValue("vnet"),
		ISOName:          r.FormValue("iso_name"),
		BaseImageName:    r.FormValue("base_image_name"),
		CloneSourceVMID:  r.FormValue("clone_source_vm_id"),
		DesiredState:     r.FormValue("desired_state"),
		VCPUs:            r.FormValue("vcpus"),
		MemoryMB:         r.FormValue("memory_mb"),
		FirewallRuleRows: len(r.PostForm["fw_direction"]),
	}
}

// validateJailCreateForm applies every jail availability rule.
func (f jailCreateForm) validateJailCreateForm() error {
	if err := checkSubmittedKind(f.Kind, guidedKindJail); err != nil {
		return err
	}

	// VM-only fields on a jail create: previously accepted and
	// silently discarded. See internal/cluster/plan.go's JailPlacement
	// ("no vcpus/memory/ISO/network/firewall fields") for why a jail
	// has nowhere to put them.
	switch {
	case f.ISOName != "":
		return fmt.Errorf("Installer image is a VM field: jails have no disk to attach install media to, so %q would be silently dropped", f.ISOName)
	case f.BaseImageName != "":
		return fmt.Errorf("Base image is a VM field: a jail's root is populated from a base template or a base archive, not from a raw disk image, so %q would be silently dropped", f.BaseImageName)
	case f.CloneSourceVMID != "":
		return fmt.Errorf("Clone source is a VM field: `zfs clone` from a VM snapshot seeds a VM's disk dataset, and a jail's root is a separate jail(8) dataset, so this would be silently dropped")
	case f.DesiredState != "":
		return fmt.Errorf("Initial state is a VM field: a jail is created stopped and started from its own lifecycle control afterwards, so %q would be silently dropped", f.DesiredState)
	case f.VCPUs != "":
		return fmt.Errorf("vCPUs is a VM field: a jail shares the owning Comb's kernel and CPU, so it has no vCPU count of its own")
	case f.MemoryMB != "":
		return fmt.Errorf("Memory is a VM field: a jail shares the owning Comb's memory, so it has no memory limit of its own")
	case f.FirewallRuleRows > 0:
		return fmt.Errorf("Firewall rules are a VM field: a jail uses the owning Comb's own pf ruleset rather than a per-jail one, so these would be silently dropped")
	}

	if f.ReplicaNodeID != "" && f.NodeID != "" && f.ReplicaNodeID == f.NodeID {
		return fmt.Errorf("Replica node %q is the same node as the owner node %q: HAST replication runs between two different Combs, so it cannot replicate a root onto the node already holding it. "+
			"Pick a different replica node, or leave it as None", f.ReplicaNodeID, f.NodeID)
	}

	// internal/cluster/jail.go ensureJail rejects this pairing by name:
	// "base_template and base_archive_name are not supported together -
	// choose one way to populate this jail's root".
	if f.BaseTemplate != "" && f.BaseArchiveName != "" {
		return fmt.Errorf("Base template and base archive are mutually exclusive: they are two different ways to populate an empty jail root, and only one can apply. "+
			"Set base template %q or base archive %q, not both", f.BaseTemplate, f.BaseArchiveName)
	}
	// ... and this one: "base_template is not supported together with
	// replica_node_id - a HAST-replicated jail's root is a raw device,
	// not a ZFS dataset". A base ARCHIVE with a replica node is fine:
	// ensureJailRoot extracts the archive into whichever root it was
	// given, HAST device included.
	if f.BaseTemplate != "" && f.ReplicaNodeID != "" {
		return fmt.Errorf("Base template and replica node are mutually exclusive: a base template is a ZFS template dataset on the owning Comb that the jail root is cloned from, and a HAST-replicated root is a raw device that has no template to clone from. " +
			"Use a base archive with a replica node (it is extracted into the new root either way), or drop the replica node")
	}
	// ADR-0117: ensureJail rejects "vnet requires network_id to be
	// set". The form used to compute that pairing away to false without
	// a word, so the operator's choice vanished with no explanation.
	if f.Vnet != "" && f.NetworkID == "" {
		return fmt.Errorf("VNET needs a network: it wires this jail's own network stack to a named managed network's bridge via a private epair(4) pair, so there is nothing for it to attach to without one. " +
			"Pick a network, or uncheck VNET to keep the default ip4=inherit networking")
	}
	if f.BaseArchiveName != "" {
		if _, reason := guidedRoleBaseArchive.verdict(f.BaseArchiveName); reason != "" {
			return fmt.Errorf("%s", reason)
		}
	}
	return nil
}

// checkSubmittedKind rejects a submission whose step-1 answer
// disagrees with the endpoint it was POSTed to. The radio group is a
// real control with a real value; letting it disagree with the target
// would mean the page and the request had diverged, and honouring
// either one alone would be a guess about which the operator meant.
// An absent kind is accepted, so every pre-existing script and
// bookmarked POST that predates the guided flow keeps working.
func checkSubmittedKind(submitted string, target guidedKind) error {
	kind := parseGuidedKind(submitted)
	if kind == "" {
		return nil
	}
	if kind != target {
		return fmt.Errorf("This form says it is creating a %s, but it was submitted to the %s create endpoint. "+
			"Reload the page and pick the workload type again - the two halves of the form have to agree",
			kind.display(), target.display())
	}
	return nil
}

// display names a kind for prose.
func (k guidedKind) display() string {
	if k == guidedKindJail {
		return "FreeBSD jail"
	}
	return "virtual machine"
}

// cloneSourceNodeID reports which node the source VM named by
// cloneSourceVMID currently lives on, or "" when it cannot be
// determined. A VM list fetch failure (or a source VM that isn't in
// the list at all) is deliberately not an error here: the point is to
// catch a mismatch the UI could have caught, not to replace the
// reconciler's own authoritative check.
func (s *Server) cloneSourceNodeID(r *http.Request, cloneSourceVMID, cloneSnapshotName string) string {
	if cloneSourceVMID == "" || cloneSnapshotName == "" {
		return ""
	}
	vms, _ := s.currentVMs(r, "id", "asc")
	for _, vm := range vms {
		if vm.ID == cloneSourceVMID {
			return vm.NodeID
		}
	}
	return ""
}
