package mcp

import (
	"context"
	"fmt"
)

// reviewrender.go renders the inline review block both reviewers share
// (docs/DUPLICATES.md §3.1 "One renderer").
//
// The duplicate reviewer and the decision reviewer each append the pairs they
// assigned to the request's ReviewPairs and never touch the envelope. This hook
// runs last in the chain and writes the block and the note lead exactly once,
// so a second reviewer can never overwrite the first one's block (F6). For a
// response with only decision pairs the bytes are exactly what the decision
// reviewer wrote before duplicate work existed.

// NewReviewRenderer returns the last hook in the chain: it renders the request's review pairs
// into Envelope.Review with the cut order and appends the note lead once. For decision pairs
// alone its output is byte-identical to the decision reviewer's before this change.
func NewReviewRenderer() DetectHook {
	return func(ctx context.Context, _ *TxContext, m *Mutation) ([]ConflictNotice, error) {
		renderReviewPairs(pathDetectionFrom(ctx), m)
		return nil, nil
	}
}

// renderReviewPairs writes the block and the note lead for the pairs assigned
// in this call, once. It is a no-op without a request, without pairs, or when
// the pairs were already rendered.
func renderReviewPairs(req *pathDetectionRequest, m *Mutation) {
	if req == nil || m == nil || req.reviewRendered || len(req.ReviewPairs) == 0 {
		return
	}
	req.reviewRendered = true
	if !req.reviewBlockOff {
		if raw, ok := renderReviewBlock(req.ReviewPairs); ok {
			m.Envelope.Review = raw
		}
	}
	how := "call get_review_context, then report_judgement"
	if m.Envelope.Review != nil {
		how = "see review, then report_judgement"
	}
	lead := fmt.Sprintf("judge %d pair(s) against your plan before you edit: %s", len(req.ReviewPairs), how)
	if m.Envelope.Note == "" {
		m.Envelope.Note = lead
	} else {
		m.Envelope.Note += "; " + lead
	}
}
