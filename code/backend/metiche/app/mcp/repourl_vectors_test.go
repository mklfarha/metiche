package mcp

import (
	"encoding/json"
	"os"
	"testing"
)

// TestRepoURLVectorsSharedWithTheCLI: the CLI copies normalizeRepoURL,
// canonicalRepoURL and projectKeyFromRepoURL so `metiche init` resolves the
// project start_session will resolve (docs/CLI.md §4.12 item 2). Both sides
// test the same file, code/cli/testdata/repourl_vectors.json, so a change here
// that the CLI does not follow fails one of the two.
func TestRepoURLVectorsSharedWithTheCLI(t *testing.T) {
	raw, err := os.ReadFile("../../../../cli/testdata/repourl_vectors.json")
	if err != nil {
		t.Skipf("the CLI's vector file is not readable here (%v)", err)
	}
	var doc struct {
		Vectors []struct{ In, Normalized, Canonical, Key string }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Vectors) < 4 {
		t.Fatalf("only %d vectors", len(doc.Vectors))
	}
	for _, v := range doc.Vectors {
		if got := normalizeRepoURL(v.In); got != v.Normalized {
			t.Errorf("normalizeRepoURL(%q) = %q, vector says %q", v.In, got, v.Normalized)
		}
		if got := canonicalRepoURL(v.In); got != v.Canonical {
			t.Errorf("canonicalRepoURL(%q) = %q, vector says %q", v.In, got, v.Canonical)
		}
		if got := projectKeyFromRepoURL(v.In); got != v.Key {
			t.Errorf("projectKeyFromRepoURL(%q) = %q, vector says %q", v.In, got, v.Key)
		}
	}
}
