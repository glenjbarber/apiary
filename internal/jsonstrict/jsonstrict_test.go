package jsonstrict

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// buzzAndSting is the file as it actually read on buzz and sting:
// manager_tls typed once with the operator's real intent, then typed
// again mid-incident, and the second spelling silently won.
const buzzAndSting = `{
  "manager_tls": true,
  "manager_addr": "buzz:17700",
  "manager_tls": false
}`

// TestRejectDuplicateKeys_RealBuzzStingFile is the incident this
// package exists for. The failure mode being pinned is not "Load
// errors" - the file did load, and that is the problem - but "the
// error, when it comes, names the key".
func TestRejectDuplicateKeys_RealBuzzStingFile(t *testing.T) {
	err := RejectDuplicateKeys([]byte(buzzAndSting))
	if err == nil {
		t.Fatal("RejectDuplicateKeys: nil, want an error for a file with manager_tls twice")
	}
	if !strings.Contains(err.Error(), `"manager_tls"`) {
		t.Errorf("error %q does not name the duplicated key", err)
	}
	if !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("errors.Is(err, ErrDuplicateKey) = false, want true (%v)", err)
	}
	var dup *DuplicateKeyError
	if !errors.As(err, &dup) {
		t.Fatalf("errors.As(*DuplicateKeyError) = false, want true (%v)", err)
	}
	if dup.Key != "manager_tls" {
		t.Errorf("dup.Key = %q, want %q", dup.Key, "manager_tls")
	}
	// The duplicate is on line 4 of the literal above - after the
	// manager_addr line an operator added in between - and the first
	// definition is on line 2. Getting these backwards would send an
	// operator to the wrong line to delete.
	if dup.Line != 4 || dup.FirstLine != 2 {
		t.Errorf("duplicate at line %d (first at line %d), want line 4 (first at line 2): %v", dup.Line, dup.FirstLine, err)
	}
	// The offsets must point at the opening quote of each key, so
	// that the byte in the file at Offset is the start of the key
	// itself rather than the comma or brace in front of it.
	for _, tc := range []struct {
		name   string
		off    int
		prefix string
	}{
		{"second", dup.Offset, `"manager_tls": false`},
		{"first", dup.FirstOffset, `"manager_tls": true,`},
	} {
		end := tc.off + len(tc.prefix)
		if got := buzzAndSting[tc.off:end]; got != tc.prefix {
			t.Errorf("%s occurrence: bytes at offset %d are %q, want %q", tc.name, tc.off, got, tc.prefix)
		}
	}
}

// TestRejectDuplicateKeys_NestedObject covers the case a top-level-only
// check would miss entirely. A duplicate three levels down is still a
// duplicate, and "duplicate key" with no path is a puzzle.
func TestRejectDuplicateKeys_NestedObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want string // substring the error must contain
	}{
		{
			name: "one level down",
			doc:  `{"tls": {"manager_tls": true, "manager_tls": false}}`,
			want: `"manager_tls"`,
		},
		{
			name: "three levels down, inside an array",
			doc:  `{"peers": [{"tls": {"ca": "/etc/ca.pem", "ca": "/tmp/other.pem"}}]}`,
			want: `"ca"`,
		},
		{
			name: "deeply nested, in the second array element",
			doc:  `{"a": {"b": [{}, {"c": {"d": [1, 2, {"e": 3, "e": 4}]}}]}}`,
			want: `"e"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectDuplicateKeys([]byte(tc.doc))
			if err == nil {
				t.Fatalf("RejectDuplicateKeys(%s) = nil, want a duplicate-key error", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the duplicated key %s", err, tc.want)
			}
			var dup *DuplicateKeyError
			if !errors.As(err, &dup) {
				t.Fatalf("errors.As(*DuplicateKeyError) = false, want true (%v)", err)
			}
			if dup.Path == "$" {
				t.Errorf("dup.Path = %q, want the path of the object that actually holds the duplicate", dup.Path)
			}
			if dup.Line != dup.FirstLine {
				t.Errorf("duplicate reported at line %d and first at line %d, want a real second line", dup.Line, dup.FirstLine)
			}
			t.Logf("path %s: %v", dup.Path, err)
		})
	}
}

// TestRejectDuplicateKeys_ReportsEveryDuplicate saves the operator the
// fix-one-restart loop: one bad edit can leave two duplicates behind.
func TestRejectDuplicateKeys_ReportsEveryDuplicate(t *testing.T) {
	doc := []byte(`{"manager_tls": true, "manager_tls": false, "http_addr": "a:1", "http_addr": "b:2", "manager_tls": true}`)
	err := RejectDuplicateKeys(doc)
	if err == nil {
		t.Fatal("RejectDuplicateKeys = nil, want an error")
	}
	var multi *DuplicateKeysError
	if !errors.As(err, &multi) {
		t.Fatalf("errors.As(*DuplicateKeysError) = false, want true (%v)", err)
	}
	if len(multi.Dups) != 3 {
		t.Errorf("reported %d duplicates, want 3: %v", len(multi.Dups), err)
	}
	for _, key := range []string{`"manager_tls"`, `"http_addr"`} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not name %s", err, key)
		}
	}
}

// TestRejectDuplicateKeys_CaseSensitive pins the rule that JSON names
// are case-sensitive. "Manager_TLS" is a different key from
// "manager_tls" and decoding to a struct that has only one of them is
// still the one behaviour this package must not change; folding case
// here would reject files that are perfectly legal JSON.
func TestRejectDuplicateKeys_CaseSensitive(t *testing.T) {
	for _, doc := range []string{
		`{"manager_tls": true, "Manager_TLS": false}`,
		`{"Manager_Tls": true, "manager_tls": false, "MANAGER_TLS": true}`,
		`{"tls": {"ca": "x", "Ca": "y", "CA": "z"}}`,
	} {
		if err := RejectDuplicateKeys([]byte(doc)); err != nil {
			t.Errorf("RejectDuplicateKeys(%s) = %v, want nil: differently-cased keys are different keys", doc, err)
		}
	}
}

// TestRejectDuplicateKeys_Accepts is everything this package must NOT
// reject. Each of these is a file that decodes today, and rejecting
// any of them would be a regression dressed up as a fix.
func TestRejectDuplicateKeys_Accepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"empty object", `{}`},
		{"empty object, padded", "  {\n}\n"},
		{"empty array", `[]`},
		{"empty nested object", `{"tls": {}}`},
		{"empty nested array", `{"peers": [[], []]}`},
		{"same key in sibling objects", `{"a": {"n": 1}, "b": {"n": 2}}`},
		{"same key in every array element", `[{"n": 1}, {"n": 2}, {"n": 3}]`},
		{"key repeated in sibling array elements, nested deeper", `{"x": [{"y": {"n": 1}}, {"y": {"n": 2}}]}`},
		{"escaped keys, none repeated", `{"manager\u005ftls": true, "http\u002daddr": "a:1"}`},
		{"a key that is a JSON keyword", `{"true": 1, "null": 2, "false": 3}`},
		{"a key holding a brace", `{"}": 1, "{": 2, ",": 3, ":": 4}`},
		{"a key holding a quote and a backslash", `{"a\"b": 1, "a\\b": 2}`},
		{"unicode key and value", "{\"k\u00e9y\": \"\u00e9\"}"},
		{"nested objects and arrays, no repeats", `{"peers": [{"n": 1, "o": {"m": 2}}, [3, 4], null, true, "s", 1.5e3]}`},
		{"null values", `{"a": null, "b": null}`},
		{"a number too large for float64", `{"big": 1e999, "neg": -1e999}`},
		{"a bare scalar document", `42`},
		{"a bare string document", `"hello"`},
		{"a bare null document", `null`},
		{"trailing whitespace", "{\"a\": 1}\n\n\t "},
		{"crlf line endings", "{\r\n\"a\": 1,\r\n\"b\": 2\r\n}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := RejectDuplicateKeys([]byte(tc.doc)); err != nil {
				t.Errorf("RejectDuplicateKeys(%s) = %v, want nil", tc.doc, err)
			}
		})
	}
}

// TestRejectDuplicateKeys_EscapedDuplicateIsADuplicate covers the other
// half of the escaping question: "\u0061" and "a" are the same name to
// every JSON implementation, so accepting one while rejecting the other
// would mean the same file is legal or illegal depending on how the
// key was typed.
func TestRejectDuplicateKeys_EscapedDuplicateIsADuplicate(t *testing.T) {
	err := RejectDuplicateKeys([]byte(`{"a": 1, "a": 2}`))
	if err == nil {
		t.Fatal("RejectDuplicateKeys = nil, want a duplicate-key error")
	}
	var dup *DuplicateKeyError
	if !errors.As(err, &dup) {
		t.Fatalf("errors.As(*DuplicateKeyError) = false, want true (%v)", err)
	}
	if dup.Key != "a" {
		t.Errorf("dup.Key = %q, want %q (escapes resolved before comparing)", dup.Key, "a")
	}
	// The name in the error is the resolved one, not the raw spelling:
	// an operator grepping the file for it must find the line they
	// need to edit.
	if strings.Contains(err.Error(), `\u0061`) {
		t.Errorf("error %q quotes the raw escape, want the resolved key name", err)
	}
}

// TestRejectDuplicateKeys_MalformedIsASyntaxError is the promise that
// this package only ever adds one new failure mode. Every malformed
// file must keep reporting the same syntax error it always reported -
// never a duplicate-key error, which would send an operator hunting a
// key that is not the problem.
func TestRejectDuplicateKeys_MalformedIsASyntaxError(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"the case every config test already uses", `{not valid json`},
		{"truncated object", `{"a": 1`},
		{"trailing comma", `{"a": 1,}`},
		{"single quotes", `{'a': 1}`},
		{"unterminated string", `{"a": "unterminated}`},
		{"two top-level values", `{"a": 1} {"b": 2}`},
		{"garbage after the top-level value", `{"a": 1} nonsense`},
		{"unclosed string in a nested object", `{"a": {"b": "c}`},
		{"a bare newline where a value belongs", "{\n\"a\":\n}"},
		{"empty file", ``},
		{"whitespace only", "  \n\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unmarshal is what every caller uses, and it must
			// keep saying exactly what json.Unmarshal said.
			var got, want any
			gotErr := Unmarshal([]byte(tc.doc), &got)
			wantErr := json.Unmarshal([]byte(tc.doc), &want)
			if wantErr == nil {
				t.Fatalf("precondition: json.Unmarshal(%q) = nil; this test case is not malformed", tc.doc)
			}
			if gotErr == nil {
				t.Fatalf("Unmarshal(%q) = nil, want the same syntax error json.Unmarshal gives", tc.doc)
			}
			if gotErr.Error() != wantErr.Error() {
				t.Errorf("Unmarshal(%q) = %q, want json.Unmarshal's own %q", tc.doc, gotErr, wantErr)
			}
			if strings.Contains(gotErr.Error(), "duplicate") {
				t.Errorf("malformed input reported as a duplicate key: %v", gotErr)
			}
			var syntax *json.SyntaxError
			if !errors.As(gotErr, &syntax) {
				t.Errorf("error %v is not a *json.SyntaxError, so it is not a syntax error an operator can act on", gotErr)
			}
		})
	}
}

// TestRejectDuplicateKeys_MalformedAndDuplicatedReportsTheSyntaxError
// covers a file that is both. The syntax error is the one to fix
// first, so it is the one to report.
func TestRejectDuplicateKeys_MalformedAndDuplicatedReportsTheSyntaxError(t *testing.T) {
	doc := []byte(`{"manager_tls": true, "manager_tls": false,`)
	err := RejectDuplicateKeys(doc)
	if err == nil {
		t.Fatal("RejectDuplicateKeys = nil, want the syntax error")
	}
	if strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %v, want the syntax error, not a duplicate-key error", err)
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Errorf("error %v is not a *json.SyntaxError", err)
	}
}

// TestUnmarshal_IdenticalToJSONUnmarshalOnValidInput is the "must not
// change behaviour for valid files" requirement, stated as a
// whole-file comparison over a realistic full config rather than as a
// spot check. Same document, decoded twice - once by encoding/json,
// once through here - into the same type from the same starting
// state, and the two results must be indistinguishable, field for
// field. A jsonstrict that dropped, coerced, or reordered anything
// would fail here even though every duplicate test passed.
func TestUnmarshal_IdenticalToJSONUnmarshalOnValidInput(t *testing.T) {
	docs := map[string]string{
		"restshimd, fully populated": `{
  "manager_addr": "buzz:17700",
  "http_addr": "127.0.0.1:8081",
  "manager_tls": true,
  "manager_tls_ca": "/usr/local/etc/apiary/managerd-ca.pem",
  "manager_tls_server_name": "managerd.buzz",
  "tls_cert": "/usr/local/etc/apiary/restshimd.cert",
  "tls_key": "/usr/local/etc/apiary/restshimd.key"
}`,
		"restshimd, empty":       `{}`,
		"restshimd, only tls on": `{"manager_tls": true}`,
		"nested objects and arrays, every type": `{
  "nodes": [
    {"id": "buzz", "tls": {"enabled": true, "ca": "/ca.pem", "sni": ["a", "b"]}},
    {"id": "sting", "tls": {"enabled": false}, "ports": [1, 2, 3], "ratio": 1.5e-3},
    {"id": "drone", "extra": {"deep": {"deeper": {"k": null}}}},
    null
  ],
  "flags": {"verbose": true, "quiet": false},
  "counts": {"0": 0, "-1": -1, "int": 9007199254740993},
  "strings": ["", "quote\"", "backslash\\", "newline\n", "unicode é", "raw \t tab"],
  "empty_things": [{}, [], {"a": {}}, [[]]]
}`,
	}

	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			var strictTarget, plainTarget any
			if err := json.Unmarshal([]byte(doc), &strictTarget); err != nil {
				t.Fatalf("precondition: json.Unmarshal: %v", err)
			}
			plainTarget = deepCopy(t, strictTarget)
			if err := Unmarshal([]byte(doc), &strictTarget); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(strictTarget, plainTarget) {
				t.Errorf("jsonstrict and encoding/json disagree\n jsonstrict: %#v\n encoding/json: %#v", strictTarget, plainTarget)
			}
		})
	}
}

// TestUnmarshal_HugeNumberReachesATargetThatCanHoldIt is why the walk
// skips scalar values as raw bytes instead of parsing them: 1e999
// overflows float64, and a helper that tripped over a number the
// config's own field type would have taken just fine would be refusing
// files it has no business refusing.
func TestUnmarshal_HugeNumberReachesATargetThatCanHoldIt(t *testing.T) {
	type target struct {
		Big  json.Number     `json:"big"`
		Raw  json.RawMessage `json:"raw"`
		Rate json.Number     `json:"rate"`
	}
	doc := []byte(`{"big": 1e999, "raw": {"nested": 1e999}, "rate": 0.125}`)

	var strictTarget, plainTarget target
	if err := json.Unmarshal(doc, &plainTarget); err != nil {
		t.Fatalf("precondition: json.Unmarshal: %v", err)
	}
	if err := Unmarshal(doc, &strictTarget); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(strictTarget, plainTarget) {
		t.Errorf("jsonstrict and encoding/json disagree: %+v vs %+v", strictTarget, plainTarget)
	}
	if strictTarget.Big.String() != "1e999" {
		t.Errorf("Big = %q, want the number preserved exactly", strictTarget.Big)
	}
}

// TestUnmarshal_LeavesTheTargetUntouchedOnADuplicate is why the
// duplicate check runs before the decode. A caller that logs the error
// and carries on - someone will - must not be holding a config
// assembled out of a file this package has already rejected.
func TestUnmarshal_LeavesTheTargetUntouchedOnADuplicate(t *testing.T) {
	type cfg struct {
		ManagerAddr string `json:"manager_addr"`
		ManagerTLS  bool   `json:"manager_tls"`
	}
	target := cfg{ManagerAddr: "preset:17700"}
	if err := Unmarshal([]byte(`{"manager_addr": "evil:1", "manager_tls": true, "manager_tls": false}`), &target); err == nil {
		t.Fatal("Unmarshal = nil, want a duplicate-key error")
	}
	if target != (cfg{ManagerAddr: "preset:17700"}) {
		t.Errorf("target = %+v, want it left exactly as it was", target)
	}
}

func deepCopy(t *testing.T, v any) any {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("re-unmarshalling: %v", err)
	}
	return out
}
