package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The three things that ship the skill — the standalone copy, the plugin copy,
// and install.sh's list of checksums it recognizes — are three separate files
// that all have to move together when the skill changes. Nothing in the build
// noticed when they did not, so each one gets a test here.
//
// They live next to TestEmbeddedInstallerMatchesTheCanonicalOne because they
// share its shape: read a file at the repository root, compare, and skip when
// the root is not there (inside the Docker build it is not, and that is fine —
// the mistake is made in a checkout, where these fail).

// repoFile reads a path relative to the repository root. A missing repository
// root skips the test; a file missing from a real checkout fails it.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	const root = "../../../../"

	if _, err := os.Stat(root + "install.sh"); err != nil {
		t.Skipf("skipped: the repository root is not readable here (%v), so there is nothing to compare", err)
	}
	b, err := os.ReadFile(root + rel)
	if err != nil {
		t.Fatalf("%s is missing from the checkout: %v", rel, err)
	}
	return b
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

const (
	standaloneSkill = "skill/metiche-teamwork/SKILL.md"
	pluginSkill     = "plugin/skills/metiche-teamwork/SKILL.md"
)

// The skill exists twice: skill/ is the standalone copy someone can drop into
// ~/.claude/skills, plugin/skills/ is the one the marketplace ships. They must
// be byte-identical — install.sh's skill_is_known compares a copy on disk
// against BOTH of them to decide whether a standalone copy is metiche's own
// (and so removable) or the person's own edit (and so untouchable). If the two
// drift, that decision starts depending on which file it happened to match,
// and the plugin ships a different skill than the one the docs describe.
//
// Until now only install.sh's own cmp and a line in plugin/README.md said so,
// and neither runs in CI.
func TestTheTwoSkillCopiesAreIdentical(t *testing.T) {
	standalone := repoFile(t, standaloneSkill)
	plugin := repoFile(t, pluginSkill)

	if sha256Hex(standalone) != sha256Hex(plugin) {
		t.Fatalf("the two copies of the skill have drifted\n"+
			"  %s %s (%d bytes)\n  %s %s (%d bytes)\n"+
			"they must be byte-identical; run: cp %s %s",
			standaloneSkill, sha256Hex(standalone)[:16], len(standalone),
			pluginSkill, sha256Hex(plugin)[:16], len(plugin),
			standaloneSkill, pluginSkill)
	}
}

// install.sh keeps KNOWN_SKILL_SHA256, the sha256 of every SKILL.md metiche has
// ever shipped. converge_skill_copy removes a standalone copy of the skill only
// when it matches one of those (or a copy present on the machine), so that it
// never deletes a SKILL.md someone edited themselves.
//
// The failure it guards against is quiet and one-directional: ship a new skill,
// forget the checksum, and the installer stops recognizing the copy it just
// installed on the last release — it leaves a duplicate skill behind and prints
// a warning about edits the person never made. This test makes forgetting a
// build failure, which is why the list is worth pinning at all.
func TestInstallerKnowsTheCurrentSkill(t *testing.T) {
	skill := repoFile(t, standaloneSkill)
	want := sha256Hex(skill)

	known := knownSkillChecksums(t, repoFile(t, "install.sh"))
	for _, k := range known {
		if k == want {
			return
		}
	}
	t.Fatalf("install.sh does not recognize the skill it ships\n"+
		"  %s is %s\n  KNOWN_SKILL_SHA256 lists %d checksum(s): %s\n"+
		"append the new one (keep the old entries — people still have those versions installed),\n"+
		"in BOTH install.sh and code/frontend/static/install.sh",
		standaloneSkill, want, len(known), strings.Join(known, " "))
}

// KNOWN_SKILL_SHA256="<sum>\n<sum>..." — a shell string the installer splits on
// whitespace, so this reads it the same way.
var knownSkillLine = regexp.MustCompile(`(?m)^KNOWN_SKILL_SHA256="([^"]*)"`)

func knownSkillChecksums(t *testing.T, installer []byte) []string {
	t.Helper()

	m := knownSkillLine.FindSubmatch(installer)
	if m == nil {
		t.Fatalf("install.sh has no KNOWN_SKILL_SHA256=\"...\" assignment any more — " +
			"if it was renamed, rename it here too; if it was removed, so should this test be")
	}
	return strings.Fields(string(m[1]))
}

// The plugin declares its version twice: plugin.json is the manifest, and
// marketplace.json is the listing that points at it with "source": "./". They
// have disagreed before, and the symptom is not a build error — it is
// `claude plugin update` deciding the installed version is already current and
// shipping nothing, which is exactly how an old skill stays on a machine.
func TestPluginAndMarketplaceAgreeOnTheVersion(t *testing.T) {
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	const manifestPath = "plugin/.claude-plugin/plugin.json"
	if err := json.Unmarshal(repoFile(t, manifestPath), &manifest); err != nil {
		t.Fatalf("%s is not valid JSON: %v", manifestPath, err)
	}

	var market struct {
		Plugins []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	const marketPath = "plugin/.claude-plugin/marketplace.json"
	if err := json.Unmarshal(repoFile(t, marketPath), &market); err != nil {
		t.Fatalf("%s is not valid JSON: %v", marketPath, err)
	}

	if manifest.Version == "" {
		t.Fatalf("%s declares no version", manifestPath)
	}

	found := false
	for _, p := range market.Plugins {
		if p.Name != manifest.Name {
			continue
		}
		found = true
		if p.Version != manifest.Version {
			t.Fatalf("the plugin's two manifests disagree about its version\n"+
				"  %s says %q\n  %s says %q\n"+
				"bump both, or `claude plugin update` ships nothing",
				manifestPath, manifest.Version, marketPath, p.Version)
		}
	}
	if !found {
		t.Fatalf("%s lists no plugin named %q, so nothing pins its version", marketPath, manifest.Name)
	}
}
