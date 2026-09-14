package cmd

import (
	"strings"
	"testing"

	"github.com/mklfarha/metiche/cli/internal/wire"
)

func TestSubcommand(t *testing.T) {
	sub, rest := subcommand([]string{"--url", "https://x/v1/mcp", "--json", "create", "Hack Night", "--quiet"}, "create", "show")
	if sub != "create" || strings.Join(rest, "|") != "--url|https://x/v1/mcp|--json|Hack Night|--quiet" {
		t.Errorf("sub=%q rest=%v", sub, rest)
	}
	if sub, _ := subcommand([]string{"--json"}, "create"); sub != "" {
		t.Error("no subcommand expected")
	}
}

func TestDuplicateNames(t *testing.T) {
	d := duplicateNames([]wire.TeamChoice{{Slug: "hack-night", Name: "Hack Night"}, {Slug: "hack-night-3f9a1c", Name: "hack night"}, {Slug: "solo", Name: "Solo"}})
	if strings.Join(d["hack-night"], ",") != "hack-night-3f9a1c" || strings.Join(d["hack-night-3f9a1c"], ",") != "hack-night" || len(d["solo"]) != 0 {
		t.Errorf("duplicates = %v", d)
	}
}
