package backup

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/origincert"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// # Secrets are excluded structurally, and the exclusion is tested
//
// Four inline live credentials exist in this codebase's local
// configuration files, and none of them is in the raft FSM snapshot, so
// no existing backup path covers them:
//
//	nodeconfig.Config.PeerAPIKey       ("a live credential", ADR-0029)
//	nodeconfig.Config.RaftdToken       ("a live credential", ADR-0033)
//	frontendconfig.Config.ManagerAPIKey (the field warnIfWorldReadable exists to warn about)
//	raftdconfig.Config.InternalToken   ("must be root-owned, mode 0600")
//
// A whole-file copy of those configs - the obvious implementation, and
// the one ADR-0131's Rejected alternative 1 exists to rule out - writes
// all four into the archive. So this package does not copy
// configuration files. It builds manifests from allow-listed projections
// into types declared here, and those types have no field capable of
// holding a secret: the exclusion is a property of the destination type,
// not a filter somebody remembered to apply.
//
// Three independent layers, all exercised by tests:
//
//  1. ALLOW-LIST PROJECTION (structural). BuildNodeConfigRecord takes a
//     nodeconfig.Config and returns a NodeConfigRecord, field by field,
//     naming each field it copies. There is no reflection, no
//     json.Marshal of the source, and no wildcard. Adding a field to
//     nodeconfig.Config cannot add it to a manifest, because the
//     projection does not mention it. TestNodeConfigDenyListIsComplete
//     turns that from a convention into a checked property: it reflects
//     over the real structs and fails if any field is unclassified.
//
//  2. REFLECTION AUDIT (deny list). AuditSecretFree walks any value on
//     its way into a manifest and refuses a non-zero field whose Go name
//     or JSON name is deny-listed, or whose name matches a secret-name
//     pattern. This is the backstop for a future value reaching the
//     manifest by some route the projections do not cover - a new
//     artifact kind, a new field added to one of the records in this
//     file, a new config struct someone forgets to audit.
//
//  3. LIVE-VALUE WATCHLIST (bytes). NewSanitizer is handed the live
//     secrets; the encoded manifest and the encoded artifacts are
//     grepped for those exact strings before Commit. This is the layer
//     that catches the case the other two cannot reason about: a secret
//     that has been copied into a field nobody thought to classify. It
//     is deliberately last and deliberately blunt.
//
// The layers are ordered cheapest-first and all three run. An
// allow-list projection alone would be enough if it were the only way in;
// it is not the only way in forever, and "forever" is how long an archive
// outlives the code that wrote it.

// SecretKind identifies one excluded credential.
type SecretKind string

const (
	// SecretPeerAPIKey is nodeconfig.Config.PeerAPIKey, the credential
	// a joining Comb presents to an existing one.
	SecretPeerAPIKey SecretKind = "nodeconfig.peer_api_key"
	// SecretRaftdToken is nodeconfig.Config.RaftdToken, this node's
	// credential to raftd's internal socket.
	SecretRaftdToken SecretKind = "nodeconfig.raftd_token"
	// SecretManagerAPIKey is frontendconfig.Config.ManagerAPIKey, the
	// key the web frontend attaches to managerd calls.
	SecretManagerAPIKey SecretKind = "frontendconfig.manager_api_key"
	// SecretRaftdInternalToken is raftdconfig.Config.InternalToken, the
	// shared secret every internal-protocol caller must present.
	SecretRaftdInternalToken SecretKind = "raftdconfig.internal_token"
)

// SecretField is one entry in the deny list. The table is the single
// source of truth for three things at once, which is what stops them
// from drifting:
//
//   - the reflection audit's set of forbidden field names,
//   - the set of fields the completeness test requires to be absent from
//     every projection, and
//   - the SecretGap list every manifest carries.
//
// Because a gap is *generated* from this table rather than written by
// hand, "the manifest does not contain the secret" and "the manifest
// says which secrets are missing" are the same fact. A secret cannot be
// quietly dropped from the list without also disappearing from the gaps
// an operator reads, and the completeness test fails on a field that
// appeared in the source struct without being classified here.
type SecretField struct {
	// Kind is the stable identifier written into a manifest's
	// SecretGap. It is stable because it outlives this build.
	Kind SecretKind
	// StructName and FieldName locate the field in the real source
	// struct. TestSecretDenyListMatchesRealStructs reflects over the
	// named struct and fails if the field is not there, so a rename
	// upstream cannot silently un-deny a live credential.
	StructName string
	FieldName  string
	// JSONName is the field's wire name in the source struct's own
	// encoding. Both spellings are audited, because a value can arrive
	// under either.
	JSONName string
	// Why is the one-line reason this is excluded.
	Why string
	// LiveCopy is where the real value is today.
	LiveCopy string
	// RestoreProcedure is what a human has to do about it.
	RestoreProcedure string
}

// DenyList is every field whose value must never appear in a manifest,
// in any encoding, ever. It is exported because the completeness test
// and any future caller-side audit need to read it, and unexported would
// only make the test reach into this file.
//
// It is a table and not a set of if-statements for one reason: a deny
// list scattered through the code is a deny list that the completeness
// test cannot check. Here, adding a secret is one row, and forgetting
// to add one is a failing test rather than a silent leak.
var DenyList = []SecretField{
	{
		Kind:       SecretPeerAPIKey,
		StructName: "nodeconfig.Config",
		FieldName:  "PeerAPIKey",
		JSONName:   "peer_api_key",
		Why:        "a live credential (ADR-0029): a peer Comb presents it to join, and a backup is a byte stream that gets copied to places this project does not control",
		LiveCopy:   "this node's managerd configuration file (internal/nodeconfig, mode 0600)",
		RestoreProcedure: "re-issue the peer API key on the Colony's admitting Comb and set it on every node that needs to join; " +
			"an archive does not contain it and never will",
	},
	{
		Kind:       SecretRaftdToken,
		StructName: "nodeconfig.Config",
		FieldName:  "RaftdToken",
		JSONName:   "raftd_token",
		Why:        "a live credential (ADR-0033): this node's own token to raftd's internal socket, and a token that works on one Comb is a token an attacker wants on a second",
		LiveCopy:   "this node's managerd configuration file (internal/nodeconfig, mode 0600)",
		RestoreProcedure: "generate a fresh raftd token for the rebuilt Comb and set it in both raftd and managerd configuration; " +
			"a rebuilt Comb must never be given a token recovered from an archive",
	},
	{
		Kind:             SecretManagerAPIKey,
		StructName:       "frontendconfig.Config",
		FieldName:        "ManagerAPIKey",
		JSONName:         "manager_api_key",
		Why:              "the credential the web frontend attaches to managerd on a logged-in user's behalf once API-key auth is enabled (ADR-0023); the field warnIfWorldReadable exists to warn about this very file",
		LiveCopy:         "this node's frontend configuration file (internal/frontendconfig, mode 0600)",
		RestoreProcedure: "issue a new manager API key, or re-enable the existing one through the API-key flow; a backup deliberately does not make a revoked key usable again",
	},
	{
		Kind:       SecretRaftdInternalToken,
		StructName: "raftdconfig.Config",
		FieldName:  "InternalToken",
		JSONName:   "internal_token",
		Why:        "the shared secret every RaftInternal caller must present, stored inline in JSON in a file its own loader says must be root-owned and mode 0600",
		LiveCopy:   "this node's raftd configuration file (internal/raftdconfig, mode 0600)",
		RestoreProcedure: "generate a fresh internal token on the rebuilt Comb and set it in every raftd and managerd that talks to it; " +
			"the Colony's membership is restored by raftd -restore (ADR-0051), which is a separate offline action and does not use archives",
	},
}

// SecretPaths is the second table: fields whose VALUE is not a secret
// but points at one. nodeconfig's own comment says it of TLSCert/TLSKey
// - "file paths, not secrets themselves" - and the rule that follows is
// the same everywhere: record the path, never the content.
//
// It exists as a table, and not as an allowance inside the heuristic,
// because the distinction between "this value IS a credential" and "this
// value names a file that holds one" is exactly the distinction an
// auditor has to be able to check. A heuristic that quietly allowed
// anything named "*token*" would also allow a field actually holding a
// token, and the difference between those two is the whole job.
//
// Every entry here also produces a SecretGap. A restore operator reading
// a manifest needs to know that the file named at this path holds
// material they will have to re-provision, and silence about it is how a
// rebuilt Comb comes up mysteriously unauthenticated.
var SecretPaths = []SecretPath{
	{
		Kind:       SecretPathTLSKey,
		StructName: "nodeconfig.Config",
		FieldName:  "TLSKey",
		JSONName:   "tls_key",
		Why:        "the value is a path to this node's managerd TLS private key; the path is recorded so a restore operator knows where the material belongs, and the key's content is never copied",
		LiveCopy:   "the file this path names, mode-restricted, on this Comb",
		RestoreProcedure: "re-issue managerd's serving certificate and key, or copy them from a secret store this path points into; " +
			"an archive contains the path and never the key",
	},
	{
		Kind:             SecretPathCloudflareToken,
		StructName:       "nodeconfig.Config",
		FieldName:        "CloudflareTokenFile",
		JSONName:         "cloudflare_token_file",
		Why:              "the value is a path to a file holding a tunnel credential, by this project's own design - the flag's help text says so and the package comment repeats it; the path is recorded, the token is not",
		LiveCopy:         "the file this path names, on this Comb",
		RestoreProcedure: "re-issue the tunnel token and write it to this path on the rebuilt Comb",
	},
	{
		Kind:             SecretPathTunnelCredentials,
		StructName:       "nodeconfig.Config",
		FieldName:        "CloudflareTunnelCredentialsFile",
		JSONName:         "cloudflare_tunnel_credentials_file",
		Why:              "the value is a path to pre-provisioned tunnel credentials; the name says credentials, the value is a path, and the distinction is what keeps the secret out of the archive while telling the operator what to re-provision",
		LiveCopy:         "the file this path names, on this Comb",
		RestoreProcedure: "re-create the tunnel credentials and write them to this path on the rebuilt Comb",
	},
	{
		Kind:             SecretPathOriginCAToken,
		StructName:       "nodeconfig.Config",
		FieldName:        "OriginCATokenFile",
		JSONName:         "origin_ca_token_file",
		Why:              "the value is a path to a zone-scoped issuance credential; the file is read for issuance only and its content is never copied",
		LiveCopy:         "the file this path names, on this Comb",
		RestoreProcedure: "re-create the zone-scoped credential at this path on the rebuilt Comb",
	},
	{
		Kind:       SecretPathRaftTLSKey,
		StructName: "raftdconfig.Config",
		FieldName:  "RaftTLSKey",
		JSONName:   "raft_tls_key",
		Why:        "the value is a path to raftd's transport private key (ADR-0078); the certificate and CA paths beside it are public and are recorded normally, and this one is not",
		LiveCopy:   "the file this path names, on this Comb",
		RestoreProcedure: "re-issue raftd's transport certificate and key at this path, or copy them from a secret store; " +
			"note that Colony membership is restored by raftd -restore, not by an archive",
	},
	{
		Kind:             SecretPathFrontendTLSKey,
		StructName:       "frontendconfig.Config",
		FieldName:        "TLSKey",
		JSONName:         "tls_key",
		Why:              "the value is a path to the web UI's serving private key; the certificate path beside it is public and is recorded normally, and this one is not",
		LiveCopy:         "the file this path names, on this Comb",
		RestoreProcedure: "re-issue the web UI's serving certificate and key at this path on the rebuilt Comb",
	},
	{
		Kind:             SecretPathOriginCertKey,
		StructName:       "origincert.InventoryEntry",
		FieldName:        "KeyPath",
		JSONName:         "key_path",
		Why:              "internal/origincert's WritePair writes a certificate and its private key side by side, so the inventory entry names the key; the certificate and the inventory are recorded, the key is not",
		LiveCopy:         "the file this path names, on this Comb, written 0600 by origincert.WritePair",
		RestoreProcedure: "re-issue the certificate and its key with the existing issuance flow; the inventory entry in the archive says a certificate of this name existed and where its key was",
	},
}

// SecretPathKind identifies one path-to-secret field.
type SecretPathKind string

const (
	// SecretPathTLSKey is nodeconfig.Config.TLSKey.
	SecretPathTLSKey SecretPathKind = "nodeconfig.tls_key"
	// SecretPathCloudflareToken is nodeconfig.Config.CloudflareTokenFile.
	SecretPathCloudflareToken SecretPathKind = "nodeconfig.cloudflare_token_file"
	// SecretPathTunnelCredentials is nodeconfig.Config.CloudflareTunnelCredentialsFile.
	SecretPathTunnelCredentials SecretPathKind = "nodeconfig.cloudflare_tunnel_credentials_file"
	// SecretPathOriginCAToken is nodeconfig.Config.OriginCATokenFile.
	SecretPathOriginCAToken SecretPathKind = "nodeconfig.origin_ca_token_file"
	// SecretPathRaftTLSKey is raftdconfig.Config.RaftTLSKey.
	SecretPathRaftTLSKey SecretPathKind = "raftdconfig.raft_tls_key"
	// SecretPathFrontendTLSKey is frontendconfig.Config.TLSKey.
	SecretPathFrontendTLSKey SecretPathKind = "frontendconfig.tls_key"
	// SecretPathOriginCertKey is origincert.InventoryEntry.KeyPath.
	SecretPathOriginCertKey SecretPathKind = "origincert.key_path"
)

// SecretPath is one entry in the path table. It carries the same four
// operator-facing fields as SecretField, for the same reason: an
// exclusion that is not described is an exclusion the operator will
// rediscover as a broken service after a disaster.
type SecretPath struct {
	Kind             SecretPathKind
	StructName       string
	FieldName        string
	JSONName         string
	Why              string
	LiveCopy         string
	RestoreProcedure string
}

// secretNamePatterns is the heuristic arm of the reflection audit. The
// deny list above is exact and is what must catch the four known
// credentials; this is the wider net for a field that does not exist
// yet - something called *Password, *Secret, *Token, *APIKey or *KeyPEM
// reaching a manifest is refused on sight, and the fix is to either
// rename it into the deny list with a real exclusion, or prove it is not
// a secret. Neither answer is "let it through".
var secretNamePatterns = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key",
	"privatekey", "private_key", "keypem", "key_pem", "credential",
	"passphrase",
}

// auditAllowedNames are the schema's own field names that are
// secret-shaped but hold no secret.
//
// There is exactly one pair, and it is not an exception - it is the
// point. Manifest.SecretGaps is the field that RECORDS what was
// deliberately left out, and a rule that refused it would mean no manifest
// could ever say which credentials a restore operator has to re-issue. An
// archive that is silent about its own gaps is the failure this package
// exists to prevent, so the field describing the gaps has to be allowed
// to exist.
//
// The list is explicit rather than a pattern carve-out so that adding a
// second entry is a visible decision carrying a stated reason, and so
// TestAuditAllowedNamesAreAllJustified can require every entry to have
// one.
var auditAllowedNames = map[string]string{
	"secret_gaps": "the list of excluded credentials itself - the record of the gaps, not any gap's contents",
	"secretgaps":  "the same field under its Go name",
}

// secretNameSuffixes catch the class the substring patterns above
// deliberately miss: a field whose name ENDS in one of these words. The
// motivating case is a bare "…Key" - a config field called EncryptionKey
// or SigningKey is a live credential, and a substring rule for "key"
// cannot be used because five fields in this codebase legitimately end in
// Key while holding a PATH. So the suffix rule is paired with the
// SecretPaths table: any field ending in one of these words must be
// classified, and a path is a classification.
//
// The result is that a new field called anythingKey is refused on arrival
// until somebody says what it is, and the answer "it is a path" is a line
// in a table rather than a carve-out in a name-matching heuristic.
var secretNameSuffixes = []string{
	"key", "secret", "token", "password", "passwd", "credential",
	"passphrase",
}

// denyIndex maps a lowercased field name to its SecretField, for the
// exact-match arm of the audit. Built once at init from DenyList so
// there is exactly one list in this file.
var denyIndex = func() map[string]SecretField {
	idx := make(map[string]SecretField, len(DenyList)*2)
	for _, f := range DenyList {
		idx[strings.ToLower(f.FieldName)] = f
		if f.JSONName != "" {
			idx[strings.ToLower(f.JSONName)] = f
		}
	}
	return idx
}()

// pathIndex maps a lowercased field name to its SecretPath, for the
// same reason denyIndex exists: the audit has to distinguish a value
// that IS a credential from a value that NAMES one, and it can only do
// that from an explicit table.
var pathIndex = func() map[string]SecretPath {
	idx := make(map[string]SecretPath, len(SecretPaths)*2)
	for _, p := range SecretPaths {
		idx[strings.ToLower(p.FieldName)] = p
		if p.JSONName != "" {
			idx[strings.ToLower(p.JSONName)] = p
		}
	}
	return idx
}()

// SecretGaps returns the gap list every manifest carries, generated from
// DenyList and SecretPaths. It is a function rather than a var so a
// caller cannot mutate a package-level slice that other manifests would
// then inherit.
//
// The list is unconditional: it does not depend on whether the node
// happens to have a peer key configured. "There is no gap" and "we did
// not look" must not be the same document, and a node with no peers
// still has a raftd token.
//
// The two tables produce differently-worded gaps on purpose. An inline
// secret has no representation in the manifest at all, while a
// secret-path field's path IS in the manifest and its content is not -
// so a reader who sees a tls_key path in an archive should be told
// exactly that, rather than being left to guess whether the key came
// with it.
func SecretGaps() []SecretGap {
	out := make([]SecretGap, 0, len(DenyList)+len(SecretPaths))
	for _, f := range DenyList {
		out = append(out, SecretGap{
			Kind:             string(f.Kind),
			Why:              f.Why,
			LiveCopyLocation: f.LiveCopy,
			RestoreProcedure: f.RestoreProcedure,
		})
	}
	for _, p := range SecretPaths {
		out = append(out, SecretGap{
			Kind:             string(p.Kind),
			Why:              p.Why,
			LiveCopyLocation: p.LiveCopy,
			RestoreProcedure: p.RestoreProcedure,
		})
	}
	return out
}

// looksLikeSecretField reports whether a field NAME is one the audit
// refuses on sight. The value is never consulted, so this is a naming
// policy; the deny list is the correctness mechanism and this is the
// alarm that says "a new field arrived and nobody classified it".
//
// A field in SecretPaths returns false: its value is a path, and the
// table is what says so. That is the only exemption, and it is a
// table lookup rather than a substring exception, so a field cannot
// exempt itself by being named suspiciously.
func looksLikeSecretField(name string) bool {
	lower := strings.ToLower(name)
	if _, allowed := auditAllowedNames[lower]; allowed {
		return false
	}
	if _, denied := denyIndex[lower]; denied {
		return true
	}
	if _, isPath := pathIndex[lower]; isPath {
		return false
	}
	for _, p := range secretNamePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	for _, s := range secretNameSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

// AuditSecretFree walks v and refuses it if any field reached has a
// secret-shaped name and a non-zero value.
//
// The walk is by reflection, unexported fields included, because
// reflection is here to be paranoid: the point is to catch a value on a
// path nobody audited, which by definition is a path this file has not
// enumerated. Cycles are handled, pointers and slices and maps are
// followed to a bounded depth, and a depth that is exceeded is itself
// an error rather than a silent stop - a manifest record that nests
// fifteen levels deep is a manifest record nobody reviewed.
func AuditSecretFree(v any) error {
	return auditValue(reflect.ValueOf(v), map[uintptr]bool{}, 0, "manifest")
}

// maxAuditDepth bounds the walk. It is generous for a record structure
// and tight enough that a self-referential value cannot spin.
const maxAuditDepth = 32

func auditValue(v reflect.Value, seen map[uintptr]bool, depth int, path string) error {
	if depth > maxAuditDepth {
		return fmt.Errorf("%w: %s nests deeper than %d levels, so it was not fully audited", ErrSecretExposed, path, maxAuditDepth)
	}
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		if v.Kind() == reflect.Pointer {
			addr := v.Pointer()
			if seen[addr] {
				return nil // cycle
			}
			seen[addr] = true
			defer delete(seen, addr)
		}
		return auditValue(v.Elem(), seen, depth+1, path)

	case reflect.Slice, reflect.Array:
		// A byte slice is data, not fields. Digests and marshalled
		// payloads live here and are exactly what should not be
		// pattern-matched.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			if err := auditValue(v.Index(i), seen, depth+1, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil

	case reflect.Map:
		for _, k := range v.MapKeys() {
			// The KEY is audited as well as the value, because a decoded
			// JSON record - which is what a node config artifact's bytes
			// become - is a map, and a map has field names only as keys.
			// Without this arm of the walk, auditing a decoded record
			// would check the values and never the names, which is
			// backwards: it is the name that says "this is a credential".
			if name, ok := k.Interface().(string); ok && looksLikeSecretField(name) && !isZeroish(v.MapIndex(k)) {
				if denied, isDenied := denyIndex[strings.ToLower(name)]; isDenied {
					return fmt.Errorf("%w: %s has a key %q, which is %s (%s.%s) and must never be serialised into a manifest",
						ErrSecretExposed, path, name, denied.Kind, denied.StructName, denied.FieldName)
				}
				return fmt.Errorf("%w: %s has a key %q, which is named secret-shaped and holds a value; a key whose name is secret-shaped must be deny-listed with a stated exclusion before it can appear in a manifest",
					ErrSecretExposed, path, name)
			}
			if err := auditValue(v.MapIndex(k), seen, depth+1, fmt.Sprintf("%s[%v]", path, k)); err != nil {
				return err
			}
		}
		return nil

	case reflect.Struct:
		// time.Time is a value, not a record with a field named
		// something alarming. Named types that wrap another type are
		// walked as their underlying value further down.
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return nil
		}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			fv := v.Field(i)
			name := f.Name
			child := path + "." + name
			if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
				name = tag
			}
			if looksLikeSecretField(name) && !isZeroish(fv) {
				if denied, ok := denyIndex[strings.ToLower(name)]; ok {
					return fmt.Errorf("%w: %s is %s (%s.%s), which is on the deny list and must never be serialised into a manifest",
						ErrSecretExposed, child, denied.Kind, denied.StructName, denied.FieldName)
				}
				return fmt.Errorf("%w: %s is named %q and holds a value; a field whose name is secret-shaped must be deny-listed with a stated exclusion before it can appear in a manifest",
					ErrSecretExposed, child, name)
			}
			if err := auditValue(fv, seen, depth+1, child); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

// isZeroish reports whether a value is "unset" for the purposes of the
// audit. An empty string, a nil pointer, a zero number and an empty
// slice are all unset; anything else is a value somebody chose to put
// there. A zero-length string named "token" is not a leak, and refusing
// it would make the audit unusable on a config that happens to have no
// peer key set.
func isZeroish(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil() || (v.Kind() == reflect.Slice && v.Len() == 0)
	case reflect.Interface:
		// A map[string]any - which is what a decoded JSON record is -
		// hands out interface-typed values, and an empty string inside
		// one is a non-nil interface holding "". Unwrapping here is what
		// makes a config with an unset credential field pass the audit
		// instead of failing it, which matters because a single-node
		// install legitimately has no peer key at all.
		if v.IsNil() {
			return true
		}
		return isZeroish(v.Elem())
	case reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	}
	return false
}

// # The projections
//
// Each of these copies named fields one at a time. There is no
// reflection, no struct copy, no encoding of the source object. A field
// added to a source struct is invisible here by construction, and
// TestNodeConfigDenyListIsComplete exists to make sure that stays a
// deliberate property rather than an accident.

// NodeConfigRecord is the backup-safe projection of a nodeconfig.Config.
//
// Read the field list as a sentence about what a rebuilt Comb needs to
// be told, minus the two credentials it must be handed fresh. There is
// no PeerAPIKey and no RaftdToken, and there cannot be one added without
// also adding a DenyList row, because the completeness test classifies
// every field of the real struct.
type NodeConfigRecord struct {
	NodeID      string `json:"node_id,omitempty"`
	RPCAddr     string `json:"rpc_addr,omitempty"`
	RaftdSocket string `json:"raftd_socket,omitempty"`
	Uplink      string `json:"uplink,omitempty"`
	NATUplink   string `json:"nat_uplink,omitempty"`
	DNSServer   string `json:"dhcp_dns_server,omitempty"`
	ZFSBase     string `json:"zfs_base,omitempty"`

	ReconcileInterval string `json:"reconcile_interval,omitempty"`

	BhyvePrefix         string `json:"bhyve_prefix,omitempty"`
	BhyveBootROM        string `json:"bhyve_bootrom,omitempty"`
	BhyveBridge         string `json:"bhyve_bridge,omitempty"`
	AllowUplinkBridging string `json:"allow_uplink_bridging,omitempty"`

	DiskSizeMB string `json:"disk_size_mb,omitempty"`
	ISODir     string `json:"iso_dir,omitempty"`

	HASTEnabled *bool `json:"hast_enabled,omitempty"`
	JailEnabled *bool `json:"jail_enabled,omitempty"`

	JailPrefix     string `json:"jail_prefix,omitempty"`
	JailMountBase  string `json:"jail_mount_base,omitempty"`
	JailDiskSizeMB string `json:"jail_disk_size_mb,omitempty"`

	PeerManagerdPort   string `json:"peer_managerd_port,omitempty"`
	PeerTLS            *bool  `json:"peer_tls,omitempty"`
	PeerTLSHostnameMap string `json:"peer_tls_hostname_map,omitempty"`
	PeerTLSCA          string `json:"peer_tls_ca,omitempty"`
	KnownPeerAddresses string `json:"known_peer_addresses,omitempty"`

	AssumptionCheckInterval     string `json:"assumption_check_interval,omitempty"`
	AssumptionHeartbeatInterval string `json:"assumption_heartbeat_interval,omitempty"`
	AssumptionStaleAfter        string `json:"assumption_stale_after,omitempty"`
	AssumptionRunDeadline       string `json:"assumption_run_deadline,omitempty"`
	AssumptionHistoryLimit      string `json:"assumption_history_limit,omitempty"`
	AssumptionHistoryMaxAge     string `json:"assumption_history_max_age,omitempty"`

	// TLSCert and TLSKey are recorded as PATHS. nodeconfig's own comment
	// is explicit that these are "file paths, not secrets themselves" -
	// the key file's content is the secret. So the path goes in and the
	// content never does, and a restore operator is told where the
	// material is rather than being handed a copy of it. This is the
	// ADR's "record the path, never the content" rule, and the
	// completeness test requires both fields to be present here, so that
	// "the path is recorded" is a checked claim.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`

	PAMService string `json:"pam_service,omitempty"`

	// CloudflareTokenFile, CloudflareTunnelCredentialsFile and
	// OriginCATokenFile are paths to files whose CONTENTS are
	// credentials. Recorded as paths on the same reasoning as TLSKey
	// above. CloudflareZoneID and CloudflareTunnelID are public
	// identifiers and are recorded normally.
	CloudflareTokenFile             string `json:"cloudflare_token_file,omitempty"`
	CloudflareZoneID                string `json:"cloudflare_zone_id,omitempty"`
	CloudflareTunnelID              string `json:"cloudflare_tunnel_id,omitempty"`
	CloudflareTunnelCredentialsFile string `json:"cloudflare_tunnel_credentials_file,omitempty"`
	OriginCATokenFile               string `json:"origin_ca_token_file,omitempty"`
	OriginCADirectory               string `json:"origin_ca_directory,omitempty"`

	OriginCARenewalCheckInterval string `json:"origin_ca_renewal_check_interval,omitempty"`

	// Peers is the list of known peer Comb identifiers, which is public
	// topology rather than a credential.
	Peers []string `json:"peers,omitempty"`
}

// BuildNodeConfigRecord projects a nodeconfig.Config into its
// backup-safe form.
//
// The two missing fields are the entire point of the function, and their
// absence is checked by TestNodeConfigDenyListIsComplete against the real
// struct rather than trusted to this comment. Durations are rendered as
// their string form rather than their nanosecond count because a reader
// looking at an archive by hand should not have to know that 30*time.Second
// is 30000000000.
func BuildNodeConfigRecord(cfg nodeconfig.Config, peers []string) NodeConfigRecord {
	return NodeConfigRecord{
		NodeID:      cfg.NodeID,
		RPCAddr:     cfg.RPCAddr,
		RaftdSocket: cfg.RaftdSocket,
		Uplink:      cfg.Uplink,
		NATUplink:   cfg.NATUplink,
		DNSServer:   cfg.DNSServer,
		ZFSBase:     cfg.ZFSBase,

		ReconcileInterval: cfg.ReconcileInterval.String(),

		BhyvePrefix:         cfg.BhyvePrefix,
		BhyveBootROM:        cfg.BhyveBootROM,
		BhyveBridge:         cfg.BhyveBridge,
		AllowUplinkBridging: cfg.AllowUplinkBridging,

		DiskSizeMB: fmt.Sprintf("%d", cfg.DiskSizeMB),
		ISODir:     cfg.ISODir,

		HASTEnabled: cfg.HASTEnabled,
		JailEnabled: cfg.JailEnabled,

		JailPrefix:     cfg.JailPrefix,
		JailMountBase:  cfg.JailMountBase,
		JailDiskSizeMB: fmt.Sprintf("%d", cfg.JailDiskSizeMB),

		PeerManagerdPort:   cfg.PeerManagerdPort,
		PeerTLS:            cfg.PeerTLS,
		PeerTLSHostnameMap: cfg.PeerTLSHostnameMap,
		PeerTLSCA:          cfg.PeerTLSCA,
		KnownPeerAddresses: cfg.KnownPeerAddresses,

		AssumptionCheckInterval:     cfg.AssumptionCheckInterval.String(),
		AssumptionHeartbeatInterval: cfg.AssumptionHeartbeatInterval.String(),
		AssumptionStaleAfter:        cfg.AssumptionStaleAfter.String(),
		AssumptionRunDeadline:       cfg.AssumptionRunDeadline.String(),
		AssumptionHistoryLimit:      fmt.Sprintf("%d", cfg.AssumptionHistoryLimit),
		AssumptionHistoryMaxAge:     cfg.AssumptionHistoryMaxAge.String(),

		TLSCert: cfg.TLSCert,
		TLSKey:  cfg.TLSKey,

		PAMService: cfg.PAMService,

		CloudflareTokenFile:             cfg.CloudflareTokenFile,
		CloudflareZoneID:                cfg.CloudflareZoneID,
		CloudflareTunnelID:              cfg.CloudflareTunnelID,
		CloudflareTunnelCredentialsFile: cfg.CloudflareTunnelCredentialsFile,
		OriginCATokenFile:               cfg.OriginCATokenFile,
		OriginCADirectory:               cfg.OriginCADirectory,

		OriginCARenewalCheckInterval: cfg.OriginCARenewalCheckInterval.String(),

		Peers: append([]string(nil), peers...),
	}
}

// RaftdConfigRecord is the backup-safe projection of a
// raftdconfig.Config, with the same shape of rule: everything that
// describes this node's raftd, minus the shared secret that authenticates
// an internal caller.
type RaftdConfigRecord struct {
	DataDir  string `json:"data_dir,omitempty"`
	Socket   string `json:"socket,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	RaftBind string `json:"raft_bind,omitempty"`
	// Join and AwaitJoin are startup-time behaviour on a fresh, empty
	// DataDir. A rebuilt Comb does not have a fresh empty DataDir any
	// more by the time anyone would read this, and re-arming a join on a
	// node that is already a member is at best a no-op and at worst a
	// second cluster. They are projected as paths/flags for the record,
	// not acted on: Restore does not read this artifact at all in v1.
	Join      string `json:"join,omitempty"`
	AwaitJoin bool   `json:"await_join,omitempty"`
	// RaftTLS* are paths, on the same reasoning as NodeConfigRecord's
	// TLSCert/TLSKey: the key file's content is the secret, the path is
	// not.
	RaftTLSCert string `json:"raft_tls_cert,omitempty"`
	RaftTLSKey  string `json:"raft_tls_key,omitempty"`
	RaftTLSCA   string `json:"raft_tls_ca,omitempty"`
}

// BuildRaftdConfigRecord projects a raftdconfig.Config, dropping
// InternalToken.
func BuildRaftdConfigRecord(cfg raftdconfig.Config) RaftdConfigRecord {
	return RaftdConfigRecord{
		DataDir:     cfg.DataDir,
		Socket:      cfg.Socket,
		NodeID:      cfg.NodeID,
		RaftBind:    cfg.RaftBind,
		Join:        cfg.Join,
		AwaitJoin:   cfg.AwaitJoin,
		RaftTLSCert: cfg.RaftTLSCert,
		RaftTLSKey:  cfg.RaftTLSKey,
		RaftTLSCA:   cfg.RaftTLSCA,
	}
}

// FrontendConfigRecord is the backup-safe projection of a
// frontendconfig.Config, minus ManagerAPIKey.
type FrontendConfigRecord struct {
	ManagerAddr          string `json:"manager_addr,omitempty"`
	HTTPAddr             string `json:"http_addr,omitempty"`
	ManagerTLS           bool   `json:"manager_tls,omitempty"`
	ManagerTLSCA         string `json:"manager_tls_ca,omitempty"`
	ManagerTLSServerName string `json:"manager_tls_server_name,omitempty"`
	// TLSCert/TLSKey are the web UI's serving material, recorded as
	// paths only.
	TLSCert            string `json:"tls_cert,omitempty"`
	TLSKey             string `json:"tls_key,omitempty"`
	PeerTLS            bool   `json:"peer_tls,omitempty"`
	PeerTLSCA          string `json:"peer_tls_ca,omitempty"`
	PeerHostnameSuffix string `json:"peer_hostname_suffix,omitempty"`
	PeerManagerPort    string `json:"peer_manager_port,omitempty"`
}

// BuildFrontendConfigRecord projects a frontendconfig.Config, dropping
// ManagerAPIKey.
func BuildFrontendConfigRecord(cfg frontendconfig.Config) FrontendConfigRecord {
	return FrontendConfigRecord{
		ManagerAddr:          cfg.ManagerAddr,
		HTTPAddr:             cfg.HTTPAddr,
		ManagerTLS:           cfg.ManagerTLS,
		ManagerTLSCA:         cfg.ManagerTLSCA,
		ManagerTLSServerName: cfg.ManagerTLSServerName,
		TLSCert:              cfg.TLSCert,
		TLSKey:               cfg.TLSKey,
		PeerTLS:              cfg.PeerTLS,
		PeerTLSCA:            cfg.PeerTLSCA,
		PeerHostnameSuffix:   cfg.PeerHostnameSuffix,
		PeerManagerPort:      cfg.PeerManagerPort,
	}
}

// OriginCertRecord is the backup-safe projection of one
// origincert.InventoryEntry.
//
// internal/origincert's WritePair writes a certificate and its private
// key side by side, so the inventory's own KeyPath is the one field here
// that names secret material. The certificate, the hostnames, the
// expiry and the renewal policy are all public and all recorded, because
// an operator restoring a Comb needs to know a certificate of that name
// existed and when it lapses - that is the difference between an archive
// that is useful for reconstruction and one that is only a blob. The key
// is not in here, and SecretPaths says so in a manifest's gap list.
type OriginCertRecord struct {
	Name         string   `json:"name,omitempty"`
	Service      string   `json:"service,omitempty"`
	Hostnames    []string `json:"hostnames,omitempty"`
	ID           string   `json:"id,omitempty"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	CertPath     string   `json:"cert_path,omitempty"`
	KeyPath      string   `json:"key_path,omitempty"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
	AutoRenew    bool     `json:"auto_renew,omitempty"`
	ValidityDays int      `json:"validity_days,omitempty"`
}

// BuildOriginCertRecords projects an origincert inventory.
//
// Times are rendered in RFC 3339 rather than as Go's default, because
// this record is read by a human looking at an archive off a dead host
// and "2026-09-26T10:49:00Z" is the difference between a usable
// document and a Go duration-looking wall of digits.
func BuildOriginCertRecords(entries []origincert.InventoryEntry) []OriginCertRecord {
	out := make([]OriginCertRecord, 0, len(entries))
	for _, e := range entries {
		rec := OriginCertRecord{
			Name:         e.Name,
			Service:      e.Service,
			Hostnames:    append([]string(nil), e.Hostnames...),
			ID:           e.ID,
			CertPath:     e.CertPath,
			KeyPath:      e.KeyPath,
			AutoRenew:    e.AutoRenew,
			ValidityDays: e.ValidityDays,
		}
		if !e.ExpiresAt.IsZero() {
			rec.ExpiresAt = e.ExpiresAt.UTC().Format(time.RFC3339)
		}
		if !e.UpdatedAt.IsZero() {
			rec.UpdatedAt = e.UpdatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, rec)
	}
	return out
}

// originCertKeyPEM-shaped live material is registered with AddSecret -
// see the note there about the certificate key PEM, which has no config
// field holding it.

// auditNodeConfigRecord is the unconditional guard on a node config
// artifact's bytes.
//
// A node config artifact is the one artifact whose content this package
// both produces and cares about the shape of, so it gets a check that
// needs no live secrets to run: the bytes are decoded as JSON and the
// resulting keys are audited by name. That is what makes the guarantee
// hold on a node whose configuration happens to hold no credentials at
// all - where the watchlist has nothing to watch and the byte guard is
// correctly silent.
//
// A decode failure is an error rather than a pass. A node config artifact
// whose bytes are not valid JSON is not the record this package said it
// would write, and letting it through would mean the check was skipped
// without anybody noticing.
//
// The value is decoded as any JSON value rather than as an object,
// because a node config artifact legitimately holds an array - the
// projected certificate inventory is one - and requiring an object would
// mean the guard rejected a record this package produces itself. The key
// names are what matters, and AuditSecretFree walks into arrays and
// objects alike.
func auditNodeConfigRecord(body []byte) error {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("node config artifact is not valid JSON, so its contents could not be audited: %w", err)
	}
	return AuditSecretFree(decoded)
}

// # Layer 3: the live-value watchlist

// Sanitizer holds the live secret VALUES from this node, for the
// byte-level guard. It is separate from the deny list on purpose: the
// deny list knows field NAMES, and a name is a hypothesis about where a
// secret is; a watchlist holds the actual bytes, and finding those bytes
// in an artifact is not a hypothesis.
//
// Constructed with NewSanitizer, never a struct literal, so that the
// value-copying happens in one place.
type Sanitizer struct {
	// watchlist holds the non-empty live secret values. They live in
	// this struct only for the lifetime of a capture and are never
	// written anywhere: Guard is the only method that reads them, and it
	// only ever compares.
	watchlist []string
	// shortOf is the minimum length worth grepping for. A one-character
	// "secret" would match essentially any byte stream and turn the
	// guard into a coin flip; a real credential is long. Values below
	// this are ignored, and the count of what was ignored is reported by
	// Ignored so a test can assert the guard was actually armed.
	shortOf int
	// Ignored names the values the guard chose not to watch, for a
	// caller that wants to say so out loud rather than quietly have a
	// weaker guard than it looks.
	Ignored []string
}

// minWatchedSecretLen is the shortest value the byte guard will grep for.
// A real token in this codebase is long; anything this short would match
// unrelated bytes and produce a false failure that trains people to
// disable the guard.
const minWatchedSecretLen = 8

// NewSanitizer returns a Sanitizer watching every non-empty secret in
// the supplied configs. nil configs are allowed - a node with no
// frontend configured simply has no frontend secret - and are
// distinguished from a config with an empty secret, because "there is
// no secret here" and "there is a secret and it is empty" are different
// states and only one of them is a reason to record a gap.
func NewSanitizer(node *nodeconfig.Config, raftd *raftdconfig.Config, frontend *frontendconfig.Config) *Sanitizer {
	s := &Sanitizer{shortOf: minWatchedSecretLen}
	add := func(kind, value string) {
		if value == "" {
			return
		}
		if len(value) < s.shortOf {
			s.Ignored = append(s.Ignored, kind)
			return
		}
		s.watchlist = append(s.watchlist, value)
	}
	if node != nil {
		add(string(SecretPeerAPIKey), node.PeerAPIKey)
		add(string(SecretRaftdToken), node.RaftdToken)
	}
	if raftd != nil {
		add(string(SecretRaftdInternalToken), raftd.InternalToken)
	}
	if frontend != nil {
		add(string(SecretManagerAPIKey), frontend.ManagerAPIKey)
	}
	return s
}

// AddSecret registers an additional live secret value with the byte
// guard - the extension point for material that is not a field in one of
// the four configuration structs.
//
// The obvious use is a certificate private key PEM: internal/origincert's
// WritePair returns it as a value alongside the certificate, and there is
// no config field holding it for NewSanitizer to find. A caller that
// holds one registers it here, and the guard then greps every artifact
// for it. The paired gap entry is generated from SecretPaths
// (SecretPathOriginCertKey), so registering the value does not have to be
// accompanied by hand-written prose about it.
//
// An empty or too-short value is refused rather than accepted-and-
// ignored: a caller that believes it registered a secret and did not is
// worse off than a caller that was told.
func (s *Sanitizer) AddSecret(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("backup: refusing to register %q with the secret guard: the value is empty, so there is nothing to guard against and a caller that believes it registered a secret here is mistaken", kind)
	}
	if len(value) < s.shortOf {
		return fmt.Errorf("backup: refusing to register %q with the secret guard: the value is %d characters, under the %d minimum, and grepping for it would match unrelated bytes and fail backups at random",
			kind, len(value), s.shortOf)
	}
	s.watchlist = append(s.watchlist, value)
	return nil
}

// Armed reports whether the byte guard has anything to watch. A
// capture must refuse to run - or must say loudly that the guard is
// empty - when this is false, because "we checked and found nothing" and
// "we had nothing to check" are not the same result, and a manifest
// claiming the first when it is the second is a lie.
func (s *Sanitizer) Armed() bool { return s != nil && len(s.watchlist) > 0 }

// Watched returns how many live secret values the guard is watching.
func (s *Sanitizer) Watched() int {
	if s == nil {
		return 0
	}
	return len(s.watchlist)
}

// Guard returns an error if any watched live secret value appears in b.
// It is called on the encoded manifest and on every encoded artifact
// immediately before anything is published.
//
// A false positive here is possible in principle - a payload could
// contain a secret-looking string that happens to equal a short token -
// and is accepted deliberately. The failure mode of a false positive is
// a backup job that fails with a clear message naming the secret kind; the
// failure mode of a false negative is a credential in an archive that
// will be copied to a second host. That is not a trade this package will
// make quietly, and there is deliberately no flag to turn the guard off.
func (s *Sanitizer) Guard(what string, b []byte) error {
	if s == nil || len(s.watchlist) == 0 {
		return fmt.Errorf("backup: the secret byte guard is not armed, so %s cannot be checked; refusing to publish rather than claiming a check that did not happen", what)
	}
	for _, secret := range s.watchlist {
		if idx := indexBytes(b, secret); idx >= 0 {
			return fmt.Errorf("%w: %s contains a live credential at byte offset %d; it was not written, and no part of the generation was published",
				ErrSecretExposed, what, idx)
		}
	}
	return nil
}

// secretScanner wraps an io.Writer and fails the write if any watched
// live secret crosses it. It exists because the artifacts are unbounded
// streams and a guard that needed the whole artifact in memory to check
// it would force this package to buffer a dataset stream in order to
// look for a 40-byte credential - which is a worse trade than the one it
// is avoiding.
//
// The scan is exact across chunk boundaries: the last maxSecretLen-1
// bytes of the previous chunk are prepended to the next one, so a secret
// split across two reads is still found. The cost is a copy of that
// overlap per chunk, which is bounded by the longest watched secret and
// is nothing next to the payload.
type secretScanner struct {
	watchlist []string
	overlap   []byte
	// longest is len of the longest watched secret, and therefore the
	// overlap size that makes the scan exact.
	longest int
	what    string
}

// newSecretScanner returns a scanner for b's content, or nil when there
// is nothing to look for. Nil is a valid no-op scanner target so a caller
// does not have to branch.
func (s *Sanitizer) newSecretScanner(what string) *secretScanner {
	if s == nil || len(s.watchlist) == 0 {
		return nil
	}
	longest := 0
	for _, secret := range s.watchlist {
		if len(secret) > longest {
			longest = len(secret)
		}
	}
	return &secretScanner{watchlist: s.watchlist, longest: longest, what: what}
}

// Write implements io.Writer, returning an error before the bytes reach
// the underlying writer if a watched secret is present.
func (sc *secretScanner) Write(p []byte) (int, error) {
	if sc == nil {
		return len(p), nil
	}
	window := make([]byte, 0, len(sc.overlap)+len(p))
	window = append(window, sc.overlap...)
	window = append(window, p...)
	for _, secret := range sc.watchlist {
		if idx := indexBytes(window, secret); idx >= 0 {
			// Report an offset into the whole stream, not into the
			// window, so the message points at a byte position an
			// operator can act on.
			return 0, fmt.Errorf("%w: %s contains a live credential; it was not written, and no part of the generation was published",
				ErrSecretExposed, sc.what)
		}
	}
	// Keep the tail that a future chunk could need.
	keep := sc.longest - 1
	if keep > 0 {
		if keep >= len(window) {
			sc.overlap = append(sc.overlap[:0], window...)
		} else {
			sc.overlap = append(sc.overlap[:0], window[len(window)-keep:]...)
		}
	}
	return len(p), nil
}

// indexBytes is bytes.Index without the import in this file's hot path;
// the guard runs on whole files, so the allocation-free call is worth
// the two lines.
func indexBytes(haystack []byte, needle string) int {
	if len(needle) == 0 {
		return -1
	}
	return bytesIndex(haystack, []byte(needle))
}

// bytesIndex is a minimal substring search. bytes.Index would do; the
// wrapper exists so the call site reads as an intent ("does this content
// contain this secret") rather than as a string search.
func bytesIndex(h, n []byte) int {
	if len(n) > len(h) {
		return -1
	}
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if h[i+j] != n[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// sortedSecretKinds is used by tests and by the audit's error messages
// to name the deny list in a stable order.
func sortedSecretKinds() []SecretKind {
	out := make([]SecretKind, 0, len(DenyList))
	for _, f := range DenyList {
		out = append(out, f.Kind)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
