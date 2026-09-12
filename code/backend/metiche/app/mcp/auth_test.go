package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMintTokenShape: the token must be prefixed (so a leak is greppable),
// long enough to be unguessable, and its stored form must fit
// agent.token_hash exactly — the column is VARCHAR(64) because a sha256 hex
// digest is 64 characters, and a hash that does not fit is a hash that gets
// silently truncated.
func TestMintTokenShape(t *testing.T) {
	token, hash, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		t.Errorf("token %q has no %q prefix", token, tokenPrefix)
	}
	// 32 random bytes in unpadded base64url is 43 characters.
	if got := len(token) - len(tokenPrefix); got != 43 {
		t.Errorf("token body is %d characters, want 43 (32 bytes of crypto/rand)", got)
	}
	if len(hash) != sha256.Size*2 {
		t.Errorf("hash is %d characters, want %d to fit agent.token_hash", len(hash), sha256.Size*2)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		t.Errorf("hash is not hex: %v", err)
	}
	if strings.Contains(hash, token) || strings.Contains(token, hash) {
		t.Error("the stored hash must not contain the token")
	}
}

// TestMintTokenIsUnique catches the class of bug where a token is derived from
// a uuid, a counter or the clock instead of crypto/rand: any of those produce
// collisions or predictable neighbours, and this loop would find them.
func TestMintTokenIsUnique(t *testing.T) {
	seen := make(map[string]bool, 512)
	for i := 0; i < 512; i++ {
		token, hash, err := MintToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatalf("MintToken repeated a token after %d draws", i)
		}
		if seen[hash] {
			t.Fatalf("MintToken repeated a hash after %d draws", i)
		}
		seen[token] = true
		seen[hash] = true
	}
}

func TestHashTokenIsStableAndTrims(t *testing.T) {
	token := "mtk_abcdefghijklmnop"
	a := HashToken(token)
	b := HashToken(token)
	if a != b {
		t.Errorf("HashToken is not deterministic: %s vs %s", a, b)
	}
	// The header parse can leave whitespace behind; the stored hash must not
	// depend on it, or a token works from one client and not another.
	if HashToken("  "+token+"\n") != a {
		t.Error("HashToken should ignore surrounding whitespace")
	}
	if HashToken(token+"x") == a {
		t.Error("a different token must hash differently")
	}
}

func TestVerifyToken(t *testing.T) {
	token, hash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	other, otherHash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
		hash  string
		want  bool
	}{
		{"matching pair", token, hash, true},
		{"other pair", other, otherHash, true},
		{"wrong token", other, hash, false},
		{"empty token", "", hash, false},
		{"empty hash", token, "", false},
		{"truncated hash", token, hash[:63], false},
		{"hash of the hash", hash, hash, false},
		{"token as hash", token, token, false},
		{"one byte off", token + "a", hash, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyToken(tc.token, tc.hash); got != tc.want {
				t.Errorf("VerifyToken = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVerifyTokenRejectsWrongLengthHash guards the constant-time comparison.
// subtle.ConstantTimeCompare returns 0 for unequal lengths, which is correct
// but would make a truncated stored hash look like a plain mismatch; the
// explicit length check makes that a deliberate rejection rather than luck.
func TestVerifyTokenRejectsWrongLengthHash(t *testing.T) {
	token, hash, err := MintToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{hash[:10], hash + "00", strings.ToUpper(hash)} {
		if VerifyToken(token, bad) {
			t.Errorf("VerifyToken accepted a malformed stored hash %q", bad)
		}
	}
}

func TestBearerFromHeader(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer mtk_abc", "mtk_abc"},
		{"bearer mtk_abc", "mtk_abc"},
		{"BEARER mtk_abc", "mtk_abc"},
		{"  Bearer   mtk_abc  ", "mtk_abc"},
		{"Basic mtk_abc", ""},
		{"mtk_abc", ""},
		{"Bearer", ""},
		{"", ""},
		{"Bearer ", ""},
	}
	for _, tc := range cases {
		if got := BearerFromHeader(tc.header); got != tc.want {
			t.Errorf("BearerFromHeader(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// TestSlugKey covers the member key, which is the only thing stopping one
// person from appearing twice on the board when two of their agents join at
// the same moment: uq_member_team_key can only deduplicate what hashes to the
// same key.
func TestSlugKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Ana", "ana"},
		{"Mark Farha", "mark-farha"},
		{"  ana  ", "ana"},
		{"Ana-María", "ana-mar-a"},
		{"a  b", "a-b"},
		{"!!!", ""},
		{"", ""},
		{strings.Repeat("x", 40), strings.Repeat("x", 32)},
		{strings.Repeat("ab-", 20), strings.Trim(strings.Repeat("ab-", 20)[:32], "-")},
	}
	for _, tc := range cases {
		if got := slugKey(tc.in, 32); got != tc.want {
			t.Errorf("slugKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := slugKey(tc.in, 32); len(got) > 32 {
			t.Errorf("slugKey(%q) is %d characters; member.key is VARCHAR(32)", tc.in, len(got))
		}
	}
}

// TestTruncateCountsRunes: the VARCHAR widths in this schema count characters,
// and slicing bytes would both overshoot the limit and emit invalid UTF-8.
func TestTruncateCountsRunes(t *testing.T) {
	in := strings.Repeat("é", 200)
	got := truncate(in, 120)
	if n := len([]rune(got)); n != 120 {
		t.Errorf("truncate kept %d runes, want 120", n)
	}
	if got := truncate("  hi  ", 120); got != "hi" {
		t.Errorf("truncate should trim: %q", got)
	}
}

// TestMintJoinCode: a join code is read aloud and typed by hand, so its
// alphabet matters as much as its entropy.
func TestMintJoinCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 512; i++ {
		code, err := MintJoinCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != joinCodeLength {
			t.Fatalf("join code %q is %d characters, want %d", code, len(code), joinCodeLength)
		}
		if len(code) > 16 {
			t.Fatalf("join code %q does not fit team.join_code VARCHAR(16)", code)
		}
		for _, r := range code {
			if !strings.ContainsRune(joinCodeAlphabet, r) {
				t.Fatalf("join code %q contains %q, which is not in the alphabet", code, r)
			}
		}
		// The ambiguous characters must be absent, or "0" and "O" become two
		// spellings of one code and a teammate types the wrong one.
		if strings.ContainsAny(code, "ILOU") {
			t.Fatalf("join code %q contains an ambiguous character", code)
		}
		if seen[code] {
			t.Fatalf("MintJoinCode repeated %q after %d draws", code, i)
		}
		seen[code] = true
	}
	// Sanity on the alphabet itself: 32 symbols, so a byte modulo 32 is
	// uniform, and no duplicates.
	if len(joinCodeAlphabet) != 32 {
		t.Fatalf("the alphabet has %d symbols; the modulo sampling is only uniform at 32", len(joinCodeAlphabet))
	}
	distinct := map[rune]bool{}
	for _, r := range joinCodeAlphabet {
		if distinct[r] {
			t.Fatalf("the alphabet repeats %q, which skews the distribution", r)
		}
		distinct[r] = true
	}
	// And a join code must never look like a bearer token.
	code, _, _ := func() (string, string, error) { c, e := MintJoinCode(); return c, "", e }()
	if strings.HasPrefix(code, tokenPrefix) {
		t.Error("a join code must not be confusable with a bearer token")
	}
}

// TestRateLimiter covers the only thing standing between an unauthenticated
// create_team and a script.
func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(3, time.Hour)

	for i := 0; i < 3; i++ {
		if ok, _ := rl.Allow("1.2.3.4"); !ok {
			t.Fatalf("call %d was rejected inside the budget", i+1)
		}
	}
	ok, retryIn := rl.Allow("1.2.3.4")
	if ok {
		t.Error("the 4th call was allowed past a budget of 3")
	}
	if retryIn <= 0 {
		t.Error("a rejection should say when to try again")
	}

	// Budgets are per key.
	if ok, _ := rl.Allow("5.6.7.8"); !ok {
		t.Error("another address should have its own budget")
	}

	// An empty key means the call did not arrive over HTTP: nothing to limit.
	for i := 0; i < 100; i++ {
		if ok, _ := rl.Allow(""); !ok {
			t.Fatal("an in-process call should never be rate limited")
		}
	}

	// A window that has passed resets the budget.
	short := NewRateLimiter(1, time.Nanosecond)
	if ok, _ := short.Allow("1.2.3.4"); !ok {
		t.Fatal("first call rejected")
	}
	time.Sleep(time.Millisecond)
	if ok, _ := short.Allow("1.2.3.4"); !ok {
		t.Error("the window did not reset")
	}

	// A zero limit disables the limiter rather than blocking everything: a
	// misconfigured budget must not take the server down.
	off := NewRateLimiter(0, time.Hour)
	for i := 0; i < 10; i++ {
		if ok, _ := off.Allow("1.2.3.4"); !ok {
			t.Fatal("a zero limit should disable the limiter, not block every call")
		}
	}
	var nilLimiter *RateLimiter
	if ok, _ := nilLimiter.Allow("1.2.3.4"); !ok {
		t.Error("a nil limiter should allow")
	}
}

func TestEnvInt(t *testing.T) {
	t.Setenv("METICHE_TEST_INT", "")
	if got := envInt("METICHE_TEST_INT", 7); got != 7 {
		t.Errorf("unset = %d, want the fallback 7", got)
	}
	t.Setenv("METICHE_TEST_INT", "12")
	if got := envInt("METICHE_TEST_INT", 7); got != 12 {
		t.Errorf("= %d, want 12", got)
	}
	t.Setenv("METICHE_TEST_INT", "nonsense")
	if got := envInt("METICHE_TEST_INT", 7); got != 7 {
		t.Errorf("unparseable = %d, want the fallback 7", got)
	}
	t.Setenv("METICHE_TEST_INT", "-1")
	if got := envInt("METICHE_TEST_INT", 7); got != 7 {
		t.Errorf("negative = %d, want the fallback 7", got)
	}
}

// TestClientIP: X-Forwarded-For is caller-supplied, so honouring it without
// a trusted proxy in front turns a per-IP rate limit into no rate limit.
func TestClientIP(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/mcp", nil)
	req.RemoteAddr = "10.0.0.5:41234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")

	t.Setenv("METICHE_TRUST_PROXY", "")
	if got := clientIP(req); got != "10.0.0.5" {
		t.Errorf("untrusted = %q, want the socket address 10.0.0.5", got)
	}

	t.Setenv("METICHE_TRUST_PROXY", "1")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Errorf("trusted = %q, want the forwarded 203.0.113.9", got)
	}

	bare := httptest.NewRequest("POST", "/v1/mcp", nil)
	bare.RemoteAddr = "10.0.0.5"
	t.Setenv("METICHE_TRUST_PROXY", "")
	if got := clientIP(bare); got != "10.0.0.5" {
		t.Errorf("port-less RemoteAddr = %q, want 10.0.0.5", got)
	}
}
