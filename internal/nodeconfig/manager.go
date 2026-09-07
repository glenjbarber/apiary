// Package nodeconfig persists a broad set of node-local runtime
// settings - most of managerd's own startup flags (see ADR-0070) - as a
// plain JSON file on disk. Physical, per-node data like
// internal/isostore/internal/hoststats - none of this is meaningful to
// any node but the one it's set on, so it is never replicated through
// raft. A change here takes effect the next time this node's managerd
// restarts, not live - see ADR-0049/ADR-0070.
//
// Three categories of field get non-default treatment, all enforced in
// Save/the manager.Server RPC layer above it, not just the web UI:
//
//   - Secrets (PeerAPIKey, RaftdToken) are write-only from the caller's
//     perspective - internal/manager.Server.GetNodeConfig never returns
//     their value, only whether one is set. Save persists them as
//     plain text in this file, which is why DefaultPath is now written
//     0600, not 0644 - this file can hold a live credential, the exact
//     class of exposure ADR-0067 already found and fixed once for
//     /etc/rc.conf itself.
//   - Resource-scope paths (ZFSBase, JailPrefix, BhyvePrefix, ISODir,
//     JailMountBase) are write-once through this package: once a
//     non-empty value has been saved, Save rejects any different
//     non-empty value for that same field. Changing one of these after
//     real resources already exist under the old value doesn't move
//     them - it just makes the reconciler stop seeing them, silently
//     orphaning real VMs/jails/ISOs. This is deliberate friction, not a
//     missing feature - see ADR-0070.
//   - node_id, rpc_addr, and raftd_socket are not represented in this
//     package at all - raft identity and internal wiring stay
//     CLI/rc.conf-only, never settable through a running instance.
package nodeconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// DefaultPath is where the settings file lives by default on a
// pkg-installed FreeBSD system, alongside internal/isostore's own
// /var/db/apiary/isos convention.
const DefaultPath = "/var/db/apiary/node-config.json"

// Config is the full set of node-local settings this package manages.
// Every field's zero value means "use the flag-provided default" - see
// cmd/managerd's own startup wiring. Grouped to mirror
// cmd/managerd/main.go's own flag declaration order.
type Config struct {
	// Uplink mirrors -vlan-uplink: the physical interface VLAN-tagged
	// networks attach to.
	Uplink string `json:"uplink,omitempty"`

	// NATUplink mirrors -nat-uplink: the interface a self-hosted
	// network's outbound NAT egresses through (ADR-0048).
	NATUplink string `json:"nat_uplink,omitempty"`

	// DNSServer mirrors -dhcp-dns-server: the resolver address handed to
	// DHCP clients on Apiary-managed networks. It is node-local because
	// each Hive can have a different reachable resolver.
	DNSServer string `json:"dhcp_dns_server,omitempty"`

	// ZFSBase mirrors -zfs-base. Write-once (see package doc comment) -
	// existing datasets under the old base would be silently orphaned,
	// not moved, by changing this later.
	ZFSBase string `json:"zfs_base,omitempty"`

	// ReconcileInterval mirrors -reconcile-interval. A pure tuning
	// knob - safe to change any time, unlike the scope paths above.
	ReconcileInterval time.Duration `json:"reconcile_interval,omitempty"`

	// BhyvePrefix mirrors -bhyve-prefix. Write-once, same reasoning as
	// ZFSBase.
	BhyvePrefix string `json:"bhyve_prefix,omitempty"`

	// BhyveBootROM mirrors -bhyve-bootrom: empty disables bhyve
	// provisioning on this node entirely.
	BhyveBootROM string `json:"bhyve_bootrom,omitempty"`

	// BhyveBridge mirrors -bhyve-bridge: empty disables VM networking
	// on this node.
	BhyveBridge string `json:"bhyve_bridge,omitempty"`

	// DiskSizeMB mirrors -disk-size-mb. Only affects newly created VM
	// disks, never resizes an existing one - safe to change any time.
	DiskSizeMB uint64 `json:"disk_size_mb,omitempty"`

	// ISODir mirrors -iso-dir. Write-once, same reasoning as ZFSBase -
	// existing uploaded images would become invisible, not moved.
	ISODir string `json:"iso_dir,omitempty"`

	// HASTEnabled mirrors -hast-enabled. Nil means use managerd's
	// startup flag; true/false are explicit local overrides, the same
	// tri-state convention JailEnabled below already established.
	HASTEnabled *bool `json:"hast_enabled,omitempty"`

	// JailEnabled mirrors -jail-enabled. Nil means use managerd's
	// startup flag; true/false are explicit local overrides.
	JailEnabled *bool `json:"jail_enabled,omitempty"`

	// JailPrefix mirrors -jail-prefix. Write-once, same reasoning as
	// ZFSBase.
	JailPrefix string `json:"jail_prefix,omitempty"`

	// JailMountBase mirrors -jail-mount-base. Write-once, same
	// reasoning as ZFSBase.
	JailMountBase string `json:"jail_mount_base,omitempty"`

	// JailDiskSizeMB mirrors -jail-disk-size-mb. Only affects newly
	// created replicated jail roots, never resizes an existing one -
	// safe to change any time.
	JailDiskSizeMB uint64 `json:"jail_disk_size_mb,omitempty"`

	// PeerAPIKey mirrors -peer-api-key - a live credential (ADR-0029),
	// write-only from any RPC caller's perspective (see package doc
	// comment). Empty means "leave whatever is currently saved
	// unchanged" at the RPC layer above this package - Save itself has
	// no such special case, it always persists exactly what it's given
	// (the RPC handler is what implements "empty means unchanged" by
	// re-reading the current value first, the same pattern the Users
	// page's own password change already established for "leave blank
	// to keep current").
	PeerAPIKey string `json:"peer_api_key,omitempty"`

	// PeerManagerdPort mirrors -peer-managerd-port.
	PeerManagerdPort string `json:"peer_managerd_port,omitempty"`

	// PeerTLS mirrors -peer-tls. Tri-state, same convention as
	// JailEnabled.
	PeerTLS *bool `json:"peer_tls,omitempty"`

	// PeerTLSHostnameMap mirrors -peer-tls-hostname-map.
	PeerTLSHostnameMap string `json:"peer_tls_hostname_map,omitempty"`

	// AssumptionCheckInterval/AssumptionHeartbeatInterval/
	// AssumptionStaleAfter/AssumptionRunDeadline/AssumptionHistoryLimit/
	// AssumptionHistoryMaxAge mirror the six -assumption-* flags -
	// pure tuning knobs, safe to change any time.
	AssumptionCheckInterval     time.Duration `json:"assumption_check_interval,omitempty"`
	AssumptionHeartbeatInterval time.Duration `json:"assumption_heartbeat_interval,omitempty"`
	AssumptionStaleAfter        time.Duration `json:"assumption_stale_after,omitempty"`
	AssumptionRunDeadline       time.Duration `json:"assumption_run_deadline,omitempty"`
	AssumptionHistoryLimit      int           `json:"assumption_history_limit,omitempty"`
	AssumptionHistoryMaxAge     time.Duration `json:"assumption_history_max_age,omitempty"`

	// TLSCert/TLSKey mirror -tls-cert/-tls-key - file paths, not
	// secrets themselves (the key file's own content is the secret,
	// same posture as CloudflareTokenFile below).
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`

	// CloudflareTokenFile/CloudflareZoneID/CloudflareTunnelID/
	// CloudflareTunnelCredentialsFile mirror ADR-0063's four
	// -cloudflare-* flags. CloudflareTokenFile is a path, never the raw
	// token - that design predates this package (see the flag's own
	// help text) and is preserved here unchanged.
	CloudflareTokenFile             string `json:"cloudflare_token_file,omitempty"`
	CloudflareZoneID                string `json:"cloudflare_zone_id,omitempty"`
	CloudflareTunnelID              string `json:"cloudflare_tunnel_id,omitempty"`
	CloudflareTunnelCredentialsFile string `json:"cloudflare_tunnel_credentials_file,omitempty"`

	// RaftdToken mirrors -raftd-token - a live credential (ADR-0033),
	// write-only, same posture as PeerAPIKey above.
	RaftdToken string `json:"raftd_token,omitempty"`
}

// scopePathFields lists the write-once resource-scope fields (see the
// package doc comment) as (name, accessor) pairs, used by both the
// conflict check in Save and anything that needs to enumerate them
// generically.
type scopePathField struct {
	name  string
	value func(Config) string
}

var scopePathFields = []scopePathField{
	{"zfs_base", func(c Config) string { return c.ZFSBase }},
	{"bhyve_prefix", func(c Config) string { return c.BhyvePrefix }},
	{"iso_dir", func(c Config) string { return c.ISODir }},
	{"jail_prefix", func(c Config) string { return c.JailPrefix }},
	{"jail_mount_base", func(c Config) string { return c.JailMountBase }},
}

// Manager reads/writes Config to a local file. Like internal/isostore,
// it does no validation of *semantic* correctness (e.g. that Uplink
// names an interface that actually exists) - that's surfaced naturally
// the next time managerd starts and internal/vlan/internal/pf actually
// try to use it. It does validate that each value is *safe* (see
// validate) and enforces the write-once rule on resource-scope paths
// (see checkScopePathConflicts) - both independent of whatever the web
// UI itself does or doesn't allow, the same "the RPC layer is the real
// boundary, not just the form" posture this project applies everywhere
// else (ADR-0067).
type Manager struct {
	// Path is where the config file is read from/written to. Defaults
	// to DefaultPath if empty.
	Path string
}

func (m *Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
}

// Load reads the current config. A missing file is not an error - it
// returns the zero Config, matching a fresh install that has never
// saved an override yet.
func (m *Manager) Load() (Config, error) {
	body, err := os.ReadFile(m.path())
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save writes cfg, replacing whatever was there before in full (not a
// merge) - the caller is expected to Load first if it wants to change
// only one field, the same convention internal/hast's WriteConfig and
// internal/dhcpd's RenderConfig already use for their own config files.
// Rejects an unsafe value (validate) or a resource-scope-path change
// that would conflict with an already-saved value (checkScopePathConflicts).
func (m *Manager) Save(cfg Config) error {
	if err := validate(cfg); err != nil {
		return err
	}
	current, err := m.Load()
	if err != nil {
		return err
	}
	if err := checkScopePathConflicts(current, cfg); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 0600, not 0644: this file can hold a live credential
	// (PeerAPIKey/RaftdToken) since the fields above were added - see
	// the package doc comment.
	return os.WriteFile(m.path(), body, 0o600)
}

// checkScopePathConflicts rejects saving a new, different, non-empty
// value for any resource-scope path field that already has a
// different, non-empty value saved - see the package doc comment for
// why. A field left unchanged (new value equal to current, or new
// value empty) never conflicts - "empty" here means "the caller didn't
// intend to touch this field," matching the RPC handler's own
// resend-current-value-for-untouched-fields convention.
func checkScopePathConflicts(current, next Config) error {
	for _, f := range scopePathFields {
		newVal := f.value(next)
		curVal := f.value(current)
		if newVal != "" && curVal != "" && newVal != curVal {
			return fmt.Errorf("nodeconfig: %s is already configured as %q; edit the underlying rc.conf flag directly and restart to change it - changing it here could orphan existing resources still using the old value", f.name, curVal)
		}
	}
	return nil
}

// validate rejects a value that would be unsafe to interpolate into
// generated configuration or otherwise malformed, without judging
// whether it's semantically correct (see the Manager doc comment
// above). Uplink/NATUplink/DNSServer/BhyveBridge/PeerTLSHostnameMap are
// rendered verbatim into generated dnsmasq.conf/pf rules/TLS
// verification logic with no escaping of their own - an unvalidated
// newline in any of them previously let an Admin inject arbitrary
// dnsmasq/pf directives via UpdateNodeConfig (ADR-0067). Every other
// new field added since then is validated too, even where the
// downstream consumer passes it as a separate argv/API argument (not
// vulnerable to injection the same way) - defense in depth, and cheap.
func validate(cfg Config) error {
	if cfg.Uplink != "" && !validInterfaceName(cfg.Uplink) {
		return fmt.Errorf("nodeconfig: invalid uplink %q: must be a plain interface name (alphanumerics and '-', max 15 chars)", cfg.Uplink)
	}
	if cfg.NATUplink != "" && !validInterfaceName(cfg.NATUplink) {
		return fmt.Errorf("nodeconfig: invalid nat_uplink %q: must be a plain interface name (alphanumerics and '-', max 15 chars)", cfg.NATUplink)
	}
	if cfg.BhyveBridge != "" && !validInterfaceName(cfg.BhyveBridge) {
		return fmt.Errorf("nodeconfig: invalid bhyve_bridge %q: must be a plain interface name (alphanumerics and '-', max 15 chars)", cfg.BhyveBridge)
	}
	if cfg.DNSServer != "" && net.ParseIP(cfg.DNSServer) == nil {
		return fmt.Errorf("nodeconfig: invalid dhcp_dns_server %q: must be a plain IP address", cfg.DNSServer)
	}
	if err := validatePathField("bhyve_bootrom", cfg.BhyveBootROM); err != nil {
		return err
	}
	if err := validatePathField("iso_dir", cfg.ISODir); err != nil {
		return err
	}
	if err := validatePathField("jail_mount_base", cfg.JailMountBase); err != nil {
		return err
	}
	if err := validatePathField("tls_cert", cfg.TLSCert); err != nil {
		return err
	}
	if err := validatePathField("tls_key", cfg.TLSKey); err != nil {
		return err
	}
	if err := validatePathField("cloudflare_token_file", cfg.CloudflareTokenFile); err != nil {
		return err
	}
	if err := validatePathField("cloudflare_tunnel_credentials_file", cfg.CloudflareTunnelCredentialsFile); err != nil {
		return err
	}
	if err := validateDatasetOrPrefix("zfs_base", cfg.ZFSBase); err != nil {
		return err
	}
	if err := validateDatasetOrPrefix("bhyve_prefix", cfg.BhyvePrefix); err != nil {
		return err
	}
	if err := validateDatasetOrPrefix("jail_prefix", cfg.JailPrefix); err != nil {
		return err
	}
	if err := validateDatasetOrPrefix("cloudflare_zone_id", cfg.CloudflareZoneID); err != nil {
		return err
	}
	if err := validateDatasetOrPrefix("cloudflare_tunnel_id", cfg.CloudflareTunnelID); err != nil {
		return err
	}
	if err := validatePeerTLSHostnameMap(cfg.PeerTLSHostnameMap); err != nil {
		return err
	}
	if cfg.PeerManagerdPort != "" {
		if _, err := parsePort(cfg.PeerManagerdPort); err != nil {
			return fmt.Errorf("nodeconfig: invalid peer_managerd_port %q: %w", cfg.PeerManagerdPort, err)
		}
	}
	if err := validateDurationField("reconcile_interval", cfg.ReconcileInterval); err != nil {
		return err
	}
	if err := validateDurationField("assumption_check_interval", cfg.AssumptionCheckInterval); err != nil {
		return err
	}
	if err := validateDurationField("assumption_heartbeat_interval", cfg.AssumptionHeartbeatInterval); err != nil {
		return err
	}
	if err := validateDurationField("assumption_stale_after", cfg.AssumptionStaleAfter); err != nil {
		return err
	}
	if err := validateDurationField("assumption_run_deadline", cfg.AssumptionRunDeadline); err != nil {
		return err
	}
	if err := validateDurationField("assumption_history_max_age", cfg.AssumptionHistoryMaxAge); err != nil {
		return err
	}
	if cfg.AssumptionHistoryLimit < 0 {
		return fmt.Errorf("nodeconfig: invalid assumption_history_limit %d: must not be negative", cfg.AssumptionHistoryLimit)
	}
	return nil
}

// validateDurationField rejects a negative duration - Go's own
// time.Duration is a signed int64, and a negative interval/deadline
// would be nonsensical (and in some call sites, e.g. a negative ticker
// interval, an outright panic) once managerd actually starts using it.
func validateDurationField(name string, d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("nodeconfig: invalid %s %q: must not be negative", name, d)
	}
	return nil
}

// validatePathField rejects a newline/carriage-return in a file-path
// field - defense in depth (see validate's own doc comment); none of
// these are currently known to be concatenated into a generated text
// file the way Uplink/NATUplink/DNSServer are, but a future change
// could make that true without anyone remembering to add a check here.
func validatePathField(name, value string) error {
	if value == "" {
		return nil
	}
	for _, r := range value {
		if r == '\n' || r == '\r' {
			return fmt.Errorf("nodeconfig: invalid %s: must not contain newlines", name)
		}
	}
	return nil
}

// validateDatasetOrPrefix applies a slightly wider allowlist than
// validInterfaceName (adding '_', '.', and '/' for a ZFS dataset path
// like "zroot/apiary") to a resource-scope prefix or a plain
// Cloudflare identifier - not a real correctness check (that's
// internal/zfs's/internal/jail's/internal/bhyve's own job at the point
// of actual use, per this package's own "semantic correctness isn't
// validated here" posture), just enough to keep it from being anything
// that could carry a control character or an unexpected separator.
func validateDatasetOrPrefix(name, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 128 {
		return fmt.Errorf("nodeconfig: invalid %s: too long (max 128 chars)", name)
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '/') {
			return fmt.Errorf("nodeconfig: invalid %s %q: must be alphanumerics, '-', '_', '.', or '/'", name, value)
		}
	}
	return nil
}

// validatePeerTLSHostnameMap checks the "ip=hostname,ip=hostname" format
// -peer-tls-hostname-map itself expects (see internal/tlsdial), so a
// malformed value fails loudly here rather than silently doing nothing
// useful the next time managerd starts.
func validatePeerTLSHostnameMap(value string) error {
	if value == "" {
		return nil
	}
	for _, pair := range splitComma(value) {
		ip, host, ok := cutOnce(pair, '=')
		if !ok || ip == "" || host == "" {
			return fmt.Errorf("nodeconfig: invalid peer_tls_hostname_map entry %q: want \"ip=hostname\"", pair)
		}
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("nodeconfig: invalid peer_tls_hostname_map entry %q: %q is not a plain IP address", pair, ip)
		}
	}
	return nil
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func cutOnce(s string, sep byte) (before, after string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func parsePort(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("must not be empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("must be a plain port number")
		}
		n = n*10 + int(r-'0')
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("must be between 1 and 65535")
	}
	return n, nil
}

// validInterfaceName mirrors internal/raft.FSM's own unexported
// function of the same name and rationale (see its doc comment) -
// duplicated rather than shared across an otherwise-unrelated package
// boundary, the same "each package validates its own written format"
// convention internal/jail and internal/isostore already follow
// independently for their own name checks.
func validInterfaceName(name string) bool {
	if len(name) > 15 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
