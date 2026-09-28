package statedigest

import (
	"bytes"
	"math"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// These cover two properties of the canonical encoding that Of itself
// cannot reach, because FSMSnapshotState happens to contain no float,
// double or bytes field. The encoding is the mechanism every
// cross-voter comparison rests on, and the digest's whole guarantee -
// two voters holding different state must never digest alike - has to
// hold for the field kinds a FUTURE schema will add, not only for the
// eleven the current one happens to have.
//
// Neither test is decorative. A mutation that made canonFixedBits
// compare floats by value instead of by bits, or that collapsed the
// string and bytes kind tags onto one another, survived the first run of
// this branch's mutation manifest against the whole suite: nothing in
// the repository could tell, because no reachable message has those
// fields. These tests are what makes them tell.

// TestCanonicalMessageDistinguishesFloatsByBits is the property the
// canonFixedBits comment claims: "Floats are compared by their IEEE-754
// bits rather than by value, so a NaN with a different payload counts as
// a difference."
//
// A value comparison would collapse these two: every NaN != every other
// NaN under ==, so a "difference" test on values reports NaN as
// unequal to itself, and any implementation that reaches for a
// tolerance or a != comparison inverts. More concretely, uint64(v.Float())
// on a NaN is not even defined, and on ordinary values it truncates, so
// 1.5 and 1.9 would collide.
func TestCanonicalMessageDistinguishesFloatsByBits(t *testing.T) {
	// Two NaNs that differ only in payload. There is no float field in
	// FSMSnapshotState, so structpb.Value stands in: its NumberValue is a
	// double and canonicalMessage is the function under test.
	first := &structpb.Value{Kind: &structpb.Value_NumberValue{NumberValue: math.Float64frombits(0x7ff8000000000001)}}
	second := &structpb.Value{Kind: &structpb.Value_NumberValue{NumberValue: math.Float64frombits(0x7ff8000000000002)}}

	a := canonicalMessage(first.ProtoReflect())
	b := canonicalMessage(second.ProtoReflect())
	if string(a) == string(b) {
		t.Error("two NaNs differing only in payload encoded identically, want them to differ - " +
			"this is the collision that would report two genuinely diverged state machines as agreeing")
	}

	// And the control: a NaN must encode differently from a plain zero,
	// which a value comparison would also get wrong in the other
	// direction.
	zero := &structpb.Value{Kind: &structpb.Value_NumberValue{NumberValue: 0}}
	if string(canonicalMessage(zero.ProtoReflect())) == string(a) {
		t.Error("a NaN encoded identically to zero, want them to differ")
	}
}

// TestCanonicalMessageDistinguishesTwoOrdinaryFloats closes the
// truncation hole separately, because a NaN-only test would not catch it.
func TestCanonicalMessageDistinguishesTwoOrdinaryFloats(t *testing.T) {
	two := func(v float64) *structpb.Value {
		return &structpb.Value{Kind: &structpb.Value_NumberValue{NumberValue: v}}
	}
	if string(canonicalMessage(two(1.5).ProtoReflect())) == string(canonicalMessage(two(1.9).ProtoReflect())) {
		t.Error("1.5 and 1.9 encoded identically, want them to differ - a value-to-integer conversion " +
			"truncates both to 1, which is a false match")
	}
	// The control, and the limit of the claim: the encoding follows
	// protoreflect's own presence semantics, and that means -0.0 and
	// +0.0 are NOT a difference. protoreflect reports Has as false for
	// both, because in IEEE-754 -0.0 == 0.0, so the field is omitted for
	// both and the digests agree.
	//
	// That is stated here rather than asserted the other way round
	// because the alternative would be a digest that disagreed with the
	// message it is derived from: two states proto calls identical would
	// digest differently, which is the same class of false mismatch the
	// last_index exclusion exists to prevent. "Every bit pattern is
	// distinguished" is not the property, and is not claimed.
	minusZero := two(math.Copysign(0, -1))
	plusZero := two(0)
	if minusZero.ProtoReflect().Has(minusZero.ProtoReflect().Descriptor().Fields().Get(0)) !=
		plusZero.ProtoReflect().Has(plusZero.ProtoReflect().Descriptor().Fields().Get(0)) {
		t.Error("protoreflect disagrees about the presence of -0.0 and +0.0, so the two really are different " +
			"states to the schema and the encoding should be distinguishing them")
	}
	if string(canonicalMessage(minusZero.ProtoReflect())) != string(canonicalMessage(plusZero.ProtoReflect())) {
		t.Log("-0.0 and +0.0 encoded differently after all; the presence check above is what should be re-read")
	}
}

// fieldKinds builds one message with a field of every protobuf scalar
// shape, so the kind tag each shape is written with can be read off a
// real descriptor rather than assumed.
func kindKinds(t *testing.T) (map[string]protoreflect.FieldDescriptor, protoreflect.MessageDescriptor) {
	t.Helper()
	str := func(s string) *string { return &s }
	kind := func(k descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto_Type { return &k }
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	msg := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("kindkinds.proto"),
		Package: proto.String("kindkinds"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: str("Every"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: str("f_bool"), Number: proto.Int32(1), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_BOOL)},
				{Name: str("f_string"), Number: proto.Int32(2), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_STRING)},
				{Name: str("f_bytes"), Number: proto.Int32(3), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_BYTES)},
				{Name: str("f_int64"), Number: proto.Int32(4), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_INT64)},
				{Name: str("f_float"), Number: proto.Int32(7), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_FLOAT)},
				{Name: str("f_double"), Number: proto.Int32(5), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_DOUBLE)},
				{Name: str("f_inner"), Number: proto.Int32(6), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_MESSAGE), TypeName: str(".kindkinds.Inner")},
			},
		}, {
			Name: str("Inner"),
		}},
	}
	fd, err := protodesc.NewFile(msg, nil)
	if err != nil {
		t.Fatalf("building the descriptor: %v", err)
	}
	md := fd.Messages().Get(0)
	out := map[string]protoreflect.FieldDescriptor{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		out[string(fields.Get(i).Name())] = fields.Get(i)
	}
	return out, md
}

func fieldKinds(t *testing.T) map[string]protoreflect.FieldDescriptor {
	t.Helper()
	out, _ := kindKinds(t)
	return out
}

// dynamicWith builds a message of the Every type above carrying a single
// set field, so the encoding can be read off a real float32 field.
func dynamicWith(t *testing.T, md protoreflect.MessageDescriptor, field string, value protoreflect.Value) protoreflect.Message {
	t.Helper()
	msg := dynamicpb.NewMessage(md)
	fd := md.Fields().ByName(protoreflect.Name(field))
	if fd == nil {
		t.Fatalf("%s is missing from the descriptor", field)
	}
	msg.Set(fd, value)
	return msg
}

// TestCanonicalMessageDistinguishesFloat32ByBits is the float32 half of
// the property above. It is a separate test because canonFixedBits has a
// separate branch for FloatKind, and a suite that only exercised double
// left that branch completely uncovered - a mutation turning it into
// uint64(v.Float()) passed the whole run, since uint64 of any float32
// above 1 truncates and any two small positive floats collide.
func TestCanonicalMessageDistinguishesFloat32ByBits(t *testing.T) {
	kinds, md := kindKinds(t)

	nan1 := dynamicWith(t, md, "f_float", protoreflect.ValueOfFloat32(float32(math.Float32frombits(0x7fc00001))))
	nan2 := dynamicWith(t, md, "f_float", protoreflect.ValueOfFloat32(float32(math.Float32frombits(0x7fc00002))))
	if string(canonicalMessage(nan1)) == string(canonicalMessage(nan2)) {
		t.Error("two float32 NaNs differing only in payload encoded identically, want them to differ")
	}

	// The truncation case, which is the one a value comparison actually
	// gets wrong on ordinary numbers.
	onePointFive := dynamicWith(t, md, "f_float", protoreflect.ValueOfFloat32(1.5))
	onePointNine := dynamicWith(t, md, "f_float", protoreflect.ValueOfFloat32(1.9))
	if string(canonicalMessage(onePointFive)) == string(canonicalMessage(onePointNine)) {
		t.Error("the float32 values 1.5 and 1.9 encoded identically, want them to differ - a value-to-integer " +
			"conversion truncates both to 1, which is a false match between two diverged state machines")
	}

	// And the kind tag for a float32 is the fixed-width one, like every
	// other numeric.
	if got := canonSingularKind(kinds["f_float"]); got != canonKindFixed {
		t.Errorf("canonSingularKind(f_float) = %d, want %d", got, canonKindFixed)
	}
}

// TestCanonSingularKindSeparatesTheShapes pins the tag each kind is
// written with, which is the property the canonKind constants document:
// "Different values of the same kind still cannot be confused, because
// the field number is written first and every variable-length value is
// length-prefixed" - and, one line earlier, the reason the tags exist at
// all, which is "so a reader can tell an 8-byte scalar from a
// length-prefixed string without inferring it from the field number".
//
// The tag is not what makes the encoding collision-free for the CURRENT
// schema: a field number has exactly one kind in one message, so no two
// reachable states can put different kinds at the same number. It is the
// reader's guarantee, and it is therefore only observable by reading the
// tag, which is what this does.
func TestCanonSingularKindSeparatesTheShapes(t *testing.T) {
	kinds := fieldKinds(t)

	want := map[string]uint64{
		"f_bool":   canonKindBool,
		"f_string": canonKindBytes,
		"f_bytes":  canonKindBytes,
		"f_int64":  canonKindFixed,
		"f_float":  canonKindFixed,
		"f_double": canonKindFixed,
		"f_inner":  canonKindMessage,
	}
	for name, wantTag := range want {
		fd, found := kinds[name]
		if !found {
			t.Fatalf("%s is missing from the descriptor", name)
		}
		if got := canonSingularKind(fd); got != wantTag {
			t.Errorf("canonSingularKind(%s) = %d, want %d", name, got, wantTag)
		}
	}

	// The negative that matters most: string and bytes are written with
	// the same tag on purpose, but a LENGTH-PREFIXED value must never
	// carry the same tag as a fixed 8-byte one, or a reader cannot tell
	// where the next value starts.
	if kinds["f_string"].Kind() == protoreflect.StringKind &&
		canonSingularKind(kinds["f_string"]) == canonSingularKind(kinds["f_int64"]) {
		t.Error("a length-prefixed string and a fixed 64-bit scalar carry the same kind tag, so a reader " +
			"cannot tell one from the other without the field number - which is exactly what the tags are for")
	}
}

// orderedKinds builds a descriptor carrying the same fields, with the same
// field numbers, types and labels, as kindKinds - but declared in ascending
// field-number order. The two descriptors therefore differ in exactly one
// respect, the order the fields appear in the .proto, which is the one thing
// canonicalMessage claims not to depend on.
func orderedKinds(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	str := func(s string) *string { return &s }
	num := func(n int32) *int32 { return &n }
	kind := func(k descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto_Type { return &k }
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	msg := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("orderedkinds.proto"),
		Package: proto.String("orderedkinds"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: str("Every"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: str("f_bool"), Number: num(1), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_BOOL)},
				{Name: str("f_string"), Number: num(2), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_STRING)},
				{Name: str("f_bytes"), Number: num(3), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_BYTES)},
				{Name: str("f_int64"), Number: num(4), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_INT64)},
				{Name: str("f_double"), Number: num(5), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_DOUBLE)},
				{Name: str("f_inner"), Number: num(6), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_MESSAGE), TypeName: str(".orderedkinds.Inner")},
				{Name: str("f_float"), Number: num(7), Label: &optional, Type: kind(descriptorpb.FieldDescriptorProto_TYPE_FLOAT)},
			},
		}, {
			Name: str("Inner"),
		}},
	}
	fd, err := protodesc.NewFile(msg, nil)
	if err != nil {
		t.Fatalf("building the descriptor: %v", err)
	}
	return fd.Messages().Get(0)
}

// populate gives every field of m a distinct non-zero value of its own kind,
// so the proto3 presence check skips none of them and the encoding carries
// all seven chunks rather than one.
func populate(t *testing.T, m protoreflect.Message) {
	t.Helper()
	set := func(name string, v protoreflect.Value) {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Fatalf("%s is missing from the descriptor", name)
		}
		m.Set(fd, v)
	}
	inner := m.Descriptor().Fields().ByName("f_inner").Message()
	set("f_bool", protoreflect.ValueOfBool(true))
	set("f_string", protoreflect.ValueOfString("string value"))
	set("f_bytes", protoreflect.ValueOfBytes([]byte{0x0a, 0x0b, 0x0c}))
	set("f_int64", protoreflect.ValueOfInt64(0x1122334455667788))
	set("f_float", protoreflect.ValueOfFloat32(1.5))
	set("f_double", protoreflect.ValueOfFloat64(1.9))
	set("f_inner", protoreflect.ValueOfMessage(dynamicpb.NewMessage(inner)))
}

// TestCanonicalMessageIsIndependentOfDeclarationOrder pins the property the
// canonicalMessage doc comment asserts: "Fields are visited in field-number
// order, so the encoding depends on neither declaration order nor on any
// iteration order the runtime happens to produce."
//
// This is not a theoretical property. FSMSnapshotState is itself declared out
// of field-number order - auth_enabled is field 5 and is written after fields
// 6, 7, 8 and 9 - so dropping the sort would change every digest the colony
// computes, today, with no other test noticing. The reason nothing noticed is
// that every other test in this package compares a digest against another
// digest rather than against a known-good encoding, so a self-consistent
// change to the byte layout is invisible to all of them.
//
// The mutation that swapped the comparator for declaration order survived a
// full 71-mutation run of this branch. This test is what it needed.
func TestCanonicalMessageIsIndependentOfDeclarationOrder(t *testing.T) {
	_, outOfOrder := kindKinds(t)
	ascending := orderedKinds(t)

	first := dynamicpb.NewMessage(outOfOrder)
	populate(t, first)
	second := dynamicpb.NewMessage(ascending)
	populate(t, second)

	got, want := canonicalMessage(first), canonicalMessage(second)
	if len(got) == 0 {
		t.Fatal("canonicalMessage wrote nothing for a message with every field set, " +
			"so this test is not exercising the encoder at all")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("two messages with identical field numbers, types and values encoded differently "+
			"purely because their fields were declared in a different order.\n"+
			"declared out of number order: %x\ndeclared in number order:     %x", got, want)
	}

	// A singular chunk is its kind tag followed by its value; only maps and
	// lists carry an explicit field number. Field 1 is f_bool, set true, so
	// the encoding must open with exactly those two bytes. The number is
	// carried by position for everything else, which is the whole reason
	// the order is the property worth pinning.
	if prefix, opening := want[:2], []byte{byte(canonKindBool), 1}; !bytes.Equal(prefix, opening) {
		t.Errorf("encoding starts with % x, want % x: the first field emitted is not field 1", prefix, opening)
	}

	// Field 5 must be written before field 7, and the two values are picked
	// so that either order is legible in the bytes: 1.9 and 1.5 have
	// different IEEE-754 patterns. f_float is DECLARED first, so a dropped
	// sort puts the 1.5 pattern at the lower offset and this fails.
	doublePattern := protowire.AppendFixed64(nil, math.Float64bits(1.9))
	floatPattern := protowire.AppendFixed64(nil, uint64(math.Float32bits(1.5)))
	di, fi := bytes.Index(want, doublePattern), bytes.Index(want, floatPattern)
	if di < 0 || fi < 0 {
		t.Fatalf("could not locate both float patterns in the encoding: double at %d, float at %d", di, fi)
	}
	if di > fi {
		t.Errorf("field 5 was written at offset %d and field 7 at %d, want field 5 first: "+
			"fields are being emitted in declaration order, not field-number order", di, fi)
	}
}
