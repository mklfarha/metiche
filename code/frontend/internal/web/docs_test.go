package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	frontend "github.com/mklfarha/metiche/frontend"
	"github.com/mklfarha/metiche/frontend/internal/view"
)

func docsTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewServer(frontend.Static(), nil).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func fetch(t *testing.T, ts *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("GET %s: reading body: %v", path, err)
	}
	return res, string(b)
}

// docsRoutes is the index plus every registered page, with its title.
func docsRoutes() map[string]string {
	out := map[string]string{"/docs": "metiche docs"}
	for _, p := range view.DocPages() {
		out[p.URL()] = p.Title
	}
	return out
}

var (
	docTagRE   = regexp.MustCompile(`<[^>]*>`)
	docTitleRE = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	docH1RE    = regexp.MustCompile(`(?s)<h1>(.*?)</h1>`)
)

func docText(body string) string { return html.UnescapeString(docTagRE.ReplaceAllString(body, " ")) }

// TestDocsPagesServe: the index and every page answer 200 with their title, as
// static, cacheable pages under a script-free CSP.
func TestDocsPagesServe(t *testing.T) {
	ts := docsTestServer(t)
	routes := docsRoutes()
	if len(routes) < 9 {
		t.Fatalf("expected the index and 8 pages, have %d routes", len(routes))
	}
	for path, title := range routes {
		res, body := fetch(t, ts, path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q", path, ct)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "public, max-age=300" {
			t.Errorf("GET %s Cache-Control = %q, want public, max-age=300", path, cc)
		}
		if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") {
			t.Errorf("GET %s CSP = %q, want script-src 'none'", path, csp)
		}
		if res.Header.Get("Set-Cookie") != "" {
			t.Errorf("GET %s sets a cookie", path)
		}
		gotTitle := html.UnescapeString(docTitleRE.FindStringSubmatch(body)[1])
		if !strings.HasPrefix(gotTitle, title) {
			t.Errorf("GET %s <title> = %q, want it to start with %q", path, gotTitle, title)
		}
		if m := docH1RE.FindStringSubmatch(body); m == nil || html.UnescapeString(m[1]) != title {
			t.Errorf("GET %s <h1> = %v, want %q", path, m, title)
		}
		// Every page links to every other page.
		for other := range routes {
			if other != "/docs" && !strings.Contains(body, `href="`+other+`"`) {
				t.Errorf("GET %s has no link to %s", path, other)
			}
		}
	}

	res, _ := fetch(t, ts, "/docs/")
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/docs" {
		t.Errorf("GET /docs/ = %d to %q, want 301 to /docs", res.StatusCode, res.Header.Get("Location"))
	}
}

// TestUnknownDocIsTheSitesNotFound: an unknown page is the same 404 as any path
// this site does not serve, and is not cached as a docs page.
func TestUnknownDocIsTheSitesNotFound(t *testing.T) {
	ts := docsTestServer(t)
	want, wantBody := fetch(t, ts, "/no-such-page-anywhere")
	if want.StatusCode != http.StatusNotFound {
		t.Fatalf("control: GET /no-such-page-anywhere = %d", want.StatusCode)
	}
	for _, path := range []string{"/docs/no-such-page", "/docs/Getting-Started", "/docs/getting-started/extra", "/docs/..%2fsignin"} {
		res, body := fetch(t, ts, path)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
		if body != wantBody {
			t.Errorf("GET %s body = %q, want the site's plain 404 %q", path, body, wantBody)
		}
		if cc := res.Header.Get("Cache-Control"); strings.Contains(cc, "public") {
			t.Errorf("GET %s 404 is cacheable: %q", path, cc)
		}
	}
}

// TestDocsHaveNoScriptsOrForms: no <script> of any kind, no on* handler, no
// inline style (the CSP would block both), and no form.
func TestDocsHaveNoScriptsOrForms(t *testing.T) {
	ts := docsTestServer(t)
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	for path := range docsRoutes() {
		_, body := fetch(t, ts, path)
		low := strings.ToLower(body)
		for _, bad := range []string{"<script", "<form", "<iframe", "javascript:"} {
			if strings.Contains(low, bad) {
				t.Errorf("GET %s contains %q", path, bad)
			}
		}
		for _, tag := range docTagRE.FindAllString(body, -1) {
			if handler.MatchString(tag) {
				t.Errorf("GET %s has an inline event handler: %s", path, tag)
			}
			if strings.Contains(strings.ToLower(tag), " style=") {
				t.Errorf("GET %s has an inline style: %s", path, tag)
			}
		}
	}
}

// TestDocsShowNoSecretsOrUnbuiltThings: nothing token-shaped, code-shaped or
// link-secret-shaped is on any page; the CLI and the unbuilt tools are not
// presented as available.
func TestDocsShowNoSecretsOrUnbuiltThings(t *testing.T) {
	ts := docsTestServer(t)
	secrets := []*regexp.Regexp{
		regexp.MustCompile(`mtk_[A-Za-z0-9_-]{4,}`),                           // agent token
		regexp.MustCompile(`mb[ls]_[A-Za-z0-9_-]{4,}`),                        // sign-in link or browser session secret
		regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`),             // a bearer value
		regexp.MustCompile(`://[^\s/@:]+:[^\s/@]+@`),                          // credentials in a URL
		regexp.MustCompile(`(?i)\b(METICHE_TOKEN|METICHE_JOIN_CODE)=[^<\s]+`), // a value set for a secret
	}
	// A join code is 10 characters of Crockford-style base32; a real one has
	// letters and digits mixed.
	codeShape := regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{10}\b`)
	hasDigit, hasLetter := regexp.MustCompile(`[0-9]`), regexp.MustCompile(`[A-Z]`)

	// record_decision, get_review_context and report_judgement shipped; they are
	// listed under Decisions on the tool reference. resolve_conflict has not.
	unbuilt := []string{"resolve_conflict",
		"metiche open", "metiche init", "metiche login", "metiche status", "metiche whoami"}

	for path := range docsRoutes() {
		_, body := fetch(t, ts, path)
		text := docText(body)
		for _, re := range secrets {
			for _, m := range re.FindAllString(text, -1) {
				if strings.HasSuffix(m, "=<join") || strings.HasSuffix(m, "=<token") {
					continue // a placeholder: METICHE_JOIN_CODE=<join code>
				}
				t.Errorf("GET %s shows a secret-shaped string %q", path, m)
			}
		}
		for _, m := range codeShape.FindAllString(text, -1) {
			if hasDigit.MatchString(m) && hasLetter.MatchString(m) {
				t.Errorf("GET %s shows a join-code-shaped string %q", path, m)
			}
		}
		if strings.Contains(body, "#mbl_") {
			t.Errorf("GET %s carries a sign-in link", path)
		}
		for _, u := range unbuilt {
			if strings.Contains(text, u) {
				t.Errorf("GET %s names %q, which is not built", path, u)
			}
		}
	}

	_, start := fetch(t, ts, "/docs/getting-started")
	if !strings.Contains(docText(start), "Command line (coming soon)") {
		t.Errorf("Getting started has no \"Command line (coming soon)\" note")
	}
	_, tools := fetch(t, ts, "/docs/tool-reference")
	next := strings.Index(tools, `id="coming-next"`)
	if next < 0 || !strings.Contains(docText(tools[next:]), "Coming next") {
		t.Fatalf("the tool reference has no section labelled Coming next")
	}
	for _, want := range []string{"Dismissals"} {
		if !strings.Contains(docText(tools[next:]), want) {
			t.Errorf("Coming next does not list %s", want)
		}
	}
	for _, built := range []string{"Contracts", "Decisions", "Duplicate work", "duplicate work"} {
		if strings.Contains(docText(tools[next:]), built) {
			t.Errorf("Coming next still lists %s, which is built", built)
		}
	}
	if strings.Contains(tools[next:], `id="tool-`) {
		t.Errorf("a tool is listed as registered inside Coming next")
	}
}

// TestDocsDescribeDuplicateWork: duplicate work is built, so the docs describe
// it in the present tense, for a hackathon (no issue numbers, caught from the
// wording alone, the issue id only the strongest signal), with the judging done
// by the agent's own model and a person asked only when the agents do not
// settle it. Both review tools name both kinds of pair.
func TestDocsDescribeDuplicateWork(t *testing.T) {
	ts := docsTestServer(t)

	_, agents := fetch(t, ts, "/docs/working-with-agents")
	start := strings.Index(agents, `id="duplicate-work"`)
	if start < 0 {
		t.Fatalf("Working with agents has no #duplicate-work section")
	}
	end := strings.Index(agents[start:], `id="subagents"`)
	if end < 0 {
		t.Fatalf("the #duplicate-work section is not followed by #subagents")
	}
	sec := strings.Join(strings.Fields(docText(agents[start:start+end])), " ")
	for _, want := range []string{
		"nobody has issue numbers", "add login page", "build the login screen", "wording alone",
		"strongest signal", "own model", "no model", "no provider key",
		"declared second", "superseded", "only if the second did not back off",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("#duplicate-work does not say %q: %s", want, sec)
		}
	}
	for _, bad := range []string{" will ", "coming next", "Coming next", "not built"} {
		if strings.Contains(sec, bad) {
			t.Errorf("#duplicate-work is not written as built (%q): %s", bad, sec)
		}
	}
	if !strings.Contains(docText(agents), "building the same thing and have not settled who keeps it") {
		t.Errorf("When an agent asks you does not name an unsettled duplicate")
	}

	_, board := fetch(t, ts, "/docs/the-board")
	for _, want := range []string{`id="duplicate-conflicts"`, "duplicate work", "asked to stop", "why paired", "What the judge said", "a person was asked"} {
		if !strings.Contains(board, want) {
			t.Errorf("The board does not describe the duplicate card: missing %q", want)
		}
	}

	_, tools := fetch(t, ts, "/docs/tool-reference")
	for _, tool := range []string{"get_review_context", "report_judgement"} {
		row := regexp.MustCompile(`(?s)id="tool-` + tool + `".*?</dd>`).FindString(tools)
		text := docText(row)
		if !strings.Contains(text, "decision") || !strings.Contains(text, "other") || !strings.Contains(text, "same") {
			t.Errorf("%s's row does not name both kinds of pair: %s", tool, strings.Join(strings.Fields(text), " "))
		}
	}
}

// TestToolReferenceListsExactlyTheRegisteredTools reads the tools the MCP
// server registers straight from the backend's source — newServer in
// app/mcp/server.go and every package function it calls — and compares them
// with the ids on /docs/tool-reference. Adding, removing or renaming a tool
// without updating the docs fails here.
func TestToolReferenceListsExactlyTheRegisteredTools(t *testing.T) {
	want := registeredMCPTools(t)

	ts := docsTestServer(t)
	_, body := fetch(t, ts, "/docs/tool-reference")
	var got []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`id="tool-([a-z_]+)"`).FindAllStringSubmatch(body, -1) {
		if seen[m[1]] {
			t.Errorf("tool %s is listed twice", m[1])
		}
		seen[m[1]] = true
		got = append(got, m[1])
	}
	sort.Strings(got)

	missing, extra := diffSorted(want, got)
	if len(missing) > 0 {
		t.Errorf("registered in app/mcp but missing from /docs/tool-reference: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("on /docs/tool-reference but not registered in app/mcp: %v", extra)
	}
}

const mcpSourceDir = "../../../backend/metiche/app/mcp"

// registeredMCPTools parses app/mcp and follows newServer through every
// package-level function it calls, collecting the Name of each addTool call.
func registeredMCPTools(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(mcpSourceDir)
	if err != nil {
		// The Docker build context is code/frontend alone; in a checkout the
		// backend is always there.
		t.Skipf("skipped: %s is not readable here (%v)", mcpSourceDir, err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(mcpSourceDir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	if funcs["newServer"] == nil {
		t.Fatalf("app/mcp has no newServer; update this test to where tools are registered")
	}

	tools := map[string]bool{}
	visited := map[string]bool{}
	var walk func(string)
	walk = func(fn string) {
		if visited[fn] || funcs[fn] == nil || funcs[fn].Body == nil {
			return
		}
		visited[fn] = true
		ast.Inspect(funcs[fn].Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fun := call.Fun
			switch x := fun.(type) {
			case *ast.IndexExpr:
				fun = x.X
			case *ast.IndexListExpr:
				fun = x.X
			}
			switch x := fun.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == "AddTool" {
					t.Errorf("%s registers a tool with AddTool directly, which this test cannot name", fset.Position(call.Pos()))
				}
			case *ast.Ident:
				if x.Name == "addTool" {
					name := toolNameLiteral(call)
					if name == "" {
						t.Errorf("%s: addTool without a literal Name", fset.Position(call.Pos()))
					}
					tools[name] = true
					return true
				}
				walk(x.Name)
			}
			return true
		})
	}
	walk("newServer")

	if len(tools) < 10 || !tools["start_session"] || !tools["declare_intent"] {
		t.Fatalf("parsed an implausible tool list from app/mcp: %v", tools)
	}
	out := make([]string, 0, len(tools))
	for name := range tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// toolNameLiteral returns Name from addTool(s, h, logger, &mcp.Tool{Name: "…"}, …).
func toolNameLiteral(call *ast.CallExpr) string {
	if len(call.Args) < 4 {
		return ""
	}
	u, ok := call.Args[3].(*ast.UnaryExpr)
	if !ok {
		return ""
	}
	lit, ok := u.X.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Name" {
			if bl, ok := kv.Value.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				s, err := strconv.Unquote(bl.Value)
				if err == nil {
					return s
				}
			}
		}
	}
	return ""
}

// diffSorted returns what is in want but not got, and in got but not want.
func diffSorted(want, got []string) (missing, extra []string) {
	in := func(list []string, s string) bool {
		i := sort.SearchStrings(list, s)
		return i < len(list) && list[i] == s
	}
	for _, w := range want {
		if !in(got, w) {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !in(want, g) {
			extra = append(extra, g)
		}
	}
	return missing, extra
}
