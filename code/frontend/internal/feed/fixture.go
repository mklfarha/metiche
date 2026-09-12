package feed

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// Fixture replays a JSONL recording as though it were happening now.
//
// Pacing comes from the recording's own occurred_at deltas divided by Speed,
// so the shape of the story survives — a burst of three events inside a second
// still arrives as a burst. The first Warmup events are emitted immediately and
// back-dated, so a board opened cold shows a team already mid-flight instead of
// an empty grid that fills in over two minutes.
type Fixture struct {
	// FS is where Path is read from. Nil means the real filesystem; the server
	// passes an embedded FS so the binary runs with no arguments and no files
	// next to it.
	FS   fs.FS
	Path string
	// Speed compresses the recording's own timeline. 1 is real time.
	Speed float64
	// Warmup events are delivered instantly at start, with their timestamps
	// shifted into the recent past.
	Warmup int
	// MaxGap caps dead air between events, so a recording with a ten-minute
	// pause in it does not stall a demo.
	MaxGap time.Duration
	// Loop restarts the recording when it ends, renumbering sequences so they
	// stay monotonic. Useful for leaving a board up on a screen.
	Loop bool
}

// Name implements Feed.
func (f *Fixture) Name() string { return "fixture:" + f.Path }

// Load parses the recording without replaying it.
func (f *Fixture) Load() ([]model.Event, error) {
	var (
		file fs.File
		err  error
	)
	if f.FS != nil {
		file, err = f.FS.Open(f.Path)
	} else {
		file, err = os.Open(f.Path)
	}
	if err != nil {
		return nil, fmt.Errorf("open fixture: %w", err)
	}
	defer file.Close()

	var events []model.Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		trimmed := trimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal(trimmed, &ev); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", f.Path, line, err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read fixture: %w", err)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
	return events, nil
}

func trimSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r') {
		i++
	}
	j := len(b)
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}

// Stream implements Feed.
func (f *Fixture) Stream(ctx context.Context, after int64) (<-chan model.Event, error) {
	events, err := f.Load()
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		out := make(chan model.Event)
		close(out)
		return out, nil
	}

	speed := f.Speed
	if speed <= 0 {
		speed = 1
	}
	maxGap := f.MaxGap
	if maxGap <= 0 {
		maxGap = 3 * time.Second
	}
	warmup := f.Warmup
	if warmup > len(events) {
		warmup = len(events)
	}

	// Virtual offsets from the recording's own clock, with long pauses clipped.
	offsets := make([]time.Duration, len(events))
	var acc time.Duration
	for i := range events {
		if i > 0 {
			gap := events[i].OccurredAt.Sub(events[i-1].OccurredAt)
			if gap < 0 {
				gap = 0
			}
			gap = time.Duration(float64(gap) / speed)
			if gap > maxGap {
				gap = maxGap
			}
			acc += gap
		}
		offsets[i] = acc
	}

	out := make(chan model.Event, 64)
	go func() {
		defer close(out)
		seqBase := int64(0)
		for {
			start := time.Now()
			if warmup > 0 {
				start = start.Add(-offsets[warmup-1])
			}
			for i, ev := range events {
				ev.Sequence += seqBase
				ev.OccurredAt = start.Add(offsets[i])
				if i >= warmup {
					wait := time.Until(ev.OccurredAt)
					if wait > 0 {
						select {
						case <-ctx.Done():
							return
						case <-time.After(wait):
						}
					}
				}
				if ev.Sequence <= after {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case out <- ev:
				}
			}
			if !f.Loop {
				return
			}
			seqBase = events[len(events)-1].Sequence + seqBase
			warmup = 0
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return out, nil
}
