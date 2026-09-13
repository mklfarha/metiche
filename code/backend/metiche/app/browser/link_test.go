package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidRedirectPath(t *testing.T) {
	for _, ok := range []string{"/", "/teams", "/t/taqueria-tracker", "/t/a", "/t/0b6b2f2e-8f6e-4d7a-9a51-2b6f0d1c9e11"} {
		if !ValidRedirectPath(ok) {
			t.Errorf("ValidRedirectPath(%q) = false", ok)
		}
	}
	for _, bad := range []string{
		"", "//evil.example", "https://evil.example", "/t/", "/t/Upper", "/t/a/b", "/t/-a", "/t/a-",
		"/teams/", "/t/a?x=1", "/t/a#x", "/account", "/t/" + strings.Repeat("a", 65), "t/a", "/t/a\n",
	} {
		if ValidRedirectPath(bad) {
			t.Errorf("ValidRedirectPath(%q) = true", bad)
		}
	}
	if p, err := TeamRedirectPath("my-team"); err != nil || p != "/t/my-team" {
		t.Errorf("TeamRedirectPath = %q, %v", p, err)
	}
	if _, err := TeamRedirectPath("../x"); !errors.Is(err, ErrInvalidRedirect) {
		t.Errorf("TeamRedirectPath(../x) err = %v", err)
	}
}

func TestParseBoardBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://metiche.xyz":         "https://metiche.xyz",
		"https://metiche.xyz/":        "https://metiche.xyz",
		" https://board.example:8443": "https://board.example:8443",
		"http://localhost:8787":       "http://localhost:8787",
		"http://127.0.0.1:8787/":      "http://127.0.0.1:8787",
		"http://localhost":            "http://localhost",
	} {
		got, err := ParseBoardBaseURL(in)
		if err != nil || got != want {
			t.Errorf("ParseBoardBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "metiche.xyz", "http://metiche.xyz", "http://localhost.evil.example", "http://127.0.0.1.evil.example",
		"https://user:pw@metiche.xyz", "https://metiche.xyz/board", "https://metiche.xyz?x=1", "https://metiche.xyz#f",
		"javascript:alert(1)", "ftp://metiche.xyz", "https://",
	} {
		if got, err := ParseBoardBaseURL(bad); !errors.Is(err, ErrInvalidBoardBaseURL) {
			t.Errorf("ParseBoardBaseURL(%q) = %q, %v; want ErrInvalidBoardBaseURL", bad, got, err)
		}
	}
}

func TestBoardBaseURLFromEnv(t *testing.T) {
	t.Setenv(BoardBaseURLEnv, "")
	if got, err := BoardBaseURLFromEnv(); err != nil || got != DefaultBoardBaseURL {
		t.Errorf("default = %q, %v", got, err)
	}
	t.Setenv(BoardBaseURLEnv, "http://localhost:8787/")
	if got, err := BoardBaseURLFromEnv(); err != nil || got != "http://localhost:8787" {
		t.Errorf("local = %q, %v", got, err)
	}
	t.Setenv(BoardBaseURLEnv, "http://metiche.xyz")
	if _, err := BoardBaseURLFromEnv(); err == nil {
		t.Error("plain http on a public host was accepted")
	}
}

func TestIPPrefixAndUserAgent(t *testing.T) {
	for in, want := range map[string]any{
		"203.0.113.77":           "203.0.113.0/24",
		"203.0.113.0/24":         "203.0.113.0/24",
		"203.0.113.77/32":        "203.0.113.0/24",
		"::ffff:198.51.100.9":    "198.51.100.0/24",
		"2001:db8:abcd:12:1::5":  "2001:db8:abcd::/48",
		"2001:db8:abcd:12::/64":  "2001:db8:abcd::/48",
		"fe80::1%eth0":           "fe80::/48",
		"":                       nil,
		"not an ip":              nil,
		"203.0.113.77, 10.0.0.1": nil,
		strings.Repeat("1", 200): nil,
	} {
		if got := ipPrefix(in); got != want {
			t.Errorf("ipPrefix(%q) = %v, want %v", in, got, want)
		}
	}
	if got := sanitizeUserAgent("Mozilla/5.0\x00\n\t(Mac)‮"); got != "Mozilla/5.0(Mac)" {
		t.Errorf("sanitizeUserAgent = %q", got)
	}
	if got := sanitizeUserAgent(strings.Repeat("é", 500)).(string); len([]rune(got)) != 200 {
		t.Errorf("sanitizeUserAgent kept %d runes", len([]rune(got)))
	}
	if sanitizeUserAgent(" \x01 ") != nil {
		t.Error("blank user agent should be NULL")
	}
}

// With no database the functions must still refuse or report unavailable,
// never panic, and a malformed secret never reaches the database at all.
func TestNoDatabase(t *testing.T) {
	ctx := context.Background()
	if _, err := ValidateSession(ctx, nil, "mbs_obviously-fake"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ValidateSession(nil db) = %v", err)
	}
	if _, err := ValidateSession(ctx, nil, "mtk_not-a-session"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("ValidateSession(wrong prefix) = %v", err)
	}
	if _, err := ValidateSession(ctx, nil, "  "); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("ValidateSession(blank) = %v", err)
	}
	if _, err := Exchange(ctx, nil, ExchangeRequest{LinkSecret: "mbl_obviously-fake"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Exchange(nil db) = %v", err)
	}
	if _, err := Exchange(ctx, nil, ExchangeRequest{LinkSecret: "mbs_wrong-kind"}); !errors.Is(err, ErrLinkNotUsable) {
		t.Errorf("Exchange(session secret as link) = %v", err)
	}
}

func TestWindowLimiter(t *testing.T) {
	l := newWindowLimiter(2, 50_000_000_000)
	a, _ := l.allow("k")
	b, _ := l.allow("k")
	c, retry := l.allow("k")
	if !a || !b || c || retry <= 0 {
		t.Errorf("limiter = %v %v %v retry %v", a, b, c, retry)
	}
	if ok, _ := newWindowLimiter(0, 1).allow("k"); !ok {
		t.Error("limit 0 must disable")
	}
}
