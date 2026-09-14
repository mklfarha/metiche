// Package mcpclient is the CLI's one door to metiche: MCP over streamable HTTP
// with the go-sdk the server uses (docs/CLI.md §5).
//
// It is also the only package that calls secret.Reveal: the RoundTripper puts
// the bearer on the wire, and nothing else in the binary ever sees the value.
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mklfarha/metiche/cli/internal/buildinfo"
	"github.com/mklfarha/metiche/cli/internal/secret"
)

// AllowedTools is the compile-time list of tools the CLI may call (§7). Every
// other tool — join_team, start_session, end_session, heartbeat and the work
// tools — is refused before anything is sent.
var AllowedTools = map[string]bool{
	"health":            true,
	"whoami":            true,
	"list_teams":        true,
	"get_team_state":    true,
	"list_invites":      true,
	"create_invite":     true,
	"revoke_invite":     true,
	"create_team":       true,
	"open_board":        true,
	"sign_out_browsers": true,
}

// Kind classifies a failure for the exit-code mapping.
type Kind int

const (
	// KindUnauthorized is an HTTP 401. Whether the token is dead or the
	// database is down is decided by an anonymous health call (IsDeadToken).
	KindUnauthorized Kind = iota + 1
	// KindUnreachable: DNS, TLS, connect, timeout, 404 at the endpoint, 5xx.
	KindUnreachable
	// KindTool is a tool that answered with an error.
	KindTool
	// KindProtocol is an answer the CLI could not read.
	KindProtocol
	// KindRefused is a call the CLI itself refused to make.
	KindRefused
)

// Error is every failure this package returns.
type Error struct {
	Kind       Kind
	HTTPStatus int
	// Code is the tool error's stable prefix (not_permitted, not_found,
	// rate_limited, already_exists, invalid_argument, …), when it has one.
	Code string
	// Detail is human text, already redacted.
	Detail string
}

func (e *Error) Error() string { return e.Detail }

// Options configure one connection.
type Options struct {
	Endpoint string
	Token    secret.Secret // empty for an anonymous connection
	Timeout  time.Duration
	// Strict decodes results with DisallowUnknownFields (tests only).
	Strict bool
}

// Client is one MCP session.
type Client struct {
	cs   *mcp.ClientSession
	rt   *authRT
	opts Options
}

type authRT struct {
	base  http.RoundTripper
	token secret.Secret
	mu    sync.Mutex
	last  int
}

func (a *authRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", buildinfo.UserAgent())
	if !a.token.Empty() {
		r.Header.Set("Authorization", "Bearer "+a.token.Reveal())
	}
	resp, err := a.base.RoundTrip(r)
	a.mu.Lock()
	if err == nil {
		a.last = resp.StatusCode
	}
	a.mu.Unlock()
	return resp, err
}

func (a *authRT) status() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// Dial opens a session. A failure is always an *Error.
func Dial(ctx context.Context, opts Options) (*Client, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	rt := &authRT{base: http.DefaultTransport, token: opts.Token}
	hc := newHTTPClient(rt, opts.Timeout)
	client := mcp.NewClient(&mcp.Implementation{Name: "metiche-cli", Version: buildinfo.Version}, nil)
	dctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	cs, err := client.Connect(dctx, &mcp.StreamableClientTransport{
		Endpoint:             opts.Endpoint,
		HTTPClient:           hc,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, classify(err, rt.status(), opts.Endpoint)
	}
	return &Client{cs: cs, rt: rt, opts: opts}, nil
}

// newHTTPClient never follows a redirect: the bearer must not follow one
// anywhere.
func newHTTPClient(rt http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: rt,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("refusing to follow a redirect from the metiche endpoint")
		},
	}
}

// Close ends the session.
func (c *Client) Close() {
	if c != nil && c.cs != nil {
		_ = c.cs.Close()
	}
}

// Call runs one allowed tool and decodes its JSON result into out.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any, out any) error {
	if !AllowedTools[tool] {
		return &Error{Kind: KindRefused, Detail: "the metiche CLI never calls " + tool}
	}
	if args == nil {
		args = map[string]any{}
	}
	cctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	res, err := c.cs.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		if strings.Contains(err.Error(), "tool not found") || strings.Contains(err.Error(), "unknown tool") {
			return &Error{Kind: KindTool, Code: "unknown_tool", Detail: "this metiche server has no " + tool + " tool"}
		}
		return classify(err, c.rt.status(), c.opts.Endpoint)
	}
	text := ""
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			text = tc.Text
			break
		}
	}
	if res.IsError {
		code, detail := toolErrorCode(text)
		return &Error{Kind: KindTool, Code: code, Detail: secret.Default.Redact(detail)}
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	if c.opts.Strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(out); err != nil {
		return &Error{Kind: KindProtocol, Detail: fmt.Sprintf("could not read %s's answer: %v", tool, err)}
	}
	return nil
}

// RequiredParams is the required list of a tool's input schema, and whether
// the server has the tool at all.
func (c *Client) RequiredParams(ctx context.Context, tool string) ([]string, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	var cursor string
	for {
		res, err := c.cs.ListTools(cctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, false, classify(err, c.rt.status(), c.opts.Endpoint)
		}
		for _, t := range res.Tools {
			if t.Name != tool {
				continue
			}
			raw, _ := json.Marshal(t.InputSchema)
			var s struct {
				Required []string `json:"required"`
			}
			_ = json.Unmarshal(raw, &s)
			return s.Required, true, nil
		}
		if res.NextCursor == "" {
			return nil, false, nil
		}
		cursor = res.NextCursor
	}
}

var codePrefix = regexp.MustCompile(`^([a-z_]+): `)

// toolErrorCode reads the stable code at the start of a tool error (§4), and
// maps the older unprefixed membership refusals to not_found.
func toolErrorCode(text string) (string, string) {
	if m := codePrefix.FindStringSubmatch(text); m != nil {
		return m[1], text
	}
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "code=provider_unavailable"):
		return "unavailable", text
	case strings.Contains(lower, "you are not a member of that team"),
		strings.Contains(lower, "no active team with slug"),
		strings.Contains(lower, "your membership of that team has been revoked"):
		return "not_found", text
	case strings.Contains(lower, "so this call needs a team_slug"):
		return "ambiguous_team", text
	case strings.Contains(lower, "too many"):
		return "rate_limited", text
	case strings.Contains(lower, "needs your metiche token"):
		return "unauthenticated", text
	}
	return "", text
}

func classify(err error, status int, endpoint string) *Error {
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	detail := secret.Default.Redact(err.Error())
	switch {
	case status == http.StatusUnauthorized:
		return &Error{Kind: KindUnauthorized, HTTPStatus: status, Detail: "metiche rejected the token (HTTP 401)"}
	case status == http.StatusNotFound:
		return &Error{Kind: KindUnreachable, HTTPStatus: status,
			Detail: "nothing answers MCP at " + endpoint + " (HTTP 404). The path is /v1/mcp."}
	case status >= 500:
		return &Error{Kind: KindUnreachable, HTTPStatus: status, Detail: fmt.Sprintf("metiche answered HTTP %d: %s", status, detail)}
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || status == 0 {
		return &Error{Kind: KindUnreachable, HTTPStatus: status, Detail: "cannot reach " + endpoint + ": " + detail}
	}
	return &Error{Kind: KindUnreachable, HTTPStatus: status, Detail: fmt.Sprintf("metiche answered HTTP %d: %s", status, detail)}
}
