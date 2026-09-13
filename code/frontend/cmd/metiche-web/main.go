// Command metiche-web serves the live team board.
//
//	go run ./cmd/metiche-web
//
// With no arguments it replays the embedded fixture recordings. Point -backend
// at a real metiche server and it serves real teams as well — MIXED MODE: the
// recordings stay up as clearly labelled demo boards (-demo, default true), and
// a request for any other slug asks the backend whether that team exists and
// may be shown (-discover, default true). -teams is only a pre-warm list.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	frontend "github.com/mklfarha/metiche/frontend"
	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/web"
)

func main() {
	var (
		addr     = flag.String("addr", ":8787", "listen address")
		backend  = flag.String("backend", "", "metiche backend base URL; empty replays the embedded fixtures only")
		fixtures = flag.String("fixtures", "", "directory of .jsonl recordings; empty uses the embedded ones")
		speed    = flag.Float64("speed", 6, "fixture replay speed multiplier")
		warmup   = flag.Int("warmup", 30, "fixture events delivered instantly at start, so a cold board is not empty")
		loop     = flag.Bool("loop", false, "restart the recording when it ends (always on for demo boards when -backend is set)")
		demo     = flag.Bool("demo", true,
			"with -backend, also serve the recordings as demo boards, looping forever; without -backend they are always served")
		teamsCSV = flag.String("teams", "", "comma-separated live team slugs to register at startup (optional pre-warm), when -backend is set")
		discover = flag.Bool("discover", true,
			"with -backend, register a live team the first time its board is requested, if the backend confirms it")
		maxTeams    = flag.Int("max-discovered-teams", 50, "cap on teams registered through -discover")
		notFoundTTL = flag.Duration("discover-notfound-ttl", 30*time.Second, "how long a backend 404 for a slug is remembered")
		// The flag names the VARIABLE, never the value. A bearer token passed
		// as an argument lands in shell history and in every `ps` on the box,
		// and a token in a file lands in a commit eventually.
		tokenEnv = flag.String("backend-token-env", "METICHE_BOARD_TOKEN",
			"name of the environment variable holding the backend read-API bearer token")
		backfill = flag.Int64("backfill", 200,
			"events of history to pull into the timeline behind the snapshot cursor, when -backend is set")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := web.NewServer(frontend.Static(), log)

	opts := options{
		backend: *backend, speed: *speed, warmup: *warmup, loop: *loop, demo: *demo,
		teams: splitCSV(*teamsCSV), discover: *discover, maxTeams: *maxTeams,
		notFoundTTL: *notFoundTTL, backfill: *backfill,
		fixtures: frontend.Fixtures(),
	}
	if *fixtures != "" {
		opts.fixtures = os.DirFS(*fixtures)
	}
	if *backend != "" {
		// Read once, keep it in memory, and never log it or put it in a flag
		// value. The variable is then cleared from this process's environment
		// so it is not inherited by anything spawned later and does not show
		// up in /proc/self/environ.
		opts.token = strings.TrimSpace(os.Getenv(*tokenEnv))
		_ = os.Unsetenv(*tokenEnv)
		if opts.token == "" {
			log.Warn("no backend token; only PUBLIC teams can be shown", "expected_env", *tokenEnv)
		}
	}
	if err := registerTeams(ctx, srv, opts, log); err != nil {
		log.Error("register teams", "err", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout on purpose: /stream is meant to stay open, and a
		// write deadline would cut the board off every N seconds.
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	shown := *addr
	if strings.HasPrefix(shown, ":") {
		shown = "localhost" + shown
	}
	fmt.Fprintf(os.Stderr, "\n  metiche board  →  http://%s/\n\n", shown)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// options is everything registerTeams needs, taken out of flag parsing so the
// modes can be tested without a process.
type options struct {
	backend  string
	token    string
	backfill int64

	fixtures fs.FS
	speed    float64
	warmup   int
	loop     bool
	demo     bool

	teams       []string
	discover    bool
	maxTeams    int
	notFoundTTL time.Duration

	client *http.Client // nil: http.DefaultClient
}

// registerTeams decides the mode and registers its teams.
//
//	no -backend          the recordings, as demo boards. Nothing else exists.
//	-backend             MIXED: the recordings as demo boards (unless -demo=false),
//	                     then -teams pre-warmed as live boards, then discovery.
//
// Demo boards are registered FIRST, and that order is what reserves their
// slugs: a registered slug is never looked up on the backend, and a pre-warmed
// live team whose slug a demo already holds is refused (logged, not fatal)
// rather than silently shadowing — or being shadowed by — a recording.
func registerTeams(ctx context.Context, srv *web.Server, o options, log *slog.Logger) error {
	live := o.backend != ""
	if !live || o.demo {
		// A demo left up next to real teams should never go still, so with a
		// backend the recordings always loop.
		if err := registerDemos(ctx, srv, o.fixtures, o.speed, o.warmup, o.loop || live, log); err != nil {
			return err
		}
	}
	if !live {
		return nil
	}

	for _, slug := range o.teams {
		if t, ok := srv.Lookup(slug); ok && t.Demo {
			log.Error("refusing to pre-warm a live team over a demo slug; that slug is reserved for the recording",
				"slug", slug)
			continue
		}
		f := newLive(o, slug, log)
		if _, err := srv.AddLiveTeam(ctx, slug, f); err != nil {
			return fmt.Errorf("add team %s: %w", slug, err)
		}
		log.Info("live team registered", "slug", slug, "feed", f.Name())
	}

	if o.discover {
		srv.EnableDiscovery(ctx, web.Discovery{
			Probe: func(ctx context.Context, slug string) (feed.ProbeResult, error) {
				return feed.ProbeTeam(ctx, o.client, o.backend, o.token, slug)
			},
			NewFeed:     func(slug string) feed.Feed { return newLive(o, slug, log) },
			MaxTeams:    o.maxTeams,
			NotFoundTTL: o.notFoundTTL,
		})
		log.Info("live team discovery on", "max_teams", o.maxTeams, "notfound_ttl", o.notFoundTTL)
	}
	return nil
}

func newLive(o options, slug string, log *slog.Logger) *feed.Live {
	return &feed.Live{
		BaseURL: o.backend, Slug: slug, Token: o.token, Client: o.client,
		TimelineBackfill: o.backfill, Logger: log,
	}
}

// registerDemos registers every recording in root as a demo board.
func registerDemos(ctx context.Context, srv *web.Server, root fs.FS, speed float64, warmup int, loop bool, log *slog.Logger) error {
	names, err := fs.Glob(root, "*.jsonl")
	if err != nil {
		return fmt.Errorf("find fixture recordings: %w", err)
	}
	if len(names) == 0 {
		return errors.New("no fixture recordings found")
	}
	sort.Strings(names)
	for _, name := range names {
		f := &feed.Fixture{
			FS: root, Path: name,
			Speed: speed, Warmup: warmup, Loop: loop,
			MaxGap: 3 * time.Second,
		}
		recorded, teamName, code, err := describe(f, name)
		if err != nil {
			return fmt.Errorf("read fixture %s: %w", name, err)
		}
		slug := demoSlug(recorded)
		if _, err := srv.AddDemoTeam(ctx, slug, teamName, code, f); err != nil {
			return fmt.Errorf("add demo team %s: %w", slug, err)
		}
		log.Info("demo team registered", "slug", slug, "name", teamName, "feed", f.Name(), "loop", loop)
	}
	return nil
}

// demoSlug puts every recording under one visibly-demo name: "demo" itself, or
// "demo-<recorded slug>". The board's routes are all /t/{slug} and the backend
// can mint any [a-z0-9-] slug, so no spelling is truly outside its namespace;
// what this buys is that the reserved slugs are few, predictable and read as
// a demo in the address bar, and a real team is only shadowed if it is itself
// called "demo…" AND matches a recording exactly.
func demoSlug(recorded string) string {
	if recorded == "demo" || strings.HasPrefix(recorded, "demo-") {
		return recorded
	}
	return "demo-" + recorded
}

func splitCSV(csv string) []string {
	var out []string
	for _, v := range strings.Split(csv, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// describe peeks at a recording's first event for the team it belongs to,
// falling back to the filename so an unlabelled recording still works.
func describe(f *feed.Fixture, name string) (slug, teamName, joinCode string, err error) {
	events, err := f.Load()
	if err != nil {
		return "", "", "", err
	}
	slug = strings.TrimSuffix(path.Base(name), ".jsonl")
	teamName = slug
	for _, ev := range events {
		if ev.Kind != "team_created" {
			continue
		}
		var p struct {
			Slug     string `json:"slug"`
			Name     string `json:"name"`
			JoinCode string `json:"join_code"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			if p.Slug != "" {
				slug = p.Slug
			}
			if p.Name != "" {
				teamName = p.Name
			}
			joinCode = p.JoinCode
		}
		break
	}
	return slug, teamName, joinCode, nil
}
