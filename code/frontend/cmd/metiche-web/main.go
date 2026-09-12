// Command metiche-web serves the live team board.
//
//	go run ./cmd/metiche-web
//
// With no arguments it replays the embedded fixture recordings, which is the
// mode to develop and demo in while the coordination backend is being built.
// Point -backend at a real metiche server and the same board runs against the
// live stream instead; nothing above the feed package can tell the difference.
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
		backend  = flag.String("backend", "", "metiche backend base URL; empty replays the embedded fixtures")
		fixtures = flag.String("fixtures", "", "directory of .jsonl recordings; empty uses the embedded ones")
		speed    = flag.Float64("speed", 6, "fixture replay speed multiplier")
		warmup   = flag.Int("warmup", 30, "fixture events delivered instantly at start, so a cold board is not empty")
		loop     = flag.Bool("loop", false, "restart the recording when it ends")
		teamsCSV = flag.String("teams", "demo", "comma-separated team slugs, when -backend is set")
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

	if *backend != "" {
		// Read once, keep it in memory, and never log it or put it in a flag
		// value. The variable is then cleared from this process's environment
		// so it is not inherited by anything spawned later and does not show
		// up in /proc/self/environ.
		token := strings.TrimSpace(os.Getenv(*tokenEnv))
		_ = os.Unsetenv(*tokenEnv)
		if token == "" {
			log.Warn("no backend token; the read API will reject this if it requires one",
				"expected_env", *tokenEnv)
		}
		for _, slug := range strings.Split(*teamsCSV, ",") {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			f := &feed.Live{
				BaseURL: *backend, Slug: slug, Token: token,
				TimelineBackfill: *backfill, Logger: log,
			}
			if _, err := srv.AddTeam(ctx, slug, slug, "", f); err != nil {
				log.Error("add team", "slug", slug, "err", err)
				os.Exit(1)
			}
			log.Info("team registered", "slug", slug, "feed", f.Name())
		}
	} else {
		root := frontend.Fixtures()
		if *fixtures != "" {
			root = os.DirFS(*fixtures)
		}
		names, err := fs.Glob(root, "*.jsonl")
		if err != nil || len(names) == 0 {
			log.Error("no fixture recordings found", "err", err)
			os.Exit(1)
		}
		sort.Strings(names)
		for _, name := range names {
			f := &feed.Fixture{
				FS: root, Path: name,
				Speed: *speed, Warmup: *warmup, Loop: *loop,
				MaxGap: 3 * time.Second,
			}
			slug, teamName, code, err := describe(f, name)
			if err != nil {
				log.Error("read fixture", "file", name, "err", err)
				os.Exit(1)
			}
			if _, err := srv.AddTeam(ctx, slug, teamName, code, f); err != nil {
				log.Error("add team", "slug", slug, "err", err)
				os.Exit(1)
			}
			log.Info("team registered", "slug", slug, "name", teamName, "feed", f.Name())
		}
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
