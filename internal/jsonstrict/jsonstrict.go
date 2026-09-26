// Package jsonstrict decodes JSON while refusing to guess about
// duplicate object keys.
//
// encoding/json's rule for a repeated name is "last one wins", with
// no error, no warning, and no log line anywhere:
//
//	{ "manager_tls": true, "manager_addr": "a:17700", "manager_tls": false }
//
// parses cleanly as manager_tls=false. A file that reads one way
// behaves another, and nothing says so. That is not hypothetical:
// buzz's and sting's /usr/local/etc/apiary/restshimd.json each carried
// manager_tls and manager_tls_server_name twice, introduced by a
// hand-edit made during an incident, and the file that resulted read as
// though the operator's first intent had never been typed at all.
//
// RFC 8259 says object names SHOULD be unique. "Should" is not "must",
// which is exactly why encoding/json resolves the ambiguity silently
// instead of rejecting it - and exactly why a config file that
// reaches a node by hand-editing is the worst possible place to accept
// that ambiguity. A duplicate is always a mistake here: no code path
// in this project ever writes one (every config file is written by
// json.Marshal of a Go struct, which cannot emit one), so a duplicate
// can only have come from a human, and the human did not mean it.
//
// What this package deliberately does NOT do is enforce any other
// policy - unknown fields, casing conventions, types that don't match.
// Those are separate decisions with separate answers per file, and a
// helper that smuggles them in would get its callers rejected for
// reasons their author never agreed to. Duplicates only.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrDuplicateKey matches any error this package reports for a
// repeated object key, via errors.Is - a caller that wants to treat
// "you have a duplicate key" differently from "this file is not JSON"
// can, without having to know which of the two error types came back.
var ErrDuplicateKey = errors.New("duplicate JSON object key")

// DuplicateKeyError reports one key that appears more than once in the
// same JSON object, and where in the file both occurrences are.
//
// Path, Line, Column and Offset locate the second (offending)
// occurrence; FirstLine, FirstColumn and FirstOffset locate the one it
// duplicates. Reporting both matters: an operator who typed a key twice
// usually wants to delete one of them, and which one to delete depends
// on which of the two values they meant - the error should not make
// them go looking.
type DuplicateKeyError struct {
	// Key is the repeated name, after JSON string escapes have been
	// resolved. Two spellings of one name - "manager_tls" and
	// "manager_tls" - are the same name to any JSON
	// implementation, so they are reported as one.
	Key string

	// Path is where the object holding the duplicate sits, in a
	// JSONPath-ish spelling: "$" for the top level, "$.tls" for a
	// nested object, "$[3]" for the fourth element of an array.
	// A name that isn't a plain identifier is quoted
	// ($["manager tls"]) so the path stays unambiguous.
	Path string

	Line, Column, Offset                int
	FirstLine, FirstColumn, FirstOffset int
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("duplicate key %q at line %d, column %d (byte offset %d) in object %s, first defined at line %d, column %d (byte offset %d)",
		e.Key, e.Line, e.Column, e.Offset, e.Path, e.FirstLine, e.FirstColumn, e.FirstOffset)
}

// Is makes errors.Is(err, ErrDuplicateKey) true for this error.
func (e *DuplicateKeyError) Is(target error) bool { return target == ErrDuplicateKey }

// DuplicateKeysError reports every duplicate found in one file, for
// the case where a bad edit produced more than one. Reporting them all
// means the operator fixes the file once instead of rediscovering the
// next one on every restart.
type DuplicateKeysError struct {
	Dups []DuplicateKeyError
}

func (e *DuplicateKeysError) Error() string {
	parts := make([]string, 0, len(e.Dups))
	for i := range e.Dups {
		parts = append(parts, e.Dups[i].Error())
	}
	return strings.Join(parts, "; ")
}

// Unwrap exposes each individual DuplicateKeyError, so errors.As finds
// them whether there is one or several.
func (e *DuplicateKeysError) Unwrap() []error {
	errs := make([]error, 0, len(e.Dups))
	for i := range e.Dups {
		errs = append(errs, &e.Dups[i])
	}
	return errs
}

// Is makes errors.Is(err, ErrDuplicateKey) true for this error.
func (e *DuplicateKeysError) Is(target error) bool { return target == ErrDuplicateKey }

// Unmarshal behaves exactly like json.Unmarshal, and additionally
// refuses a document that repeats an object key. For every input that
// json.Unmarshal accepts today, the value v is left holding exactly
// what it would have held before, and the error (if any) is exactly
// the error json.Unmarshal would have returned.
//
// The duplicate check runs BEFORE the decode, deliberately: a file we
// consider unreadable should not get to half-populate a config struct
// on its way out. That costs one extra pass over a file these
// daemons read once at startup, which is the right trade against a
// silently-wrong config.
func Unmarshal(data []byte, v any) error {
	if err := RejectDuplicateKeys(data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// RejectDuplicateKeys returns nil if data holds no repeated object
// key, and otherwise a *DuplicateKeyError (one duplicate) or
// *DuplicateKeysError (several).
//
// Names are compared after JSON string escapes are resolved, because
// that is what "the same name" means to any JSON implementation, and
// they are compared case-sensitively, because JSON names are
// case-sensitive: "Manager_TLS" and "manager_tls" are two different
// names and neither duplicates the other.
//
// Only objects are checked, and only within one object: two objects in
// an array may both hold "name" without either being a duplicate, and
// only the enclosing object's names are candidates.
//
// A document that is not valid JSON is reported as the syntax error
// encoding/json would report, never as a duplicate-key error - a file
// that is both malformed and duplicated has a syntax problem to fix
// first, and saying so is more useful than saying which key to delete.
func RejectDuplicateKeys(data []byte) error {
	start := nextTokenStart(data, 0)
	if start >= len(data) {
		return nil // empty, or nothing but whitespace
	}
	switch data[start] {
	case '{', '[':
	default:
		// A bare scalar, true/null/"x"/42, has no names to repeat.
		// Whether it is otherwise well formed is json.Unmarshal's
		// question to answer, and it still will.
		return nil
	}

	w := &walker{data: data, dec: json.NewDecoder(bytes.NewReader(data)), open: start}
	if err := w.check(); err != nil {
		return w.syntaxError(data, err)
	}

	switch len(w.dups) {
	case 0:
		return nil
	case 1:
		return &w.dups[0]
	default:
		return &DuplicateKeysError{Dups: w.dups}
	}
}

// walker holds the state of one walk of one document.
type walker struct {
	data []byte
	// open is the offset of the top-level value's opening delimiter.
	open int
	dec  *json.Decoder
	dups []DuplicateKeyError
}

// check walks the whole document: its syntax, then its keys. A walk
// that fails is always a syntax failure - a duplicate is recorded, not
// raised - so the caller can hand the failure to syntaxError.
func (w *walker) check() error {
	if err := w.container(w.data[w.open], "$"); err != nil {
		return err
	}
	// The document ends where the top-level value does. Trailing
	// bytes are a syntax error, not silence, and saying nothing here
	// would let `{"a":1} {"b":2}` through as a clean read.
	if tok, err := w.dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("jsonstrict: unexpected %v after the top-level value, at byte offset %d", tok, w.off())
	}
	return nil
}

// syntaxError re-derives a walk failure as the error json.Unmarshal
// would itself have returned, so that a malformed file keeps
// producing the exact message it has always produced, down to the
// wording. The walk sees a syntax error a token at a time and
// therefore blames a slightly later byte than the full-document scan
// does ("invalid character 'o' in literal null" where
// encoding/json says "invalid character 'n' looking for beginning of
// object key string"); both are right about a file that is broken
// either way, and an operator grepping a log for the message they
// were given last week should still find it. The second parse happens
// only on the failure path, from a daemon that is about to refuse to
// start, so the cost never lands on a file that loads.
//
// If the document turns out to be valid after all - which would mean
// the walk was wrong, not the file - the walk's own error is returned
// rather than a nil, so the caller never gets a quiet success it did
// not earn.
func (w *walker) syntaxError(data []byte, walkErr error) error {
	var probe json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	return walkErr
}

// object walks one JSON object, from just after its '{' to its '}'.
func (w *walker) object(path string) error {
	// seen maps a name to the byte offset of the first place it was
	// defined in THIS object. It is per-call on purpose: an object's
	// names are only in conflict with its own.
	seen := make(map[string]int)
	for w.dec.More() {
		keyOff := nextTokenStart(w.data, w.off())
		tok, err := w.dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			// Unreachable on any input the decoder accepts: every
			// token in an object's key position is a string.
			return fmt.Errorf("jsonstrict: expected a key in object %s at byte offset %d, got %v", path, w.dec.InputOffset(), tok)
		}
		if first, dup := seen[key]; dup {
			w.dups = append(w.dups, w.duplicate(key, path, keyOff, first))
		} else {
			seen[key] = keyOff
		}
		if err := w.value(w.off(), memberPath(path, key)); err != nil {
			return err
		}
	}
	_, err := w.dec.Token() // the closing '}'
	return err
}

// array walks one JSON array, from just after its '[' to its ']'. Its
// elements get no duplicate checking of their own - each element is a
// separate scope, and the walk descends into each in turn so nested
// objects are still checked.
func (w *walker) array(path string) error {
	for i := 0; w.dec.More(); i++ {
		if err := w.value(w.off(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	_, err := w.dec.Token() // the closing ']'
	return err
}

// value walks the member or element value that begins at or after
// prev, which is the offset just past the token that introduced it -
// a key, an element separator, or an opening delimiter.
func (w *walker) value(prev int, path string) error {
	start := nextTokenStart(w.data, prev)
	if start < len(w.data) {
		switch w.data[start] {
		case '{':
			return w.container('{', path)
		case '[':
			return w.container('[', path)
		}
	}
	// A scalar. Its bytes are skipped whole - and as a RawMessage,
	// not as a float64, so a value too large for float64 (1e999)
	// passes here exactly as it passes json.Unmarshal into a field
	// that can hold it. What a value contains is none of this
	// package's business.
	var raw json.RawMessage
	return w.dec.Decode(&raw)
}

// container consumes the opening delimiter at the current position
// (already known from the bytes) and walks what it contains.
func (w *walker) container(delim byte, path string) error {
	tok, err := w.dec.Token()
	if err != nil {
		return err
	}
	got, ok := tok.(json.Delim)
	if !ok || byte(got) != delim {
		return fmt.Errorf("jsonstrict: expected %q at byte offset %d, got %v", delim, w.dec.InputOffset(), tok)
	}
	if delim == '{' {
		return w.object(path)
	}
	return w.array(path)
}

func (w *walker) duplicate(key, path string, off, firstOff int) DuplicateKeyError {
	line, column := w.lineColumn(off)
	firstLine, firstColumn := w.lineColumn(firstOff)
	return DuplicateKeyError{
		Key:         key,
		Path:        path,
		Line:        line,
		Column:      column,
		Offset:      off,
		FirstLine:   firstLine,
		FirstColumn: firstColumn,
		FirstOffset: firstOff,
	}
}

// off is the decoder's current input-stream offset, as an int.
func (w *walker) off() int { return int(w.dec.InputOffset()) }

// lineColumn turns a byte offset into a 1-based line and column.
func (w *walker) lineColumn(offset int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(w.data) {
		offset = len(w.data)
	}
	head := w.data[:offset]
	return 1 + bytes.Count(head, []byte{'\n'}), offset - (bytes.LastIndexByte(head, '\n') + 1) + 1
}

// nextTokenStart returns the offset of the first byte of the next
// token at or after prev, skipping JSON whitespace and the structural
// separators (',' and ':') that may sit between. Those two cannot
// begin a token, so skipping them is unambiguous, and it is what lets
// the walk report the offset of a key's opening quote rather than of
// the punctuation in front of it.
func nextTokenStart(data []byte, prev int) int {
	i := prev
	if i < 0 {
		i = 0
	}
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\r', '\n', ',', ':':
			i++
		default:
			return i
		}
	}
	return i
}

// memberPath names the member key of the object at path.
func memberPath(path, key string) string {
	if isIdentifier(key) {
		return path + "." + key
	}
	return path + "[" + strconv.Quote(key) + "]"
}

// isIdentifier reports whether key can be written as a bare JSONPath
// name. Anything else gets quoted, so a key with a dot or a space in
// it cannot produce a path that reads as some other key.
func isIdentifier(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
