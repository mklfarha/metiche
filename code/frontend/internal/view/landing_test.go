package view

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
)

// TestInstallLineIsExactlyTheCommand: the install block is copied two ways —
// by the copy button, which takes the <pre>'s text, and by hand. Both must
// yield the command and nothing else: no prompt glyph, no stray markup.
func TestInstallLineIsExactlyTheCommand(t *testing.T) {
	plain := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(installHTML, "")
	if plain != installCommand {
		t.Fatalf("installHTML as text = %q, want %q", plain, installCommand)
	}
	if strings.HasPrefix(strings.TrimSpace(plain), "#") || strings.HasPrefix(strings.TrimSpace(plain), "$") {
		t.Fatalf("install line carries a prompt: %q", plain)
	}

	var buf bytes.Buffer
	if err := Landing("/t/demo").Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if !strings.Contains(body, `<pre id="install-cmd">`+installHTML+`</pre>`) {
		t.Fatalf("landing does not render the install line in #install-cmd")
	}
	// The button is revealed by script; with JS off it must not be a dead control.
	if !regexp.MustCompile(`<button[^>]*data-copy="install-cmd"[^>]*\bhidden\b`).MatchString(body) {
		t.Fatalf("copy button is not shipped hidden")
	}
}
