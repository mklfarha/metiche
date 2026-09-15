package coordination

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokenizeKeepsShortTechnicalWords(t *testing.T) {
	got := Tokenize("JWT api UI db", 32)
	want := []string{"jwt", "api", "ui", "db"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %v, want %v", got, want)
	}
}

func TestTokenizeSplitsIdentifiers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"localStorage", []string{"local", "storage"}},
		{"HTTPServer", []string{"http", "server"}},
		{"session_token_store", []string{"session", "token", "store"}},
		{"auth-jwt-cookie", []string{"auth", "jwt", "cookie"}},
		{"web/src/auth/session.ts", []string{"web", "src", "auth", "session", "ts"}},
		{"oauth2Client", []string{"oauth2", "client"}},
	}
	for _, tc := range cases {
		if got := Tokenize(tc.in, 32); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Tokenize(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTokenizeDropsStopwordsSingleCharsAndNumbers(t *testing.T) {
	got := Tokenize("Never store the x in a 2024 cache, only in a cookie", 32)
	want := []string{"store", "cache", "cookie"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %v, want %v", got, want)
	}
}

func TestTokenizeLowercasesDedupesAndFoldsPlurals(t *testing.T) {
	got := Tokenize("Tokens TOKEN token cookies Cookie status class redis", 32)
	want := []string{"token", "cookie", "status", "class", "redis"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %v, want %v", got, want)
	}
}

func TestTokenizeCapsAndIsStable(t *testing.T) {
	text := "alpha bravo charlie delta echo foxtrot golf hotel"
	got := Tokenize(text, 3)
	if want := []string{"alpha", "bravo", "charlie"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize cap = %v, want %v", got, want)
	}
	for i := 0; i < 5; i++ {
		if again := Tokenize(text, 3); !reflect.DeepEqual(again, got) {
			t.Fatalf("run %d = %v, want %v", i, again, got)
		}
	}
	if Tokenize(text, 0) != nil || Tokenize("   ", 5) != nil {
		t.Error("a zero cap or blank text must return nothing")
	}
}

func TestTokenizeClipsToTheColumn(t *testing.T) {
	long := strings.Repeat("abcdefgh", 6) // 48 bytes, one word
	got := Tokenize(long, 5)
	if len(got) != 1 || len(got[0]) != TokenMaxBytes {
		t.Fatalf("Tokenize(long) = %v, want one %d-byte token", got, TokenMaxBytes)
	}
}

// The words signal on the spec's own example: the plan and the decision share
// enough tokens to be paired.
func TestTokenizeSharedWordsOnTheSpecExample(t *testing.T) {
	decision := Tokenize("Sessions are a signed JWT in an httpOnly, Secure cookie. Never store tokens in localStorage or sessionStorage.", 32)
	plan := Tokenize("store the session token in localStorage after login", 32)
	in := map[string]bool{}
	for _, tok := range decision {
		in[tok] = true
	}
	shared := 0
	for _, tok := range plan {
		if in[tok] {
			shared++
		}
	}
	if shared < 2 {
		t.Fatalf("shared %d tokens (decision %v, plan %v), want at least 2", shared, decision, plan)
	}
}
