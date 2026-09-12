package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	frontend "github.com/mklfarha/metiche/frontend"
)

// install.sh exists twice: the canonical script at the repository root, which
// is what people read on GitHub, and an embedded copy under static/, because
// go:embed cannot reach outside the module and the Docker build context is
// code/frontend alone.
//
// Two copies of anything drift. This one would drift SILENTLY and in the worst
// possible place: the landing page's single headline instruction is
// `curl -fsSL https://metiche.xyz/install.sh | sh`, so a stale copy means
// every new teammate runs an old installer while the page next to it describes
// the new one. The copy is regenerated with:
//
//	cp install.sh code/frontend/static/install.sh
func TestEmbeddedInstallerMatchesTheCanonicalOne(t *testing.T) {
	const canonical = "../../../../install.sh"

	want, err := os.ReadFile(canonical)
	if err != nil {
		// Inside the Docker build the repository root is not present, and
		// that is fine: the point of this test is to fail in a checkout,
		// where the mistake is actually made and can be fixed.
		t.Skipf("skipped: %s is not readable here (%v), so there is nothing to compare against", canonical, err)
	}

	embedded, err := os.ReadFile("../../static/install.sh")
	if err != nil {
		t.Fatalf("the embedded copy is missing: %v\nrun: cp install.sh code/frontend/static/install.sh", err)
	}

	if sum(embedded) != sum(want) {
		t.Fatalf("static/install.sh has drifted from the canonical install.sh\n"+
			"  canonical %s (%d bytes)\n  embedded  %s (%d bytes)\n"+
			"run: cp install.sh code/frontend/static/install.sh",
			sum(want), len(want), sum(embedded), len(embedded))
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}

// The route the landing page tells everyone to curl. A 404 here is the first
// thing a new teammate would ever see of metiche.
func TestInstallShIsServed(t *testing.T) {
	s := NewServer(frontend.Static(), nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/install.sh")
	if err != nil {
		t.Fatalf("GET /install.sh: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /install.sh returned %d, want 200 — this is the landing page's one command", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain so a browser shows it rather than downloading it", ct)
	}

	buf := make([]byte, 256)
	n, _ := res.Body.Read(buf)
	head := string(buf[:n])
	if !strings.HasPrefix(head, "#!/bin/sh") {
		t.Fatalf("what came back is not the installer, it starts: %q", head)
	}
}
