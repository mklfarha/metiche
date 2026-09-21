package coordination

import (
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// PLAN.md: "No third-party credential may ever be required to run the server.
// That one constraint rules out a server-side LLM judge." And a CI check
// forbids outbound HTTP clients in app/coordination ("let's just call an LLM to
// rank the decisions").
//
// This is that check, for coordination and for the app/mcp files that pair
// decisions with plans and take verdicts: none of them may import net/http or
// a model SDK. The judge is always the plan's own agent.

// modelSDKImports are import path prefixes of model provider SDKs and
// frameworks. Adding a provider here is cheap; missing one is the bug.
var modelSDKImports = []string{
	"github.com/anthropics/",
	"github.com/liushuangls/go-anthropic",
	"github.com/openai/",
	"github.com/sashabaranov/go-openai",
	"google.golang.org/genai",
	"github.com/google/generative-ai-go",
	"cloud.google.com/go/vertexai",
	"cloud.google.com/go/aiplatform",
	"github.com/aws/aws-sdk-go-v2/service/bedrock",
	"github.com/tmc/langchaingo",
	"github.com/cohere-ai/",
	"github.com/mistralai/",
	"github.com/ollama/",
	"github.com/firebase/genkit",
	"github.com/cloudwego/eino",
}

// forbiddenNetImports are the stdlib packages that make outbound calls.
var forbiddenNetImports = []string{"net/http", "net/rpc", "net/smtp"}

func TestImportGuardNoHTTPClientOrModelSDK(t *testing.T) {
	coordination, err := filepath.Glob("*.go")
	if err != nil || len(coordination) == 0 {
		t.Fatalf("listing app/coordination: %v (%d files)", err, len(coordination))
	}
	decisionFiles := []string{
		"../mcp/recorddecision.go",
		"../mcp/decisionreview.go",
		"../mcp/reviewcontext.go",
		"../mcp/reportjudgement.go",
		"../mcp/decisionresolve.go",
	}
	// The duplicate-work scorer is pure like the rest (docs/DUPLICATES.md §9.1);
	// the glob must be looking at it.
	sawDuplicates := false
	for _, f := range coordination {
		sawDuplicates = sawDuplicates || f == "duplicates.go"
	}
	if !sawDuplicates {
		t.Error("app/coordination/duplicates.go is not under the guard")
	}
	// The app/mcp files that pair duplicates and render the review block
	// (§7.1). A later wave writes them; each is checked from the moment it
	// exists.
	for _, file := range []string{
		"../mcp/duplicatereview.go",
		"../mcp/duplicateresolve.go",
		"../mcp/reviewrender.go",
	} {
		if _, err := os.Stat(file); err == nil {
			decisionFiles = append(decisionFiles, file)
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stat %s: %v", file, err)
		}
	}
	checked := 0
	for _, file := range append(coordination, decisionFiles...) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parsing %s: %v", file, err)
			continue
		}
		checked++
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbiddenNetImports {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports %s: the server never calls out, least of all to a model", file, path)
				}
			}
			for _, sdk := range modelSDKImports {
				if strings.HasPrefix(path, sdk) {
					t.Errorf("%s imports model SDK %s: the plan's own agent judges, never the server", file, path)
				}
			}
		}
	}
	if checked < len(decisionFiles)+3 {
		t.Errorf("checked %d files; the guard is not looking at what it should", checked)
	}
}

// The guard must actually fire: a synthetic file importing net/http and a
// model SDK is caught by the same matching.
func TestImportGuardCatchesAViolation(t *testing.T) {
	src := "package x\nimport (\n\t\"net/http\"\n\t\"github.com/anthropics/anthropic-sdk-go\"\n)\n"
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var net, sdk bool
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		for _, bad := range forbiddenNetImports {
			net = net || path == bad
		}
		for _, p := range modelSDKImports {
			sdk = sdk || strings.HasPrefix(path, p)
		}
	}
	if !net || !sdk {
		t.Fatalf("the guard's matching missed net/http=%v sdk=%v", net, sdk)
	}
}
