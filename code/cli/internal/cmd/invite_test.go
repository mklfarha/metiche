package cmd

import "testing"

func TestExpiresHours(t *testing.T) {
	for in, want := range map[string]int{"48h": 48, "7d": 168, "90m": 2, "1h30m": 2} {
		if got, err := expiresHours(in); err != nil || got != want {
			t.Errorf("expiresHours(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "-1h", "soon"} {
		if _, err := expiresHours(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
