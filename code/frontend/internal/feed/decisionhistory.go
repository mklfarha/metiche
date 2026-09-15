package feed

import (
	"context"
	"net/url"
	"strconv"

	"github.com/mklfarha/metiche/frontend/internal/model"
)

// DecisionHistory reads a team's past decisions from the backend: the ones
// superseded or revoked, which GET /decisions no longer returns, and any one
// decision with the wordings it was recorded with.
//
// Like BoardHistory, a Live implements it and reads with exactly the principal
// it reads the board with; a Fixture does not, so a demo board has no way to
// ask the backend anything. It is its own interface rather than a fourth
// BoardHistory method so nothing that implements that one has to change.
type DecisionHistory interface {
	DecisionHistory(ctx context.Context, q model.DecisionQuery) (model.DecisionPage, error)
}

var _ DecisionHistory = (*Live)(nil)

// DecisionHistory implements DecisionHistory: GET /v1/teams/{slug}/decisions/history.
func (l *Live) DecisionHistory(ctx context.Context, in model.DecisionQuery) (model.DecisionPage, error) {
	q := url.Values{}
	if in.Key != "" {
		q.Set("key", in.Key)
	} else {
		setIf(q, "status", in.Status)
		setIf(q, "cursor", in.Cursor)
	}
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	var wire decisionHistoryWire
	if err := l.readHistory(ctx, l.teamURL("decisions/history", q), &wire); err != nil {
		return model.DecisionPage{}, err
	}
	return wire.page(), nil
}
