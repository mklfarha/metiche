// Package secret holds credentials so they cannot be printed by accident.
//
// Every token the CLI reads becomes a Secret the moment it is read. Its String,
// GoString, Format and MarshalJSON all yield "<redacted>". Reveal is called in
// exactly one place, the MCP client's RoundTripper, and a test enforces that
// (docs/CLI.md §7).
package secret

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const redacted = "<redacted>"

// Secret is a credential value.
type Secret struct{ v string }

// New wraps a value and seeds the process redactor with it, so the value is
// cut out of any foreign text the CLI prints later in this run.
func New(v string) Secret {
	v = strings.TrimSpace(v)
	Default.Add(v)
	return Secret{v: v}
}

// Reveal returns the value. Only internal/mcpclient may call it.
func (s Secret) Reveal() string { return s.v }

// Empty reports whether there is no value.
func (s Secret) Empty() bool { return s.v == "" }

// Equal compares two secrets without revealing either.
func (s Secret) Equal(o Secret) bool { return s.v == o.v }

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Redactor replaces every secret value it has been given, plus anything shaped
// like a metiche credential or a bearer header, in text the CLI did not write
// itself: server error details, client CLI output, log lines.
type Redactor struct {
	mu     sync.Mutex
	values []string
}

// Default is the process-wide redactor every Secret seeds.
var Default = &Redactor{}

// minRedactLen keeps a short or empty value from shredding unrelated text.
const minRedactLen = 6

// Add registers a value to cut out of later output.
func (r *Redactor) Add(v string) {
	v = strings.TrimSpace(v)
	if len(v) < minRedactLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, have := range r.values {
		if have == v {
			return
		}
	}
	r.values = append(r.values, v)
	// Longest first, so a value that contains another is replaced whole.
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

var shapes = regexp.MustCompile(`(?i)(bearer\s+)\S+|\b(mtk|mbl|mbs)_[A-Za-z0-9_-]+`)

// Redact returns s with every known secret and credential-shaped run replaced.
func (r *Redactor) Redact(s string) string {
	r.mu.Lock()
	vals := append([]string(nil), r.values...)
	r.mu.Unlock()
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, redacted)
	}
	return shapes.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(strings.ToLower(m), "bearer") {
			i := strings.IndexFunc(m[6:], func(c rune) bool { return c != ' ' && c != '\t' })
			if i < 0 {
				return m
			}
			return m[:6+i] + redacted
		}
		return m[:4] + redacted
	})
}

// UnmarshalJSON lets a result struct decode a credential straight into a
// Secret, so it is never held as a plain string.
func (s *Secret) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = New(v)
	return nil
}
