package coordination

import (
	"encoding/json"
	"testing"
)

func mustContractShape(t *testing.T, raw string) Shape {
	t.Helper()
	s, err := ParseShape(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseShape(%s): unexpected error: %v", raw, err)
	}
	return s
}

func contractFieldStrings(s Shape) []string {
	out := make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		req, nul := "", ""
		if f.Required {
			req = "!"
		}
		if f.Nullable {
			nul = "?"
		}
		out = append(out, string(f.Direction)+" "+f.Path+" "+string(f.Type)+req+nul)
	}
	return out
}

func contractAssertFields(t *testing.T, s Shape, want []string) {
	t.Helper()
	got := contractFieldStrings(s)
	if len(got) != len(want) {
		t.Fatalf("field count = %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCanonicalBytesStable is the headline property: the same logical shape
// submitted three different ways must produce byte-identical canonical bytes.
// If this ever fails, COUNT(DISTINCT shape_hash) reports a mismatch between two
// agents who agree, and contract dedupe evaporates.
func TestCanonicalBytesStable(t *testing.T) {
	plain := `{"in":{"email":"string!","password":"string!"},
	           "out":{"token":"string!","user":{"id":"uuid!","email":"string"}}}`

	// Same shape, keys in a different order, gratuitous whitespace.
	reordered := "{\n\t\"out\" : {\n \"user\" : { \"email\" :\t\"string\" ,\n\"id\":\"uuid!\" } ,\n" +
		"  \"token\"  :  \"string!\"  }  ,\n  \"in\":{ \"password\":\"string!\" ,  \"email\" : \"string!\" }\n}"

	// Same shape again, written as descriptors carrying descriptions, examples
	// and defaults — every one of which must be stripped.
	documented := `{
	  "in": {
	    "email":    {"type":"string","required":true,"description":"the user's email address","example":"a@b.co"},
	    "password": {"type":"string","required":true,"description":"plaintext, hashed server-side","format":"password"}
	  },
	  "out": {
	    "token": {"type":"string","required":true,"example":"eyJhbGciOi...","description":"bearer token, 24h"},
	    "user":  {"id":"uuid!","email":"string"}
	  }
	}`

	base := CanonicalBytes(mustContractShape(t, plain))
	if len(base) == 0 {
		t.Fatal("canonical bytes are empty")
	}
	for name, variant := range map[string]string{"reordered": reordered, "documented": documented} {
		got := CanonicalBytes(mustContractShape(t, variant))
		if string(got) != string(base) {
			t.Errorf("%s canonical bytes differ\n got: %s\nwant: %s", name, got, base)
		}
		if h, want := ShapeHash(mustContractShape(t, variant)), ShapeHash(mustContractShape(t, plain)); h != want {
			t.Errorf("%s shape hash = %s, want %s", name, h, want)
		}
	}

	// And the canonical form is what we think it is - sorted by (direction,
	// path), no whitespace, no prose.
	want := `[{"d":"in","p":"email","t":"string","r":true,"n":false},` +
		`{"d":"in","p":"password","t":"string","r":true,"n":false},` +
		`{"d":"out","p":"token","t":"string","r":true,"n":false},` +
		`{"d":"out","p":"user","t":"object","r":false,"n":false},` +
		`{"d":"out","p":"user.email","t":"string","r":false,"n":false},` +
		`{"d":"out","p":"user.id","t":"uuid","r":true,"n":false}]`
	if string(base) != want {
		t.Errorf("canonical bytes =\n%s\nwant\n%s", base, want)
	}
}

// TestCanonicalBytesRepeatable guards the map-iteration trap directly: building
// the same shape many times must never vary.
func TestCanonicalBytesRepeatable(t *testing.T) {
	raw := `{"out":{"z":"int","a":"string!","m":{"q":"bool","b":"uuid"},"list":[{"k":"string","j":"int!"}]}}`
	first := string(CanonicalBytes(mustContractShape(t, raw)))
	for i := 0; i < 200; i++ {
		if got := string(CanonicalBytes(mustContractShape(t, raw))); got != first {
			t.Fatalf("run %d differs:\n got: %s\nwant: %s", i, got, first)
		}
	}
}

func TestContractTypeAliases(t *testing.T) {
	cases := []struct {
		spec string
		want ContractType
	}{
		{"string", ContractTypeString},
		{"str", ContractTypeString},
		{"text", ContractTypeString},
		{"varchar", ContractTypeString},
		{"char", ContractTypeString},
		{"email", ContractTypeString},
		{"url", ContractTypeString},
		{"uri", ContractTypeString},
		{"slug", ContractTypeString},
		{"int", ContractTypeInt},
		{"integer", ContractTypeInt},
		{"int32", ContractTypeInt},
		{"int64", ContractTypeInt},
		{"long", ContractTypeInt},
		{"number", ContractTypeInt},
		{"float", ContractTypeFloat},
		{"float32", ContractTypeFloat},
		{"float64", ContractTypeFloat},
		{"double", ContractTypeFloat},
		{"decimal", ContractTypeFloat},
		{"numeric", ContractTypeFloat},
		{"bool", ContractTypeBool},
		{"boolean", ContractTypeBool},
		{"timestamp", ContractTypeTimestamp},
		{"datetime", ContractTypeTimestamp},
		{"date-time", ContractTypeTimestamp},
		{"date", ContractTypeTimestamp},
		{"time", ContractTypeTimestamp},
		{"uuid", ContractTypeUUID},
		{"guid", ContractTypeUUID},
		{"json", ContractTypeJSON},
		{"jsonb", ContractTypeJSON},
		{"any", ContractTypeJSON},
		{"object", ContractTypeObject},
		{"struct", ContractTypeObject},
		{"map", ContractTypeObject},
		{"null", ContractTypeNull},
		{"nil", ContractTypeNull},
		{"none", ContractTypeNull},

		// Case and spacing are dialect, not meaning.
		{"String", ContractTypeString},
		{"  INT  ", ContractTypeInt},
		{"DateTime", ContractTypeTimestamp},

		// Unknown types degrade to json instead of erroring.
		{"Decimal128", ContractTypeJSON},
		{"whatever", ContractTypeJSON},
		{"", ContractTypeJSON},

		// Array spellings.
		{"string[]", ContractArrayOf(ContractTypeString)},
		{"[]uuid", ContractArrayOf(ContractTypeUUID)},
		{"array<int>", ContractArrayOf(ContractTypeInt)},
		{"list<str>", ContractArrayOf(ContractTypeString)},
		{"array", ContractArrayOf(ContractTypeJSON)},
		{"string[][]", ContractArrayOf(ContractTypeJSON)},
	}
	for _, c := range cases {
		got, _, _ := contractParseTypeSpec(c.spec)
		if got != c.want {
			t.Errorf("contractParseTypeSpec(%q) = %q, want %q", c.spec, got, c.want)
		}
		// Same answer through the real entry point.
		raw, err := json.Marshal(map[string]any{"out": map[string]any{"f": c.spec}})
		if err != nil {
			t.Fatal(err)
		}
		s := mustContractShape(t, string(raw))
		if len(s.Fields) != 1 || s.Fields[0].Type != c.want {
			t.Errorf("ParseShape with type %q gave %v, want single field of %q", c.spec, contractFieldStrings(s), c.want)
		}
	}
}

func TestContractTypeMarkers(t *testing.T) {
	s := mustContractShape(t, `{"out":{"a":"string!","b":"string?","c":"uuid","d":"string[]!","e":"string | null","f!":"int","g?":"bool"}}`)
	contractAssertFields(t, s, []string{
		"out a string!",
		"out b string?",
		"out c uuid",
		"out d array<string>!",
		"out e string?",
		"out f int!",
		"out g bool?",
	})
}

func TestParseShapeNestingAndArrays(t *testing.T) {
	raw := `{"out":{
	  "count":"int",
	  "items":[{"user_id":"uuid!","tags":["string"],"meta":{"score":"float!"}}],
	  "cursor":{"next":"string?","prev":"string?"},
	  "empty":[]
	}}`
	contractAssertFields(t, mustContractShape(t, raw), []string{
		"out count int",
		"out cursor object",
		"out cursor.next string?",
		"out cursor.prev string?",
		"out empty array<json>",
		"out items array<object>",
		"out items[].meta object",
		"out items[].meta.score float!",
		"out items[].tags array<string>",
		"out items[].user_id uuid!",
	})
}

func TestParseShapeDescriptorNesting(t *testing.T) {
	raw := `{"out":{
	  "rows":{"type":"array","description":"page of rows","items":{"type":"object","properties":{"id":"uuid!","name":{"type":"string","nullable":true}}}},
	  "total":{"type":"integer","required":true,"example":42}
	}}`
	contractAssertFields(t, mustContractShape(t, raw), []string{
		"out rows array<object>",
		"out rows[].id uuid!",
		"out rows[].name string?",
		"out total int!",
	})
}

// An object that merely happens to have a `type` key is payload, not metadata.
// Misreading it would silently delete every sibling field.
func TestParseShapeTypeKeyIsNotAlwaysDescriptor(t *testing.T) {
	raw := `{"out":{"event":{"type":"string","payload":"json"}}}`
	contractAssertFields(t, mustContractShape(t, raw), []string{
		"out event object",
		"out event.payload json",
		"out event.type string",
	})
}

func TestParseShapePathSnake(t *testing.T) {
	s := mustContractShape(t, `{"out":{"userId":"uuid","HTTPStatus":"int","user-name":"string","items":[{"createdAt":"datetime"}]}}`)
	want := map[string]string{
		"userId":            "user_id",
		"HTTPStatus":        "http_status",
		"user-name":         "user_name",
		"items":             "items",
		"items[].createdAt": "items[].created_at",
	}
	for _, f := range s.Fields {
		if w, ok := want[f.Path]; !ok {
			t.Errorf("unexpected field %q", f.Path)
		} else if f.PathSnake != w {
			t.Errorf("PathSnake(%q) = %q, want %q", f.Path, f.PathSnake, w)
		}
	}
}

func TestParseShapeDirectionInference(t *testing.T) {
	for _, key := range []string{"in", "input", "request", "req", "params", "body"} {
		s := mustContractShape(t, `{"`+key+`":{"email":"string!"}}`)
		if len(s.Fields) != 1 || s.Fields[0].Direction != DirectionIn {
			t.Errorf("top-level %q gave %v, want a single in field", key, contractFieldStrings(s))
		}
		if s.DirectionInferred {
			t.Errorf("top-level %q should not be flagged as inferred", key)
		}
	}
	for _, key := range []string{"out", "output", "response", "returns", "result"} {
		s := mustContractShape(t, `{"`+key+`":{"token":"string!"}}`)
		if len(s.Fields) != 1 || s.Fields[0].Direction != DirectionOut {
			t.Errorf("top-level %q gave %v, want a single out field", key, contractFieldStrings(s))
		}
		if s.DirectionInferred {
			t.Errorf("top-level %q should not be flagged as inferred", key)
		}
	}

	// No envelope at all: the whole body is out, and the caller is told.
	s := mustContractShape(t, `{"token":"string!","user":{"id":"uuid!"}}`)
	if !s.DirectionInferred {
		t.Error("shape without an envelope should set DirectionInferred")
	}
	if s.Note == "" {
		t.Error("shape without an envelope should carry a note")
	}
	contractAssertFields(t, s, []string{
		"out token string!",
		"out user object",
		"out user.id uuid!",
	})

	// Both envelopes at once, plus a loose sibling that must not vanish.
	both := mustContractShape(t, `{"in":{"email":"string!"},"out":{"token":"string!"},"etag":"string"}`)
	contractAssertFields(t, both, []string{
		"in email string!",
		"out etag string",
		"out token string!",
	})
	if both.DirectionInferred {
		t.Error("an explicit envelope must not set DirectionInferred")
	}
}

func TestParseShapeStructuralErrors(t *testing.T) {
	for _, raw := range []string{``, `   `, `not json`, `[1,2,3]`, `"just a string"`, `{"in":`} {
		if _, err := ParseShape(json.RawMessage(raw)); err == nil {
			t.Errorf("ParseShape(%q) = nil error, want a structural error", raw)
		}
	}
	// An unknown *type* is never a structural error.
	if _, err := ParseShape(json.RawMessage(`{"out":{"x":"Decimal128"}}`)); err != nil {
		t.Errorf("unknown type should not error: %v", err)
	}
}

func TestShapeFingerprintIgnoresRequiredNullableDirection(t *testing.T) {
	a := mustContractShape(t, `{"out":{"token":"string!","id":"uuid!"}}`)
	b := mustContractShape(t, `{"out":{"token":"string","id":"uuid?"}}`)
	c := mustContractShape(t, `{"in":{"token":"string!","id":"uuid!"}}`)
	d := mustContractShape(t, `{"out":{"token":"int!","id":"uuid!"}}`)

	if ShapeHash(a) == ShapeHash(b) {
		t.Error("ShapeHash must distinguish required/nullable differences")
	}
	if ShapeFingerprint(a) != ShapeFingerprint(b) {
		t.Error("ShapeFingerprint must ignore required/nullable")
	}
	if ShapeFingerprint(a) != ShapeFingerprint(c) {
		t.Error("ShapeFingerprint must ignore direction")
	}
	if ShapeFingerprint(a) == ShapeFingerprint(d) {
		t.Error("ShapeFingerprint must still distinguish types")
	}
}

func contractIssueKinds(issues []ContractIssue) []ContractIssueKind {
	out := make([]ContractIssueKind, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Kind)
	}
	return out
}

func TestCompareShapesMissingOut(t *testing.T) {
	producer := mustContractShape(t, `{"out":{"token":"string!"}}`)
	consumer := mustContractShape(t, `{"out":{"token":"string!","refresh_token":"string!","nice_to_have":"string"}}`)

	issues := CompareShapes(producer, consumer)
	if len(issues) != 1 {
		t.Fatalf("got %d issues %v, want 1 missing_out", len(issues), contractIssueKinds(issues))
	}
	got := issues[0]
	if got.Kind != IssueMissingOut || got.Path != "refresh_token" || got.Direction != DirectionOut {
		t.Errorf("issue = %+v, want missing_out on refresh_token/out", got)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %v, want high", got.Severity)
	}
	if got.Expected != "string" || got.Actual != "" {
		t.Errorf("expected/actual = %q/%q, want \"string\"/\"\"", got.Expected, got.Actual)
	}
	if got.Note == "" {
		t.Error("issue must carry an actionable note")
	}
}

func TestCompareShapesMissingIn(t *testing.T) {
	producer := mustContractShape(t, `{"in":{"email":"string!","password":"string!","remember":"bool"}}`)
	consumer := mustContractShape(t, `{"in":{"email":"string!"}}`)

	issues := CompareShapes(producer, consumer)
	if len(issues) != 1 {
		t.Fatalf("got %d issues %v, want 1 missing_in", len(issues), contractIssueKinds(issues))
	}
	got := issues[0]
	if got.Kind != IssueMissingIn || got.Path != "password" || got.Direction != DirectionIn {
		t.Errorf("issue = %+v, want missing_in on password/in", got)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %v, want high", got.Severity)
	}
}

// The same field name, required on one side, is a different agent's fault
// depending only on direction. This is the property `direction` exists for.
func TestCompareShapesDirectionFlipsFault(t *testing.T) {
	inSide := CompareShapes(
		mustContractShape(t, `{"in":{"nonce":"string!"}}`),
		mustContractShape(t, `{"in":{}}`),
	)
	if len(inSide) != 1 || inSide[0].Kind != IssueMissingIn || inSide[0].Direction != DirectionIn {
		t.Fatalf("in-side = %v, want a single missing_in", contractIssueKinds(inSide))
	}

	outSide := CompareShapes(
		mustContractShape(t, `{"out":{}}`),
		mustContractShape(t, `{"out":{"nonce":"string!"}}`),
	)
	if len(outSide) != 1 || outSide[0].Kind != IssueMissingOut || outSide[0].Direction != DirectionOut {
		t.Fatalf("out-side = %v, want a single missing_out", contractIssueKinds(outSide))
	}
}

func TestCompareShapesTypeMismatchFamilies(t *testing.T) {
	cases := []struct {
		name     string
		prodType string
		consType string
		want     Severity
	}{
		{"int vs float stays high (same numeric family)", "int", "float", SeverityHigh},
		{"uuid vs string stays high (same textual family)", "uuid", "string", SeverityHigh},
		{"timestamp vs string stays high", "timestamp", "string", SeverityHigh},
		{"string vs int crosses families", "string", "int", SeverityCritical},
		{"bool vs int crosses families", "bool", "int", SeverityCritical},
		{"string vs array crosses families", "string", "string[]", SeverityCritical},
		{"object vs json stays high (both structural)", "object", "json", SeverityHigh},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			producer := mustContractShape(t, `{"out":{"v":"`+c.prodType+`"}}`)
			consumer := mustContractShape(t, `{"out":{"v":"`+c.consType+`"}}`)
			issues := CompareShapes(producer, consumer)
			if len(issues) != 1 || issues[0].Kind != IssueTypeMismatch {
				t.Fatalf("got %v, want a single type_mismatch", contractIssueKinds(issues))
			}
			if issues[0].Severity != c.want {
				t.Errorf("severity = %v, want %v", issues[0].Severity, c.want)
			}
			if issues[0].Expected == issues[0].Actual {
				t.Errorf("expected/actual should differ, both %q", issues[0].Expected)
			}
		})
	}

	// The canonical cross-family case from the spec: a scalar on one side, a
	// nested object on the other.
	producer := mustContractShape(t, `{"out":{"meta":"string"}}`)
	consumer := mustContractShape(t, `{"out":{"meta":{"score":"float"}}}`)
	issues := CompareShapes(producer, consumer)
	if len(issues) != 1 || issues[0].Kind != IssueTypeMismatch || issues[0].Severity != SeverityCritical {
		t.Fatalf("string vs object = %v, want one critical type_mismatch", issues)
	}
}

func TestCompareShapesNamingVariant(t *testing.T) {
	producer := mustContractShape(t, `{"out":{"user_id":"uuid!","created_at":"timestamp"}}`)
	consumer := mustContractShape(t, `{"out":{"userId":"uuid!","createdAt":"timestamp"}}`)

	issues := CompareShapes(producer, consumer)

	var variants []ContractIssue
	for _, i := range issues {
		if i.Kind == IssueNamingVariant {
			variants = append(variants, i)
		}
	}
	if len(variants) != 2 {
		t.Fatalf("got %d naming variants in %v, want 2", len(variants), contractIssueKinds(issues))
	}
	for _, v := range variants {
		if v.Severity != SeverityLow {
			t.Errorf("naming variant %q severity = %v, want low", v.Path, v.Severity)
		}
		if v.Direction != DirectionOut {
			t.Errorf("naming variant %q direction = %q, want out", v.Path, v.Direction)
		}
	}
	if variants[0].Path != "created_at" || variants[0].Expected != "created_at" || variants[0].Actual != "createdAt" {
		t.Errorf("variant[0] = %+v, want created_at/createdAt", variants[0])
	}
	if variants[1].Path != "user_id" || variants[1].Expected != "user_id" || variants[1].Actual != "userId" {
		t.Errorf("variant[1] = %+v, want user_id/userId", variants[1])
	}

	// The required one is also a genuine missing out field: the SQL anti-join
	// reports both branches and so must this, or the two disagree.
	if kinds := contractIssueKinds(issues); len(kinds) != 3 {
		t.Fatalf("kinds = %v, want missing_out + 2 naming_variant", kinds)
	} else if kinds[0] != IssueMissingOut {
		t.Errorf("kinds = %v, want missing_out first", kinds)
	}

	// A shape that spells a field both ways does not accuse itself.
	self := mustContractShape(t, `{"out":{"user_id":"uuid!","userId":"uuid!"}}`)
	if got := CompareShapes(self, self); len(got) != 0 {
		t.Errorf("a shape compared with itself produced %v, want none", got)
	}
}

// The false-positive guard. A producer is allowed to return more than any one
// consumer reads, and a consumer is allowed to send more than the producer
// requires. Neither is a conflict, and reporting either is what makes an agent
// start ignoring this tool's output.
func TestCompareShapesIgnoresUnusedProducerFields(t *testing.T) {
	producer := mustContractShape(t, `{"in":{"email":"string!"},
	  "out":{"token":"string!","trace_id":"uuid!","debug":{"ms":"int!","host":"string"},"rows":[{"id":"uuid!"}]}}`)
	consumer := mustContractShape(t, `{"in":{"email":"string!","remember_me":"bool"},"out":{"token":"string!"}}`)

	if issues := CompareShapes(producer, consumer); len(issues) != 0 {
		t.Fatalf("got %d issues %+v, want none", len(issues), issues)
	}
}

func TestCompareShapesIdenticalShapes(t *testing.T) {
	raw := `{"in":{"email":"string!","password":"string!"},"out":{"token":"string!","user":{"id":"uuid!"}}}`
	s := mustContractShape(t, raw)
	if issues := CompareShapes(s, s); len(issues) != 0 {
		t.Fatalf("identical shapes produced %+v, want none", issues)
	}
}

// All four branches at once, to pin the reporting order the caller renders in.
func TestCompareShapesAllFourBranches(t *testing.T) {
	producer := mustContractShape(t, `{"in":{"email":"string!","tenant_id":"uuid!"},"out":{"token":"string!","count":"int"}}`)
	consumer := mustContractShape(t, `{"in":{"email":"string!","tenantId":"uuid!"},"out":{"token":"string!","count":"float","expires_at":"timestamp!"}}`)

	issues := CompareShapes(producer, consumer)
	want := []ContractIssueKind{IssueMissingOut, IssueMissingIn, IssueTypeMismatch, IssueNamingVariant}
	got := contractIssueKinds(issues)
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if issues[0].Path != "expires_at" || issues[1].Path != "tenant_id" || issues[2].Path != "count" || issues[3].Path != "tenant_id" {
		t.Errorf("paths = %q/%q/%q/%q", issues[0].Path, issues[1].Path, issues[2].Path, issues[3].Path)
	}
	if issues[2].Severity != SeverityHigh {
		t.Errorf("int vs float severity = %v, want high", issues[2].Severity)
	}
	if issues[3].Severity != SeverityLow {
		t.Errorf("naming variant severity = %v, want low", issues[3].Severity)
	}
}

func TestNormalizeContractKey(t *testing.T) {
	cases := []struct {
		kind, key, want string
	}{
		{"http", "POST /api/users/{userId}", "http:post /api/users/{}"},
		{"http", "post /api/users/:id/", "http:post /api/users/{}"},
		{"http", "POST /api/users/<id>", "http:post /api/users/{}"},
		{"http", "  POST   /api/users/{userId}  ", "http:post /api/users/{}"},
		{"http", "POST /api/users/42", "http:post /api/users/{}"},
		{"http", "POST /api/users/0B7E4C1A-9F3D-4E2B-8A1C-0D5F6E7A8B9C", "http:post /api/users/{}"},
		{"HTTP", "POST /api/users//{id}//", "http:post /api/users/{}"},
		{"http", "GET /api/users/{id}/posts/{postId}", "http:get /api/users/{}/posts/{}"},
		{"http", "GET https://localhost:3000/api/session?x=1", "http:get /api/session"},
		{"", "/api/session/", "/api/session"},
		{"", "/", "/"},
		{"event", "User.Created", "event:user.created"},
	}
	for _, c := range cases {
		if got := NormalizeContractKey(c.kind, c.key); got != c.want {
			t.Errorf("NormalizeContractKey(%q, %q) = %q, want %q", c.kind, c.key, got, c.want)
		}
	}

	// The property that actually matters: the three param spellings collide.
	a := NormalizeContractKey("http", "POST /api/users/{userId}")
	b := NormalizeContractKey("http", "post /api/users/:id/")
	c := NormalizeContractKey("http", "POST /API/Users/<id>")
	if a != b || b != c {
		t.Errorf("param spellings did not normalize together: %q %q %q", a, b, c)
	}

	// Kind scopes the key: an event and a route that share a name are not the
	// same contract.
	if NormalizeContractKey("http", "/foo") == NormalizeContractKey("event", "/foo") {
		t.Error("kind must scope the normalized key")
	}
}

func TestContractKeyDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"/api/session", "/api/sessions", 1},
		{"GET /api/session", "get /api/sessions/", 1},
		{"/api/session", "/api/session", 0},
		{"/api/users/{id}", "/api/users/:id/", 0},
		{"/api/login", "/api/logon", 1},
		{"/api/session", "/api/sessionsx", 2},
		{"/api/session", "/api/sessionsxy", 3},
		{"/api/session", "/api/sessionsxyz", 4},
		{"/api/session", "/completely/different/thing", 4},
		{"", "", 0},
	}
	for _, c := range cases {
		if got := ContractKeyDistance(c.a, c.b); got != c.want {
			t.Errorf("ContractKeyDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := ContractKeyDistance(c.b, c.a); got != c.want {
			t.Errorf("ContractKeyDistance(%q, %q) = %d, want %d (must be symmetric)", c.b, c.a, got, c.want)
		}
	}
}
