package secret

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const canary = "mtk_CANARY_secret_0123456789abcdef"

func TestSecretNeverPrints(t *testing.T) {
	s := New(canary)
	type wrap struct {
		Token Secret `json:"token"`
	}
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q %x", s, s, s, s, s, s), s.String(), s.GoString(),
	} {
		if strings.Contains(out, "CANARY") {
			t.Errorf("a Secret printed its value: %q", out)
		}
	}
	b, _ := json.Marshal(wrap{Token: s})
	if strings.Contains(string(b), "CANARY") || !strings.Contains(string(b), "redacted") {
		t.Errorf("MarshalJSON = %s", b)
	}
	var back wrap
	if err := json.Unmarshal([]byte(`{"token":"`+canary+`"}`), &back); err != nil || back.Token.Reveal() != canary {
		t.Errorf("UnmarshalJSON did not wrap the value: %v", err)
	}
	if !s.Equal(New(canary)) || s.Empty() || !(Secret{}).Empty() {
		t.Error("Equal/Empty are wrong")
	}
}

func TestRedactor(t *testing.T) {
	r := &Redactor{}
	r.Add("s3cr3t-value-xyz")
	r.Add("abc") // too short to register
	in := "detail s3cr3t-value-xyz; Authorization: Bearer   some.jwt-thing and mtk_ABCdef_123 and mbl_linksecret and abc"
	got := r.Redact(in)
	for _, leak := range []string{"s3cr3t-value-xyz", "some.jwt-thing", "ABCdef_123", "linksecret"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q survived: %s", leak, got)
		}
	}
	if !strings.Contains(got, "abc") || !strings.Contains(got, "Bearer   <redacted>") || !strings.Contains(got, "mtk_<redacted>") {
		t.Errorf("redaction shape: %s", got)
	}
}

// TestRevealIsCalledOnlyByTheMCPClient enforces docs/CLI.md §7: the bearer
// reaches the wire in one place.
func TestRevealIsCalledOnlyByTheMCPClient(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if strings.HasPrefix(rel, filepath.Join("internal", "mcpclient")) || strings.HasPrefix(rel, filepath.Join("internal", "secret")) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), ".Reveal()") {
			t.Errorf("%s calls Reveal(); only internal/mcpclient may", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
