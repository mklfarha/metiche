package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
	"go.uber.org/zap"

	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	agent_entity "github.com/mklfarha/metiche/backend/entity/agent"
)

// These need no database, and that is the point of the first one: a Handler
// built without a core (the tool-surface tests build exactly that) used to
// panic the moment a request carried a bearer token, because resolving it
// dereferenced a nil core. A store that is absent is "unauthenticated", the
// same opaque answer as a store that says no.

func TestIdentityByTokenWithNoDatabaseIsUnauthenticated(t *testing.T) {
	ctx := context.Background()

	if _, err := IdentityByToken(ctx, nil, fakeBearerValue); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("IdentityByToken with a nil db = %v, want ErrUnauthenticated", err)
	}
	if _, err := AccountByToken(ctx, nil, fakeBearerValue); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("AccountByToken with a nil db = %v, want ErrUnauthenticated", err)
	}

	hdl := NewHandler(nil, zap.NewNop())
	if _, err := hdl.resolveToken(ctx, fakeBearerValue); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("resolveToken on a handler with no core = %v, want ErrUnauthenticated", err)
	}
	var nilHandler *Handler
	if _, err := nilHandler.resolveToken(ctx, fakeBearerValue); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("resolveToken on a nil handler = %v, want ErrUnauthenticated", err)
	}

	// The HTTP edge: a bearer on a handler with no core is a 401, not a
	// crashed request, and the token is not echoed.
	reached := false
	mw := hdl.authMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+fakeBearerValue)
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || reached {
		t.Fatalf("no core + a bearer: status %d, reached the handler=%v; want 401 and not reached", rec.Code, reached)
	}
	if strings.Contains(rec.Body.String(), fakeBearerValue) {
		t.Fatal("the rejection echoed the presented token back")
	}
}

// WithAccount is an account-only identity (a legacy token), and
// AccountFromContext reads the person through an agent identity. Existing
// callers of both depend on that.
func TestIdentityContextWrappers(t *testing.T) {
	acct := account_entity.Account{ID: uuid.Must(uuid.NewV4())}

	id, ok := IdentityFromContext(WithAccount(context.Background(), acct))
	if !ok || id.Agent != nil || id.Account.ID != acct.ID {
		t.Fatalf("WithAccount -> identity %+v ok=%v, want the account and no agent", id, ok)
	}

	ag := &agent_entity.Agent{ID: uuid.Must(uuid.NewV4()), AccountUUID: acct.ID}
	ctx := WithIdentity(context.Background(), Identity{Account: acct, Agent: ag})
	got, ok := AccountFromContext(ctx)
	if !ok || got.ID != acct.ID {
		t.Fatalf("AccountFromContext through an agent identity = %v ok=%v, want %s", got.ID, ok, acct.ID)
	}
	if _, ok := IdentityFromContext(context.Background()); ok {
		t.Fatal("an empty context produced an identity")
	}
}
