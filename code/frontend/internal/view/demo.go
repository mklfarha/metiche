package view

import "context"

type demoKey struct{}

// WithDemo marks a render as a demo board: a recording replaying on a loop,
// not a real team. The layout reads it and says so on every page of that
// board.
//
// It travels on the context rather than on state.Snapshot because whether a
// board is a recording is a fact about how this server registered it, not
// about the team's state — and a snapshot folded from a recording looks
// exactly like a live one, which is the whole problem the label solves.
func WithDemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, demoKey{}, true)
}

// IsDemo reports whether WithDemo marked this render.
func IsDemo(ctx context.Context) bool {
	v, _ := ctx.Value(demoKey{}).(bool)
	return v
}
