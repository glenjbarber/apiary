package raft

// Canonical FSM state digest (ADR-0143).
//
// The problem this exists to solve: a matching last_log_index and
// applied_index on every voter proves the raft LOGS agree, not that the
// state machines do. A build that changes what an apply function does -
// as the 2026-09-26 audit's four functions did - can apply the same
// committed entry to different results on different voters, and nothing
// in raft will ever notice. Comparing a digest of each voter's own state
// turns that silence into a fact the operator can see.
//
// The digest is computed over internalpb.FSMSnapshotState, which is by
// construction the FSM's entire ephemeral state (SnapshotState copies
// every field, and Restore reads them all back). That is deliberate: it
// means the digest cannot silently omit a state field, because it is
// derived from the same message raft's own snapshot uses rather than from
// a hand-maintained list of sections a later commit could forget to
// update.
//
// The encoding is walked from the message DESCRIPTOR for the same
// reason, and map entries are emitted in sorted key order, so two voters
// holding identical state produce identical bytes on every process. A
// digest built on proto.Marshal output over Go maps would differ between
// processes on identical state, and would be worse than useless: it
// would report divergence on a perfectly healthy colony.

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strconv"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// canonKind tags each encoded value's shape, so a reader can tell an
// 8-byte scalar from a length-prefixed string without inferring it from
// the field number. Different values of the same kind still cannot be
// confused, because the field number is written first and every
// variable-length value is length-prefixed.
const (
	canonKindFixed   = 1 // 8 bytes, little-endian: every numeric and enum kind
	canonKindBool    = 2 // 1 byte, 0 or 1
	canonKindBytes   = 3 // varint length, then raw bytes: string and bytes
	canonKindMessage = 4 // varint length, then the recursive encoding
	canonKindList    = 5 // varint count, then each element's encoding
	canonKindMap     = 6 // varint count, then each entry's key and value
)

// lastIndexFieldName is the one FSMSnapshotState field deliberately kept
// OUT of the digest.
//
// last_index is this node's own progress, not shared state. Folding it
// in would make every voter disagree with every other voter at every
// instant the cluster was merely making progress, so a digest mismatch
// could never distinguish "these state machines diverged" from "this
// sample was taken while entries were still being applied". Leaving it
// out is what makes the digest a statement about state. The applied
// index travels beside it, so a consumer can reason about sampling.
const lastIndexFieldName = "last_index"

// stateDigestOf returns a lowercase hex SHA-256 over a canonical encoding
// of state, excluding last_index.
//
// The empty state has a real, non-empty digest. An empty RETURN value is
// therefore never produced here, which is what lets raftd's StatusResponse
// treat an empty state_digest as "not observed" with no separate presence
// flag to keep in sync.
//
// Unknown fields are skipped, and that is safe rather than a gap. An
// unknown field can only be present in a message one build wrote and an
// older build parsed, so whichever build produced the difference
// necessarily knows the field and includes it in its own digest - which
// is exactly the mismatch this exists to surface. A state differing only
// in a field no build understands cannot be produced by any build at all.
func stateDigestOf(state *internalpb.FSMSnapshotState) string {
	var buf []byte
	if state != nil {
		buf = canonicalMessage(state.ProtoReflect())
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// canonicalMessage encodes m. Fields are visited in field-number order, so
// the encoding depends on neither declaration order nor on any iteration
// order the runtime happens to produce.
func canonicalMessage(m protoreflect.Message) []byte {
	if !m.IsValid() {
		return nil
	}

	fields := m.Descriptor().Fields()
	descs := make([]protoreflect.FieldDescriptor, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Name() == lastIndexFieldName {
			continue
		}
		descs = append(descs, fd)
	}
	sort.Slice(descs, func(i, j int) bool { return descs[i].Number() < descs[j].Number() })

	var out []byte
	for _, fd := range descs {
		switch {
		case fd.IsMap():
			mmap := m.Get(fd).Map()
			if mmap.Len() == 0 {
				continue
			}
			out = protowire.AppendVarint(out, uint64(fd.Number()))
			out = protowire.AppendVarint(out, canonKindMap)
			// Appended, not assigned: canonicalMap builds its own buffer
			// and returns it, so assigning would silently drop everything
			// encoded so far - leaving only the last map in the message
			// counted towards the digest at all.
			out = append(out, canonicalMap(mmap, fd)...)
		case fd.IsList():
			list := m.Get(fd).List()
			if list.Len() == 0 {
				continue
			}
			out = protowire.AppendVarint(out, uint64(fd.Number()))
			out = protowire.AppendVarint(out, canonKindList)
			out = protowire.AppendVarint(out, uint64(list.Len()))
			for i := 0; i < list.Len(); i++ {
				out = appendTaggedValue(out, list.Get(i), fd)
			}
		default:
			// A proto3 singular scalar has no presence, so Has is true
			// exactly when the value is non-zero. Omitting a zero-valued
			// scalar is not a loss: two states differing only there are
			// the same state by proto3's own definition, and the digest
			// must agree with that or it would report divergence between
			// two replicas that are in fact identical.
			if !m.Has(fd) {
				continue
			}
			out = appendTaggedValue(out, m.Get(fd), fd)
		}
	}
	return out
}

// canonicalMap encodes every entry of mmap in sorted key order. This is
// the whole reason the encoding is not proto.Marshal output: Go map
// iteration order is randomized per process, so an unsorted walk would
// produce a different digest on every call for identical state.
//
// fd is the map FIELD's descriptor, not the map's. A protoreflect.Map
// carries no kind information of its own, but its field does, naming
// both the key's and the value's kind exactly - which is why this takes
// a descriptor rather than inferring one from a value's Go type.
func canonicalMap(mmap protoreflect.Map, fd protoreflect.FieldDescriptor) []byte {
	keyDesc, valDesc := fd.MapKey(), fd.MapValue()

	type entry struct {
		sortKey string
		key     protoreflect.MapKey
		val     protoreflect.Value
	}
	entries := make([]entry, 0, mmap.Len())
	mmap.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		entries = append(entries, entry{sortKey: canonMapKeySort(k, keyDesc), key: k, val: v})
		return true
	})
	// The sort key is derived from the key itself, so the emitted order
	// is a function of state alone and never of insertion or hash order.
	sort.Slice(entries, func(i, j int) bool { return entries[i].sortKey < entries[j].sortKey })

	var out []byte
	out = protowire.AppendVarint(out, uint64(len(entries)))
	for _, e := range entries {
		out = appendTaggedValue(out, e.key.Value(), keyDesc)
		out = appendTaggedValue(out, e.val, valDesc)
	}
	return out
}

// canonSingularKind maps a field's declared kind to the tag used for it.
func canonSingularKind(fd protoreflect.FieldDescriptor) uint64 {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return canonKindBool
	case protoreflect.StringKind, protoreflect.BytesKind:
		return canonKindBytes
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return canonKindMessage
	default:
		// Every remaining kind - the signed and unsigned integers, the
		// sint zigzag variants, fixed-width types, floats, doubles and
		// enums - is emitted as its raw 64-bit pattern. protoreflect
		// hands back the DECODED value for sint and enums, so this is
		// injective across kinds as well as within one.
		return canonKindFixed
	}
}

// appendTaggedValue appends v with the kind tag that introduces it, which
// is what every position in the encoding actually needs: a field, a list
// element, or a map key or value. appendCanonicalValue writes the payload
// alone, for the one caller (canonicalMessage) that has already written
// the tag.
func appendTaggedValue(out []byte, v protoreflect.Value, fd protoreflect.FieldDescriptor) []byte {
	out = protowire.AppendVarint(out, canonSingularKind(fd))
	return appendCanonicalValue(out, v, fd)
}

// appendCanonicalValue appends v, length-prefixed so its bytes cannot run
// into the next value's.
//
// fd supplies the kind. protoreflect.Value carries none of its own, and
// inferring one from the value's Go type would be a guess about runtime
// internals rather than a reading of the schema.
func appendCanonicalValue(out []byte, v protoreflect.Value, fd protoreflect.FieldDescriptor) []byte {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		if v.Bool() {
			return append(out, 1)
		}
		return append(out, 0)
	case protoreflect.StringKind:
		// Value.Bytes panics on a string field, and almost every field in
		// this state is a string. Only a genuine bytes field goes through
		// Bytes().
		str := v.String()
		out = protowire.AppendVarint(out, uint64(len(str)))
		return append(out, str...)
	case protoreflect.BytesKind:
		raw := v.Bytes()
		out = protowire.AppendVarint(out, uint64(len(raw)))
		return append(out, raw...)
	case protoreflect.MessageKind, protoreflect.GroupKind:
		inner := canonicalMessage(v.Message())
		out = protowire.AppendVarint(out, uint64(len(inner)))
		return append(out, inner...)
	default:
		return protowire.AppendFixed64(out, canonFixedBits(v, fd))
	}
}

// canonFixedBits returns v's raw 64-bit pattern.
//
// Floats are compared by their IEEE-754 bits rather than by value, so a
// NaN with a different payload counts as a difference. That is the honest
// answer: two state machines that disagree about a NaN payload have
// genuinely diverged, and collapsing them would hide exactly the class
// of bug this digest is for.
func canonFixedBits(v protoreflect.Value, fd protoreflect.FieldDescriptor) uint64 {
	switch fd.Kind() {
	case protoreflect.FloatKind:
		return uint64(math.Float32bits(float32(v.Float())))
	case protoreflect.DoubleKind:
		return math.Float64bits(v.Float())
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return uint64(uint32(int32(v.Int())))
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return uint64(v.Int())
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return uint64(v.Uint())
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return v.Uint()
	case protoreflect.EnumKind:
		return uint64(v.Enum())
	default:
		return 0
	}
}

// canonMapKeySort renders a map key as a string that orders the same way
// on every process, for every legal map key type. The prefix keeps a
// string key "1" from colliding with an integer key 1. Only determinism
// matters here, not numeric sense, so "-1" sorting before "10" is fine.
func canonMapKeySort(k protoreflect.MapKey, keyDesc protoreflect.FieldDescriptor) string {
	switch keyDesc.Kind() {
	case protoreflect.StringKind:
		return "s" + k.String()
	case protoreflect.BoolKind:
		if k.Bool() {
			return "b1"
		}
		return "b0"
	case protoreflect.Int32Kind, protoreflect.Int64Kind,
		protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind:
		return "i" + strconv.FormatInt(k.Int(), 10)
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind,
		protoreflect.Fixed32Kind, protoreflect.Fixed64Kind:
		return "u" + strconv.FormatUint(k.Uint(), 10)
	default:
		// Unreachable for every protobuf map key type. The fallback is
		// still deterministic, which is the only property the sort needs.
		return "x?"
	}
}
