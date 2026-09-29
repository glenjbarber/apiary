package manager

// Helpers for ADR-0147 Part 3's tests. Kept in their own file so the
// tests that carry the security claim read as assertions rather than as
// setup.

import (
	"os"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func writeRawFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func assertModeIs0600(t *testing.T, path, when string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: stat %s: %v", when, path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("%s: %s is mode %04o, want 0600; the mode is the security property, not a style choice", when, path, got)
	}
}

// copyStoreFile models a second Comb holding the same authorization
// file, byte for byte, which is what a stale copy after a leadership
// change actually looks like.
func copyStoreFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("reading %s: %v", from, err)
	}
	if err := os.WriteFile(to, body, 0o600); err != nil {
		t.Fatalf("writing %s: %v", to, err)
	}
}

// lookupExternalMessage resolves a message in the compiled api/rpc
// schema by its full name.
//
// Read out of the global registry rather than a hard-coded list of Go
// types, so a test that names a message which no longer exists fails
// loudly instead of being deleted along with the field it was checking.
func lookupExternalMessage(t *testing.T, fullName string) protoreflect.MessageDescriptor {
	t.Helper()
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(fullName))
	if err != nil {
		t.Fatalf("looking up %s in the compiled schema: %v; a test that cannot find the message it is inspecting would pass vacuously", fullName, err)
	}
	msg, ok := desc.(protoreflect.MessageDescriptor)
	if !ok {
		t.Fatalf("%s is a %T, want a message descriptor", fullName, desc)
	}
	return msg
}

func fieldNames(msg protoreflect.MessageDescriptor) []string {
	fields := msg.Fields()
	out := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		out = append(out, string(fields.Get(i).Name()))
	}
	return out
}

// fieldNamesByEitherName indexes a message's fields under BOTH their
// proto name and their JSON name.
//
// Both, deliberately: a test asserting "the wire has an
// approving_node_id field" means the proto name, while a test asserting
// "an operator's tooling sees approvingNodeId" means the JSON one, and
// having to remember which is which at every call site is how a
// vacuous test gets written.
func fieldNamesByEitherName(msg protoreflect.MessageDescriptor) map[string]protoreflect.FieldDescriptor {
	fields := msg.Fields()
	out := make(map[string]protoreflect.FieldDescriptor, fields.Len()*2)
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		out[string(f.Name())] = f
		out[f.JSONName()] = f
	}
	return out
}
