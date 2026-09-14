package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mklfarha/metiche/cli/internal/buildinfo"
)

// releasesAPI is the only address the CLI contacts besides the MCP endpoint
// (docs/CLI.md §7). A variable only so a unit test can point it at a stub.
var releasesAPI = "https://api.github.com/repos/mklfarha/metiche/releases/latest"

func (a *app) cmdVersion(args []string) error {
	fs := a.flags("version")
	short := fs.Bool("short", false, "print only the version")
	check := fs.Bool("check", false, "say whether a newer release exists (contacts api.github.com)")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	d := doc("version")
	d["version"], d["commit"], d["date"], d["platform"] = buildinfo.Version, buildinfo.Commit, buildinfo.Date, buildinfo.Platform()
	latest := ""
	if *check {
		v, err := latestRelease(a.timeout)
		if err != nil {
			return fail(exitNoServer, "unreachable", "could not ask GitHub for the latest release: %v", err)
		}
		latest = v
		d["latest"] = v
		d["update_available"] = v != "" && strings.TrimPrefix(v, "v") != strings.TrimPrefix(buildinfo.Version, "v")
	}
	if a.json {
		a.writeJSON(d)
		return nil
	}
	switch {
	case *short:
		a.out("%s", buildinfo.Version)
	default:
		a.out("metiche %s (commit %s, built %s, %s)", buildinfo.Version, buildinfo.Commit, buildinfo.Date, buildinfo.Platform())
	}
	if *check {
		if d["update_available"] == true {
			a.out("metiche %s · %s is available. Update by re-running the installer:\n  curl -fsSL https://metiche.xyz/install.sh | sh", buildinfo.Version, strings.TrimPrefix(latest, "v"))
		} else {
			a.out("metiche %s is the latest release.", buildinfo.Version)
		}
	}
	return nil
}

func latestRelease(timeout time.Duration) (string, error) {
	hc := &http.Client{Timeout: timeout}
	req, _ := http.NewRequest(http.MethodGet, releasesAPI, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil // no release published yet
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.TagName, nil
}
