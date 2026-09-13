package browser_test

import (
	"strings"
	"testing"

	"github.com/mklfarha/metiche/backend/app/browser"
	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
)

// app/browser defines its own hashing so it never imports app/mcp. This test
// (an external package, so the import is test-only) pins the two definitions
// together: "one definition of the stored form of a secret" (§4.1, §7).
func TestHashParityWithMCP(t *testing.T) {
	link, linkHash, err := browser.MintLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	sess, sessHash, err := browser.MintSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := metichemcp.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	inputs := []string{
		"", " ", "mbl_obviously-fake", "  mbs_obviously-fake\n", "mtk_x", "ñandú", strings.Repeat("a", 4096),
		link, sess, tok, " " + link + "\t",
	}
	for _, in := range inputs {
		if got, want := browser.Hash(in), metichemcp.HashToken(in); got != want {
			t.Errorf("Hash(%q) = %s, mcp.HashToken = %s", in, got, want)
		}
	}
	if linkHash != browser.Hash(link) || sessHash != browser.Hash(sess) {
		t.Error("minted hash is not Hash(secret)")
	}

	for _, c := range []struct{ secret, stored string }{
		{link, linkHash},
		{link, sessHash},
		{" " + link, linkHash},
		{"", browser.Hash("")},
		{link, linkHash[:63]},
		{link, strings.ToUpper(linkHash)},
		{sess, sessHash},
		{"mbs_obviously-fake", ""},
	} {
		if got, want := browser.Verify(c.secret, c.stored), metichemcp.VerifyToken(c.secret, c.stored); got != want {
			t.Errorf("Verify(%q, %q) = %v, mcp.VerifyToken = %v", c.secret, c.stored, got, want)
		}
	}
	if !browser.Verify(link, linkHash) || browser.Verify(link, sessHash) {
		t.Error("Verify does not accept its own hash or accepts another's")
	}
}

func TestSecretsShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		link, _, err := browser.MintLinkSecret()
		if err != nil {
			t.Fatal(err)
		}
		sess, _, err := browser.MintSessionSecret()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(link, "mbl_") || len(link) != 4+43 {
			t.Fatalf("link secret shape: prefix/len %d", len(link))
		}
		if !strings.HasPrefix(sess, "mbs_") || len(sess) != 4+43 {
			t.Fatalf("session secret shape: prefix/len %d", len(sess))
		}
		if seen[link] || seen[sess] {
			t.Fatal("secret repeated")
		}
		seen[link], seen[sess] = true, true
	}
	if len(browser.Hash("x")) != 64 {
		t.Fatal("hash is not 64 hex characters")
	}
}
