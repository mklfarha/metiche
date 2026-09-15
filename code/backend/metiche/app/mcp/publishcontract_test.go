package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mklfarha/metiche/backend/app/coordination"
	"github.com/mklfarha/metiche/backend/enums"
)

func shapeValue(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("test shape %q: %v", raw, err)
	}
	return v
}

// TestParseContractInputCanonicalAcrossDialectsAndKeyOrder: the server
// hashes, and two agents describing one shape in different key order or a
// different dialect land on one hash; spellings of one endpoint land on one key.
func TestParseContractInputCanonicalAcrossDialectsAndKeyOrder(t *testing.T) {
	variants := []PublishContractParams{
		{Key: "POST /api/login", Role: "produces",
			Request: shapeValue(t, `{"email!":"string","remember":"bool"}`), Response: shapeValue(t, `{"token!":"string","user":{"id!":"uuid"}}`)},
		{Key: "post  /api/login/", Role: "producer", Kind: "http",
			Request: shapeValue(t, `{"remember":"boolean","email":"string!"}`), Response: shapeValue(t, `{"user":{"id":{"type":"uuid","required":true}},"token":{"type":"string","required":true,"description":"a JWT"}}`)},
	}
	var hash, norm string
	for i, v := range variants {
		in, err := parseContractInput(v)
		if err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		t.Logf("variant %d: key_norm=%q hash=%s canonical=%s", i, in.KeyNorm, in.Hash, in.Canonical)
		if i == 0 {
			hash, norm = in.Hash, in.KeyNorm
			continue
		}
		if in.Hash != hash {
			t.Errorf("variant %d hashes to %s, want %s", i, in.Hash, hash)
		}
		if in.KeyNorm != norm {
			t.Errorf("variant %d normalizes to %q, want %q", i, in.KeyNorm, norm)
		}
	}
}

// TestParseContractInputRefusesUnboundedInput: size, depth, value count, field
// count and key length are all refused before anything is written.
func TestParseContractInputRefusesUnboundedInput(t *testing.T) {
	deep := `"string"`
	for i := 0; i < MaxContractShapeDepth+2; i++ {
		deep = `{"f":` + deep + `}`
	}
	var many strings.Builder
	many.WriteString("{")
	for i := 0; i < MaxContractFields+5; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`"f` + strings.Repeat("x", 3) + string(rune('a'+i%26)) + strings.Repeat("y", i%7) + `_` + itoa(int64(i)) + `":"string"`)
	}
	many.WriteString("}")
	var nodes strings.Builder
	nodes.WriteString(`{"a":[`)
	for i := 0; i < MaxContractShapeNodes+10; i++ {
		if i > 0 {
			nodes.WriteString(",")
		}
		nodes.WriteString(`"string"`)
	}
	nodes.WriteString("]}")
	big := `{"blob":"` + strings.Repeat("s", MaxContractShapeBytes) + `"}`

	cases := []struct {
		name string
		args PublishContractParams
		want string
	}{
		{"no key", PublishContractParams{Role: "produces", Response: shapeValue(t, `{}`)}, "key is required"},
		{"long key", PublishContractParams{Key: "GET /" + strings.Repeat("a", MaxContractKeyChars), Role: "produces", Response: shapeValue(t, `{}`)}, "limit is 200"},
		{"no role", PublishContractParams{Key: "GET /x", Response: shapeValue(t, `{}`)}, "role is required"},
		{"bad role", PublishContractParams{Key: "GET /x", Role: "maybe", Response: shapeValue(t, `{}`)}, "role must be"},
		{"bad kind", PublishContractParams{Key: "GET /x", Role: "consumes", Kind: "telepathy", Response: shapeValue(t, `{}`)}, "kind must be"},
		{"no shape", PublishContractParams{Key: "GET /x", Role: "consumes"}, "send the shape"},
		{"too deep", PublishContractParams{Key: "GET /x", Role: "consumes", Response: shapeValue(t, deep)}, "nested deeper"},
		{"too many values", PublishContractParams{Key: "GET /x", Role: "consumes", Response: shapeValue(t, nodes.String())}, "more than 512 values"},
		{"too many bytes", PublishContractParams{Key: "GET /x", Role: "consumes", Response: shapeValue(t, big)}, "bytes and the limit"},
		{"too many fields", PublishContractParams{Key: "GET /x", Role: "consumes", Response: shapeValue(t, many.String())}, "fields and the limit"},
	}
	for _, c := range cases {
		if _, err := parseContractInput(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
		} else {
			t.Logf("%s: refused: %v", c.name, err)
		}
	}
	if _, err := parseContractInput(PublishContractParams{Key: "GET /x", Role: "consumes", Response: shapeValue(t, `{}`)}); err != nil {
		t.Errorf("an empty response object should be accepted: %v", err)
	}
}

func issue(kind coordination.ContractIssueKind, sev coordination.Severity) coordination.ContractIssue {
	return coordination.ContractIssue{Kind: kind, Path: "f", Severity: sev}
}

// TestContractFault: the direction decides who has to change. A missing out
// field is the producer's to add; a missing in field is the consumer's to send.
func TestContractFault(t *testing.T) {
	cases := []struct {
		name   string
		issues []coordination.ContractIssue
		want   string
	}{
		{"missing out", []coordination.ContractIssue{issue(coordination.IssueMissingOut, coordination.SeverityHigh)}, FaultProducer},
		{"missing in", []coordination.ContractIssue{issue(coordination.IssueMissingIn, coordination.SeverityHigh)}, FaultConsumer},
		{"type", []coordination.ContractIssue{issue(coordination.IssueTypeMismatch, coordination.SeverityCritical)}, FaultBoth},
		{"both directions", []coordination.ContractIssue{issue(coordination.IssueMissingOut, coordination.SeverityHigh), issue(coordination.IssueMissingIn, coordination.SeverityHigh)}, FaultBoth},
		{"worse type wins", []coordination.ContractIssue{issue(coordination.IssueMissingOut, coordination.SeverityHigh), issue(coordination.IssueTypeMismatch, coordination.SeverityCritical)}, FaultBoth},
		{"worse missing out wins", []coordination.ContractIssue{issue(coordination.IssueMissingOut, coordination.SeverityCritical), issue(coordination.IssueMissingIn, coordination.SeverityHigh)}, FaultProducer},
	}
	for _, c := range cases {
		if got := ContractFault(c.issues); got != c.want {
			t.Errorf("%s: fault = %q, want %q", c.name, got, c.want)
		}
	}
}

func sideWith(t *testing.T, session string, role enums.AssertionRole, raw string) contractSide {
	t.Helper()
	s, err := coordination.ParseShape(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return contractSide{SessionUUID: session, SessionKey: session, AgentLabel: "test", Role: role,
		ContractKey: "POST /api/login", Hash: coordination.ShapeHash(s), Shape: s, ShapeOK: true}
}

// TestDecideContractSettlement: converged when shapes agree, superseded when a
// side is gone, open otherwise.
func TestDecideContractSettlement(t *testing.T) {
	prod := sideWith(t, "S-1", enums.ASSERTION_ROLE_PRODUCES, `{"out":{"token!":"string"}}`)
	fixed := sideWith(t, "S-1", enums.ASSERTION_ROLE_PRODUCES, `{"out":{"token!":"string","expires_at":"timestamp"}}`)
	cons := sideWith(t, "S-2", enums.ASSERTION_ROLE_CONSUMES, `{"out":{"token!":"string","expires_at!":"timestamp"}}`)
	parts := func(p, c *contractSide) []contractSettlePart {
		return []contractSettlePart{
			{SessionUUID: "S-1", Role: enums.ASSERTION_ROLE_PRODUCES, Status: enums.SESSION_STATUS_LIVE, Current: p},
			{SessionUUID: "S-2", Role: enums.ASSERTION_ROLE_CONSUMES, Status: enums.SESSION_STATUS_LIVE, Current: c},
		}
	}
	mismatch := enums.ConflictKind(enums.CONFLICT_KIND_CONTRACT_MISMATCH)
	if _, ok := decideContractSettlement(mismatch, RuleContractMissingOut, parts(&prod, &cons), nil); ok {
		t.Error("a mismatch that still disagrees was settled")
	}
	if r, ok := decideContractSettlement(mismatch, RuleContractMissingOut, parts(&fixed, &cons), nil); !ok || r != enums.CONFLICT_RESOLUTION_CONVERGED {
		t.Errorf("converged shapes: resolution %v ok=%v, want converged", r, ok)
	}
	if r, ok := decideContractSettlement(mismatch, RuleContractMissingOut, parts(&prod, nil), nil); !ok || r != enums.CONFLICT_RESOLUTION_SUPERSEDED {
		t.Errorf("a side gone: resolution %v ok=%v, want superseded", r, ok)
	}
	unclaimed := enums.ConflictKind(enums.CONFLICT_KIND_CONTRACT_UNCLAIMED)
	onlyCons := []contractSettlePart{{SessionUUID: "S-2", Role: enums.ASSERTION_ROLE_CONSUMES, Status: enums.SESSION_STATUS_LIVE, Current: &cons}}
	if _, ok := decideContractSettlement(unclaimed, "contract_unclaimed", onlyCons, nil); ok {
		t.Error("unclaimed with no producer was settled")
	}
	if r, ok := decideContractSettlement(unclaimed, "contract_unclaimed", onlyCons, &contractProducerRef{SessionKey: "S-1"}); !ok || r != enums.CONFLICT_RESOLUTION_CONVERGED {
		t.Errorf("unclaimed with a producer: %v %v, want converged", r, ok)
	}
}

// TestContractKeysAreVariants: /api/session vs /api/sessions is a variant;
// another verb, a far key, or two trivially short keys are not.
func TestContractKeysAreVariants(t *testing.T) {
	norm := func(k string) string { return coordination.NormalizeContractKey("http_endpoint", k) }
	cases := []struct {
		a, b string
		want bool
	}{
		{"GET /api/session", "GET /api/sessions", true},
		{"GET /api/user/{id}", "GET /api/users/:id", true},
		{"GET /api/session", "PUT /api/sessions", false},
		{"GET /api/session", "GET /api/bookings", false},
		{"GET /a", "GET /b", false},
		{"GET /api/session", "get /API/session/", false}, // same contract, not a variant
	}
	for _, c := range cases {
		if got := ContractKeysAreVariants(norm(c.a), norm(c.b)); got != c.want {
			t.Errorf("%q vs %q: variant = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestContractActionsAreActionableAndFit: never empty, name the tool, and the
// other side's copy fits what get_instructions delivers.
func TestContractActionsAreActionableAndFit(t *testing.T) {
	prod := sideWith(t, "S-1", enums.ASSERTION_ROLE_PRODUCES, `{"in":{"email!":"string","password!":"string"},"out":{"token!":"string"}}`)
	cons := sideWith(t, "S-2", enums.ASSERTION_ROLE_CONSUMES, `{"in":{"email":"string"},"out":{"token!":"string","expires_at!":"timestamp","refresh!":"string"}}`)
	f, ok := assessContractPair(prod, cons)
	if !ok {
		t.Fatal("expected a finding")
	}
	for _, reader := range []enums.AssertionRole{enums.ASSERTION_ROLE_PRODUCES, enums.ASSERTION_ROLE_CONSUMES} {
		for _, short := range []bool{false, true} {
			a := contractAction(f, reader, short)
			if a == "" || !strings.Contains(a, "publish_contract") && !short {
				t.Errorf("reader %v short=%v: action %q is not actionable", reader, short, a)
			}
			if short && utf8.RuneCountInString(a) > instructionTextChars {
				t.Errorf("short action is %d runes, over %d: %q", utf8.RuneCountInString(a), instructionTextChars, a)
			}
			t.Logf("reader=%s short=%v: %s", reader, short, a)
		}
	}
}
