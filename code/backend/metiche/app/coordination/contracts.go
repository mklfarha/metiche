package coordination

// Contract shape handling: canonicalization, stable hashing, field flattening
// with direction, and the deterministic half of mismatch detection.
//
// Everything in this file is a pure function over values the caller already
// holds — no database, no HTTP, no clock. Two reasons, both load-bearing:
// detection runs synchronously inside the declaring call (see PLAN "Races"), so
// it may not do I/O; and the four-branch SQL anti-join over `contract_field`
// must agree verdict-for-verdict with CompareShapes below. Two implementations
// only stay honest if one of them is trivially testable.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ContractDirection is which way a field travels.
//
// This is not decoration. `in` is caller -> producer (request body, params);
// `out` is producer -> caller (response). A missing required *out* field breaks
// the consumer, a missing required *in* field breaks the producer, and a
// detector that loses the direction blames the wrong agent — which is worse
// than staying quiet, because the wrong agent then "fixes" a shape that was
// correct.
type ContractDirection string

const (
	DirectionIn  ContractDirection = "in"
	DirectionOut ContractDirection = "out"
)

// ContractType is the closed type vocabulary. Agents describe shapes in
// whatever dialect their language habits suggest; canonicalization maps all of
// it onto exactly these, plus `array<T>` built by ContractArrayOf. Anything
// unrecognized lands on json rather than erroring: a shape the server cannot
// fully understand is still worth comparing on the fields it does understand,
// and rejecting the whole submission would teach agents to stop publishing.
type ContractType string

const (
	ContractTypeString    ContractType = "string"
	ContractTypeInt       ContractType = "int"
	ContractTypeFloat     ContractType = "float"
	ContractTypeBool      ContractType = "bool"
	ContractTypeTimestamp ContractType = "timestamp"
	ContractTypeUUID      ContractType = "uuid"
	ContractTypeJSON      ContractType = "json"
	ContractTypeNull      ContractType = "null"
	ContractTypeObject    ContractType = "object"
)

// ContractArrayOf builds the array member of the vocabulary. There is no bare
// `array` type: an array whose element type is unknown is array<json>.
func ContractArrayOf(elem ContractType) ContractType {
	if elem == "" {
		elem = ContractTypeJSON
	}
	return ContractType("array<" + string(elem) + ">")
}

// ContractField is one leaf or container in a flattened shape.
type ContractField struct {
	// Path is the canonical dotted path, with `[]` marking an array hop:
	// `user.id`, `items[].user_id`.
	Path string
	// PathSnake is Path with every segment snake_cased. It exists so that
	// `userId` and `user_id` can be recognised as the same field under a
	// different serializer without being treated as the same field for type
	// checking.
	PathSnake string
	Type      ContractType
	Required  bool
	Nullable  bool
	Direction ContractDirection
}

// Shape is a parsed, flattened, sorted contract shape.
type Shape struct {
	// Fields is sorted by (Direction, Path) and carries at most one entry per
	// (Direction, Path) pair.
	Fields []ContractField

	// DirectionInferred records that the submitted shape carried no in/out
	// envelope and the whole body was read as `out`. Callers surface this in
	// the response note: the guess is usually right (agents paste response
	// bodies) but a silently mis-directed shape blames the wrong side forever.
	DirectionInferred bool

	// Note is a short human/LLM-facing remark about how the shape was read.
	Note string
}

// ContractIssueKind names one of the four anti-join branches.
type ContractIssueKind string

const (
	IssueMissingOut    ContractIssueKind = "missing_out"
	IssueMissingIn     ContractIssueKind = "missing_in"
	IssueTypeMismatch  ContractIssueKind = "type_mismatch"
	IssueNamingVariant ContractIssueKind = "naming_variant"
)

// ContractIssue is one deterministic mismatch finding.
//
// Note is written for an LLM to act on, not for a log: PLAN's noise rules say
// never surface a conflict without a suggested next action, and this is the
// only place that text can be produced from both shapes at once.
type ContractIssue struct {
	Kind      ContractIssueKind
	Path      string
	Expected  string
	Actual    string
	Direction ContractDirection
	Severity  Severity
	Note      string
}

// contractEnvelopeKeys maps the top-level wrapper names agents actually write
// onto a direction. Everything else at the top level is treated as payload.
var contractEnvelopeKeys = map[string]ContractDirection{
	"in":       DirectionIn,
	"input":    DirectionIn,
	"request":  DirectionIn,
	"req":      DirectionIn,
	"params":   DirectionIn,
	"body":     DirectionIn,
	"out":      DirectionOut,
	"output":   DirectionOut,
	"response": DirectionOut,
	"returns":  DirectionOut,
	"result":   DirectionOut,
}

// contractTypeAliases is the whole of the dialect tolerance. Keep it here
// rather than scattering strings.Contains checks through the walker.
var contractTypeAliases = map[string]ContractType{
	"string":    ContractTypeString,
	"str":       ContractTypeString,
	"text":      ContractTypeString,
	"varchar":   ContractTypeString,
	"char":      ContractTypeString,
	"email":     ContractTypeString,
	"url":       ContractTypeString,
	"uri":       ContractTypeString,
	"slug":      ContractTypeString,
	"int":       ContractTypeInt,
	"integer":   ContractTypeInt,
	"int32":     ContractTypeInt,
	"int64":     ContractTypeInt,
	"long":      ContractTypeInt,
	"number":    ContractTypeInt,
	"float":     ContractTypeFloat,
	"float32":   ContractTypeFloat,
	"float64":   ContractTypeFloat,
	"double":    ContractTypeFloat,
	"decimal":   ContractTypeFloat,
	"numeric":   ContractTypeFloat,
	"bool":      ContractTypeBool,
	"boolean":   ContractTypeBool,
	"timestamp": ContractTypeTimestamp,
	"datetime":  ContractTypeTimestamp,
	"date-time": ContractTypeTimestamp,
	"date":      ContractTypeTimestamp,
	"time":      ContractTypeTimestamp,
	"uuid":      ContractTypeUUID,
	"guid":      ContractTypeUUID,
	"json":      ContractTypeJSON,
	"jsonb":     ContractTypeJSON,
	"any":       ContractTypeJSON,
	"object":    ContractTypeObject,
	"struct":    ContractTypeObject,
	"map":       ContractTypeObject,
	"null":      ContractTypeNull,
	"nil":       ContractTypeNull,
	"none":      ContractTypeNull,
}

// contractDescriptorKeys are the keys that make an object a *description of a
// field* rather than a nested object of fields. An object only counts as a
// descriptor if it has a string `type` AND every other key is in this set —
// without that second condition a real payload like
// {"type":"string","payload":"json"} would be eaten as metadata.
var contractDescriptorKeys = map[string]bool{
	"type": true, "required": true, "optional": true, "nullable": true,
	"description": true, "desc": true, "doc": true, "comment": true,
	"example": true, "examples": true, "default": true, "format": true,
	"title": true, "enum": true, "items": true, "properties": true, "fields": true,
}

// ParseShape flattens a submitted shape into canonical fields.
//
// Only a structurally unusable submission errors (not JSON, or not a JSON
// object). Unknown *types* never error — see ContractType.
func ParseShape(raw json.RawMessage) (Shape, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Shape{}, errors.New("contract shape is empty")
	}

	var root any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// UseNumber keeps 1 and 1.0 distinguishable, so an example-value shape
	// ({"count": 1}) infers int instead of float for every number.
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return Shape{}, fmt.Errorf("contract shape is not valid JSON: %w", err)
	}

	obj, ok := root.(map[string]any)
	if !ok {
		return Shape{}, fmt.Errorf("contract shape must be a JSON object, got %T", root)
	}

	acc := &contractAccum{seen: map[string]bool{}}
	var envelopes, loose []string
	for k := range obj {
		if _, isEnv := contractEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))]; isEnv {
			envelopes = append(envelopes, k)
		} else {
			loose = append(loose, k)
		}
	}
	sort.Strings(envelopes)
	sort.Strings(loose)

	shape := Shape{}
	for _, k := range envelopes {
		dir := contractEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))]
		contractWalkEnvelope(obj[k], dir, acc)
	}
	if len(envelopes) == 0 {
		// No envelope at all: agents overwhelmingly paste a response body. Out
		// is the safe default because a wrongly-out field produces a
		// consumer-side complaint the consumer can see and correct, whereas a
		// wrongly-in field makes the producer chase a request field nobody sends.
		shape.DirectionInferred = true
		shape.Note = "shape had no in/out envelope; whole body read as `out` (wrap it in {\"in\":…,\"out\":…} to be explicit)"
		contractWalkEnvelope(obj, DirectionOut, acc)
	} else {
		// Keys sitting beside the envelopes are payload, not metadata: dropping
		// them would silently shrink the contract.
		for _, k := range loose {
			name, req, nul := contractSplitKeyMarkers(k)
			contractWalkValue(name, obj[k], DirectionOut, req, nul, acc)
		}
	}

	shape.Fields = acc.fields
	contractSortFields(shape.Fields)
	return shape, nil
}

// contractAccum collects fields and keeps the first definition of any
// (direction, path). Two envelopes can map to the same direction
// ({"body":…,"params":…}); first-wins keeps that deterministic.
type contractAccum struct {
	fields []ContractField
	seen   map[string]bool
}

func (a *contractAccum) emit(path string, dir ContractDirection, t ContractType, required, nullable bool) {
	if path == "" {
		return
	}
	k := string(dir) + "\x00" + path
	if a.seen[k] {
		return
	}
	a.seen[k] = true
	a.fields = append(a.fields, ContractField{
		Path:      path,
		PathSnake: contractSnakePath(path),
		Type:      t,
		Required:  required,
		Nullable:  nullable,
		Direction: dir,
	})
}

func contractSortFields(f []ContractField) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].Direction != f[j].Direction {
			return f[i].Direction < f[j].Direction
		}
		return f[i].Path < f[j].Path
	})
}

// contractWalkEnvelope walks the contents of one direction envelope. The
// envelope itself never becomes a field.
func contractWalkEnvelope(node any, dir ContractDirection, acc *contractAccum) {
	switch v := node.(type) {
	case map[string]any:
		if d, ok := contractAsDescriptor(v); ok {
			// {"out": {"type":"array","items":{...}}} — a whole-body descriptor.
			contractWalkDescriptor("body", d, dir, false, false, acc)
			return
		}
		for _, k := range contractSortedKeys(v) {
			name, req, nul := contractSplitKeyMarkers(k)
			contractWalkValue(name, v[k], dir, req, nul, acc)
		}
	case []any:
		// A bare list body: {"out": [{"id":"uuid!"}]}.
		contractWalkValue("items", v, dir, false, false, acc)
	default:
		contractWalkValue("body", node, dir, false, false, acc)
	}
}

func contractWalkValue(path string, v any, dir ContractDirection, req, nul bool, acc *contractAccum) {
	switch t := v.(type) {
	case string:
		ty, sreq, snul := contractParseTypeSpec(t)
		acc.emit(path, dir, ty, req || sreq, nul || snul)

	case map[string]any:
		if d, ok := contractAsDescriptor(t); ok {
			contractWalkDescriptor(path, d, dir, req, nul, acc)
			return
		}
		acc.emit(path, dir, ContractTypeObject, req, nul)
		for _, k := range contractSortedKeys(t) {
			name, kreq, knul := contractSplitKeyMarkers(k)
			contractWalkValue(contractJoinPath(path, name), t[k], dir, kreq, knul, acc)
		}

	case []any:
		contractWalkArray(path, t, dir, req, nul, acc)

	case bool:
		acc.emit(path, dir, ContractTypeBool, req, nul)

	case json.Number:
		acc.emit(path, dir, contractNumberType(t.String()), req, nul)

	case float64:
		// Only reachable if a caller hands us a pre-decoded tree.
		acc.emit(path, dir, ContractTypeFloat, req, nul)

	case nil:
		acc.emit(path, dir, ContractTypeNull, req, true)

	default:
		acc.emit(path, dir, ContractTypeJSON, req, nul)
	}
}

// contractWalkArray handles both notations for a list: a single-element sample
// list, and (via the descriptor path) type:array + items.
func contractWalkArray(path string, list []any, dir ContractDirection, req, nul bool, acc *contractAccum) {
	if len(list) == 0 {
		acc.emit(path, dir, ContractArrayOf(ContractTypeJSON), req, nul)
		return
	}
	contractWalkElement(path, list[0], dir, req, nul, acc)
}

// contractWalkElement emits the array field at path and, when the element is an
// object, its children under `path[].child`.
func contractWalkElement(path string, elem any, dir ContractDirection, req, nul bool, acc *contractAccum) {
	switch e := elem.(type) {
	case map[string]any:
		if d, ok := contractAsDescriptor(e); ok {
			// {"items": {"type":"object","properties":{…}}} or a scalar descriptor.
			ety, _, _ := contractParseTypeSpec(contractDescriptorTypeSpec(d))
			acc.emit(path, dir, ContractArrayOf(contractBaseOfArray(ety)), req, nul)
			if props := contractDescriptorProps(d); props != nil {
				for _, k := range contractSortedKeys(props) {
					name, kreq, knul := contractSplitKeyMarkers(k)
					contractWalkValue(path+"[]."+name, props[k], dir, kreq, knul, acc)
				}
			}
			return
		}
		acc.emit(path, dir, ContractArrayOf(ContractTypeObject), req, nul)
		for _, k := range contractSortedKeys(e) {
			name, kreq, knul := contractSplitKeyMarkers(k)
			contractWalkValue(path+"[]."+name, e[k], dir, kreq, knul, acc)
		}
	case string:
		ety, _, _ := contractParseTypeSpec(e)
		acc.emit(path, dir, ContractArrayOf(contractBaseOfArray(ety)), req, nul)
	case bool:
		acc.emit(path, dir, ContractArrayOf(ContractTypeBool), req, nul)
	case json.Number:
		acc.emit(path, dir, ContractArrayOf(contractNumberType(e.String())), req, nul)
	default:
		// Nested lists and nulls collapse: array<array<…>> is not in the
		// vocabulary and nobody compares on it usefully.
		acc.emit(path, dir, ContractArrayOf(ContractTypeJSON), req, nul)
	}
}

// contractBaseOfArray unwraps one array level so array<array<x>> cannot be
// built; the vocabulary has no nested array member.
func contractBaseOfArray(t ContractType) ContractType {
	if strings.HasPrefix(string(t), "array<") {
		return ContractTypeJSON
	}
	return t
}

func contractAsDescriptor(m map[string]any) (map[string]any, bool) {
	spec, ok := m["type"].(string)
	if !ok || strings.TrimSpace(spec) == "" {
		return nil, false
	}
	for k := range m {
		if !contractDescriptorKeys[strings.ToLower(strings.TrimSpace(k))] {
			return nil, false
		}
	}
	return m, true
}

func contractDescriptorTypeSpec(d map[string]any) string {
	s, _ := d["type"].(string)
	return s
}

func contractDescriptorProps(d map[string]any) map[string]any {
	for _, k := range []string{"properties", "fields"} {
		if m, ok := d[k].(map[string]any); ok {
			return m
		}
	}
	return nil
}

// contractWalkDescriptor reads a JSON-Schema-ish field description. Everything
// in it except type/required/nullable/items/properties is stripped here — this
// is the single point where descriptions and examples leave the pipeline, which
// is what makes CanonicalBytes identical for a documented and an undocumented
// submission of the same shape.
func contractWalkDescriptor(path string, d map[string]any, dir ContractDirection, req, nul bool, acc *contractAccum) {
	ty, sreq, snul := contractParseTypeSpec(contractDescriptorTypeSpec(d))
	req = req || sreq
	nul = nul || snul
	if b, ok := d["required"].(bool); ok {
		req = b
	}
	if b, ok := d["optional"].(bool); ok && b {
		req = false
	}
	if b, ok := d["nullable"].(bool); ok {
		nul = b
	}

	items, hasItems := d["items"]
	isArray := strings.HasPrefix(string(ty), "array<")
	if isArray || hasItems {
		if hasItems {
			contractWalkElement(path, items, dir, req, nul, acc)
			return
		}
		acc.emit(path, dir, ContractArrayOf(ContractTypeJSON), req, nul)
		return
	}

	props := contractDescriptorProps(d)
	if props != nil {
		acc.emit(path, dir, ContractTypeObject, req, nul)
		for _, k := range contractSortedKeys(props) {
			name, kreq, knul := contractSplitKeyMarkers(k)
			contractWalkValue(contractJoinPath(path, name), props[k], dir, kreq, knul, acc)
		}
		return
	}
	acc.emit(path, dir, ty, req, nul)
}

// contractParseTypeSpec turns one written type into the closed vocabulary.
// Returns the type plus the required/nullable markers carried by the spelling
// ("string!" is required, "string?" is nullable).
func contractParseTypeSpec(spec string) (ContractType, bool, bool) {
	s := strings.ToLower(strings.TrimSpace(spec))
	req, nul := false, false
	arr := 0

	// Markers can be written in any order and stacked: "string[]!", "uuid!?".
strip:
	for {
		switch {
		case strings.HasSuffix(s, "!"):
			req = true
			s = s[:len(s)-1]
		case strings.HasSuffix(s, "?"):
			nul = true
			s = s[:len(s)-1]
		case strings.HasSuffix(s, "*"):
			req = true
			s = s[:len(s)-1]
		case strings.HasSuffix(s, "[]"):
			arr++
			s = s[:len(s)-2]
		default:
			break strip
		}
		s = strings.TrimSpace(s)
	}

	// "string | null" is how a lot of people write nullable.
	if strings.Contains(s, "|") {
		parts := strings.Split(s, "|")
		s = ""
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if contractTypeAliases[p] == ContractTypeNull {
				nul = true
				continue
			}
			if s == "" {
				s = p
			}
		}
	}

	if strings.HasPrefix(s, "[]") {
		arr++
		s = strings.TrimSpace(s[2:])
	}
	for _, kw := range []string{"array", "list", "slice", "set"} {
		if strings.HasPrefix(s, kw) {
			rest := strings.TrimSpace(s[len(kw):])
			if strings.HasPrefix(rest, "<") && strings.HasSuffix(rest, ">") {
				arr++
				s = strings.TrimSpace(rest[1 : len(rest)-1])
				break
			}
			if rest == "" {
				arr++
				s = ""
				break
			}
		}
	}

	base, known := contractTypeAliases[s]
	if !known {
		// Unknown type, not an error: json keeps the field comparable at all.
		base = ContractTypeJSON
	}
	if s == "" {
		base = ContractTypeJSON
	}
	if arr > 0 {
		if arr > 1 {
			base = ContractTypeJSON
		}
		return ContractArrayOf(base), req, nul
	}
	if base == ContractTypeNull {
		nul = true
	}
	return base, req, nul
}

func contractNumberType(lit string) ContractType {
	if strings.ContainsAny(lit, ".eE") {
		return ContractTypeFloat
	}
	return ContractTypeInt
}

// contractSplitKeyMarkers pulls required/nullable markers off a key name, so
// {"email!": "string"} means the same as {"email": "string!"}.
func contractSplitKeyMarkers(k string) (name string, req, nul bool) {
	name = strings.TrimSpace(k)
	for {
		switch {
		case strings.HasSuffix(name, "!"), strings.HasSuffix(name, "*"):
			req = true
			name = name[:len(name)-1]
		case strings.HasSuffix(name, "?"):
			nul = true
			name = name[:len(name)-1]
		default:
			return strings.TrimSpace(name), req, nul
		}
	}
}

func contractSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contractJoinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// contractSnakePath snake_cases each segment, preserving the `[]` array marker.
func contractSnakePath(path string) string {
	segs := strings.Split(path, ".")
	for i, s := range segs {
		suffix := ""
		if strings.HasSuffix(s, "[]") {
			suffix = "[]"
			s = s[:len(s)-2]
		}
		segs[i] = contractSnake(s) + suffix
	}
	return strings.Join(segs, ".")
}

func contractSnake(s string) string {
	runes := []rune(s)
	out := make([]rune, 0, len(runes)+4)
	for i, r := range runes {
		switch {
		case r == '-' || r == '_' || r == ' ' || r == '/':
			if len(out) > 0 && out[len(out)-1] != '_' {
				out = append(out, '_')
			}
		case unicode.IsUpper(r):
			prevLower := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			prevUpper := i > 0 && unicode.IsUpper(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			// Split userId -> user_id and HTTPServer -> http_server, but never
			// UserID -> user_i_d.
			if len(out) > 0 && out[len(out)-1] != '_' && (prevLower || (prevUpper && nextLower)) {
				out = append(out, '_')
			}
			out = append(out, unicode.ToLower(r))
		default:
			out = append(out, unicode.ToLower(r))
		}
	}
	return strings.Trim(string(out), "_")
}

// contractCanonField is the on-the-wire canonical form. Field order in this
// struct IS the byte order of the output, which is exactly why a struct is used
// and not a map: encoding/json emits struct fields in declaration order but
// gives no ordering guarantee for maps, and a hash that depends on Go's map
// iteration order would make dedupe collapse at random.
type contractCanonField struct {
	D string `json:"d"`
	P string `json:"p"`
	T string `json:"t"`
	R bool   `json:"r"`
	N bool   `json:"n"`
}

// CanonicalBytes is the deterministic serialization a shape hash is taken over.
// Sorted by (Direction, Path); descriptions, examples and every other piece of
// prose are already gone by this point; no insignificant whitespace.
func CanonicalBytes(s Shape) []byte {
	fields := make([]ContractField, len(s.Fields))
	copy(fields, s.Fields)
	contractSortFields(fields)

	rows := make([]contractCanonField, 0, len(fields))
	for _, f := range fields {
		rows = append(rows, contractCanonField{
			D: string(f.Direction),
			P: f.Path,
			T: string(f.Type),
			R: f.Required,
			N: f.Nullable,
		})
	}
	// A []struct marshals deterministically; the only error path here is an
	// unmarshalable value, which these five scalar fields cannot produce.
	b, err := json.Marshal(rows)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// ShapeHash is the dedupe key: sha256 of CanonicalBytes, hex.
//
// Agents never compute this. Two agents describing the same endpoint in
// different dialects must land on the same hash or COUNT(DISTINCT shape_hash)
// reports a mismatch on every single contract.
func ShapeHash(s Shape) string {
	sum := sha256.Sum256(CanonicalBytes(s))
	return hex.EncodeToString(sum[:])
}

type contractFingerprintRow struct {
	P string `json:"p"`
	T string `json:"t"`
}

// ShapeFingerprint is the coarse hash: paths and types only, with Required,
// Nullable and Direction dropped entirely. It answers "is this the same set of
// fields?" for a shape that was published once as a request body and once as a
// response, or where one side marked optional what the other marked required —
// differences worth an issue, but not worth calling it a different contract.
func ShapeFingerprint(s Shape) string {
	seen := map[string]bool{}
	rows := make([]contractFingerprintRow, 0, len(s.Fields))
	for _, f := range s.Fields {
		k := f.Path + "\x00" + string(f.Type)
		if seen[k] {
			continue
		}
		seen[k] = true
		rows = append(rows, contractFingerprintRow{P: f.Path, T: string(f.Type)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].P != rows[j].P {
			return rows[i].P < rows[j].P
		}
		return rows[i].T < rows[j].T
	})
	b, err := json.Marshal(rows)
	if err != nil {
		b = []byte("[]")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// contractTypeFamily groups types that a runtime might coerce between. Within a
// family a mismatch is a bug to fix; across families it is a shape that cannot
// possibly deserialize, which is why the crossing is what escalates severity.
func contractTypeFamily(t ContractType) string {
	if strings.HasPrefix(string(t), "array<") {
		return "structural"
	}
	switch t {
	case ContractTypeInt, ContractTypeFloat:
		return "numeric"
	case ContractTypeString, ContractTypeUUID, ContractTypeTimestamp:
		return "textual"
	case ContractTypeObject, ContractTypeJSON:
		return "structural"
	case ContractTypeBool:
		return "boolean"
	case ContractTypeNull:
		return "null"
	}
	return "structural"
}

// CompareShapes is the in-process mirror of the four-branch directional
// anti-join in PLAN "Contract mismatch". It runs for the immediate two-shape
// case inside the declaring call; the SQL runs for cross-session queries. They
// must agree, so the branches are implemented one-for-one and in the same
// order.
//
// What is deliberately NOT a branch: fields the producer returns that the
// consumer never asked for. A producer is allowed to return more than any one
// consumer reads — flagging that is the single largest false-positive source in
// a system like this, and a detector that cries wolf gets ignored.
func CompareShapes(producer, consumer Shape) []ContractIssue {
	prod := contractIndex(producer)
	cons := contractIndex(consumer)
	var issues []ContractIssue

	// 1. Required `out` fields the consumer needs and the producer omits.
	for _, cf := range consumer.Fields {
		if cf.Direction != DirectionOut || !cf.Required {
			continue
		}
		if _, ok := prod[contractKeyOf(cf)]; ok {
			continue
		}
		issues = append(issues, ContractIssue{
			Kind:      IssueMissingOut,
			Path:      cf.Path,
			Expected:  string(cf.Type),
			Actual:    "",
			Direction: DirectionOut,
			Severity:  SeverityHigh,
			Note: fmt.Sprintf("consumer requires out field `%s` (%s) that the producer does not return; "+
				"add it to the producer response or drop the requirement on the consumer", cf.Path, cf.Type),
		})
	}

	// 2. Required `in` fields the producer needs and the consumer omits.
	for _, pf := range producer.Fields {
		if pf.Direction != DirectionIn || !pf.Required {
			continue
		}
		if _, ok := cons[contractKeyOf(pf)]; ok {
			continue
		}
		issues = append(issues, ContractIssue{
			Kind:      IssueMissingIn,
			Path:      pf.Path,
			Expected:  string(pf.Type),
			Actual:    "",
			Direction: DirectionIn,
			Severity:  SeverityHigh,
			Note: fmt.Sprintf("producer requires in field `%s` (%s) that the consumer does not send; "+
				"add it to the request or make it optional on the producer", pf.Path, pf.Type),
		})
	}

	// 3. Same path, same direction, different type.
	for _, pf := range producer.Fields {
		cf, ok := cons[contractKeyOf(pf)]
		if !ok || cf.Type == pf.Type {
			continue
		}
		sev := SeverityHigh
		cross := contractTypeFamily(pf.Type) != contractTypeFamily(cf.Type)
		note := fmt.Sprintf("%s field `%s`: producer says %s, consumer says %s; agree on one type",
			pf.Direction, pf.Path, pf.Type, cf.Type)
		if cross {
			// Different families cannot be coerced by any serializer, so this
			// is a guaranteed runtime failure rather than a rounding argument.
			sev = SeverityCritical
			note = fmt.Sprintf("%s field `%s`: producer says %s (%s), consumer says %s (%s) — different type families, "+
				"this cannot deserialize; one side is wrong about the shape, not just the precision",
				pf.Direction, pf.Path, pf.Type, contractTypeFamily(pf.Type), cf.Type, contractTypeFamily(cf.Type))
		}
		issues = append(issues, ContractIssue{
			Kind:      IssueTypeMismatch,
			Path:      pf.Path,
			Expected:  string(pf.Type),
			Actual:    string(cf.Type),
			Direction: pf.Direction,
			Severity:  sev,
			Note:      note,
		})
	}

	// 4. Same snake path, different literal path.
	issues = append(issues, contractNamingVariants(producer, consumer)...)

	contractSortIssues(issues)
	return issues
}

// contractNamingVariants finds fields that are the same field wearing a
// different serializer's casing. Only reported when neither side carries the
// other's literal spelling, so a shape that lists both `user_id` and `userId`
// does not accuse itself.
func contractNamingVariants(producer, consumer Shape) []ContractIssue {
	prodPaths := map[string]bool{}
	for _, f := range producer.Fields {
		prodPaths[contractKeyOf(f)] = true
	}
	consPaths := map[string]bool{}
	for _, f := range consumer.Fields {
		consPaths[contractKeyOf(f)] = true
	}

	type variantKey struct {
		dir   ContractDirection
		snake string
	}
	prodBySnake := map[variantKey][]string{}
	for _, f := range producer.Fields {
		if consPaths[contractKeyOf(f)] {
			continue
		}
		k := variantKey{f.Direction, f.PathSnake}
		prodBySnake[k] = append(prodBySnake[k], f.Path)
	}

	var issues []ContractIssue
	emitted := map[variantKey]bool{}
	for _, cf := range consumer.Fields {
		if prodPaths[contractKeyOf(cf)] {
			continue
		}
		k := variantKey{cf.Direction, cf.PathSnake}
		if emitted[k] {
			continue
		}
		candidates := prodBySnake[k]
		if len(candidates) == 0 {
			continue
		}
		sort.Strings(candidates)
		producerPath := candidates[0]
		if producerPath == cf.Path {
			continue
		}
		emitted[k] = true
		issues = append(issues, ContractIssue{
			Kind:      IssueNamingVariant,
			Path:      cf.PathSnake,
			Expected:  producerPath,
			Actual:    cf.Path,
			Direction: cf.Direction,
			Severity:  SeverityLow,
			Note: fmt.Sprintf("%s field is `%s` on the producer and `%s` on the consumer; same field, different casing — "+
				"usually a serializer setting, not a real mismatch (it will also show as a missing field)",
				cf.Direction, producerPath, cf.Path),
		})
	}
	return issues
}

func contractIndex(s Shape) map[string]ContractField {
	m := make(map[string]ContractField, len(s.Fields))
	for _, f := range s.Fields {
		m[contractKeyOf(f)] = f
	}
	return m
}

func contractKeyOf(f ContractField) string {
	return string(f.Direction) + "\x00" + f.Path
}

var contractIssueRank = map[ContractIssueKind]int{
	IssueMissingOut:    0,
	IssueMissingIn:     1,
	IssueTypeMismatch:  2,
	IssueNamingVariant: 3,
}

func contractSortIssues(issues []ContractIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		if contractIssueRank[issues[i].Kind] != contractIssueRank[issues[j].Kind] {
			return contractIssueRank[issues[i].Kind] < contractIssueRank[issues[j].Kind]
		}
		if issues[i].Direction != issues[j].Direction {
			return issues[i].Direction < issues[j].Direction
		}
		return issues[i].Path < issues[j].Path
	})
}

// NormalizeContractKey turns a written contract key into the form stored in
// `key_norm`. `POST /api/users/{userId}` and `post /api/users/:id/` must land on
// the same string or the producer and the consumer of one endpoint never meet.
//
// kind ("http", "event", "graphql", …) is prefixed so that an event named
// `/foo` and an HTTP path `/foo` are not the same contract.
func NormalizeContractKey(kind, key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.Join(strings.Fields(k), " ")

	verb := ""
	if i := strings.Index(k, " "); i >= 0 {
		verb, k = k[:i]+" ", k[i+1:]
	}

	// Drop a scheme+host if someone pasted a whole URL; the contract is the
	// path, and localhost:3000 vs the deployed host must not fork it.
	if i := strings.Index(k, "://"); i >= 0 {
		rest := k[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			k = rest[j:]
		} else {
			k = "/"
		}
	}
	if i := strings.IndexAny(k, "?#"); i >= 0 {
		k = k[:i]
	}

	segs := strings.Split(k, "/")
	outSegs := make([]string, 0, len(segs))
	for i, s := range segs {
		if s == "" {
			// Keep the leading empty segment so a rooted path stays rooted;
			// drop doubled and trailing separators.
			if i == 0 {
				outSegs = append(outSegs, "")
			}
			continue
		}
		outSegs = append(outSegs, contractNormalizeSegment(s))
	}
	norm := strings.Join(outSegs, "/")
	if norm == "" && strings.HasPrefix(k, "/") {
		norm = "/"
	}

	norm = strings.TrimSpace(verb + norm)
	if kind = strings.ToLower(strings.TrimSpace(kind)); kind != "" {
		return kind + ":" + norm
	}
	return norm
}

// contractNormalizeSegment collapses anything that is a value rather than part
// of the route. All three param spellings plus concrete ids: an agent that
// pasted the URL it actually called must not fork the contract away from the
// agent that wrote the route.
func contractNormalizeSegment(s string) string {
	switch {
	case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
		return "{}"
	case strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"):
		return "{}"
	case strings.HasPrefix(s, ":") && len(s) > 1:
		return "{}"
	case strings.HasPrefix(s, "$") && len(s) > 1:
		return "{}"
	case s == "*" || s == "**":
		return "{}"
	case contractIsAllDigits(s), contractLooksLikeUUID(s):
		return "{}"
	}
	return s
}

func contractIsAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func contractLooksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// contractMaxKeyDistance is the point past which "did you mean?" stops being
// useful. Beyond it the answer is only ever "these are different endpoints", so
// the matrix is abandoned early and 4 means "far".
const contractMaxKeyDistance = 3

// ContractKeyDistance is Levenshtein over normalized keys, capped: any distance
// above contractMaxKeyDistance returns contractMaxKeyDistance+1. This is what
// catches `/api/session` vs `/api/sessions`, and it runs against every other
// live contract key, so the cap is what keeps it cheap.
func ContractKeyDistance(a, b string) int {
	x := NormalizeContractKey("", a)
	y := NormalizeContractKey("", b)
	if x == y {
		return 0
	}
	far := contractMaxKeyDistance + 1
	if len(x)-len(y) > contractMaxKeyDistance || len(y)-len(x) > contractMaxKeyDistance {
		return far
	}

	prev := make([]int, len(y)+1)
	cur := make([]int, len(y)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(x); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if cur[j] < best {
				best = cur[j]
			}
		}
		if best > contractMaxKeyDistance {
			return far
		}
		prev, cur = cur, prev
	}
	if d := prev[len(y)]; d <= contractMaxKeyDistance {
		return d
	}
	return far
}

// ShapeFromCanonical reads back what CanonicalBytes wrote.
//
// It is how a stored assertion is compared again later: contract_assertion.shape
// holds the canonical field list, and a detector or a settle check that runs
// minutes after the publish rebuilds the Shape from it rather than from the
// agent's original submission, which is never stored. The round trip is exact
// in the only sense that matters: ShapeHash(ShapeFromCanonical(CanonicalBytes(s)))
// equals ShapeHash(s). MySQL re-serializes a JSON column (key order, spacing),
// which is why this parses rather than hashing the stored bytes.
func ShapeFromCanonical(raw []byte) (Shape, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Shape{}, errors.New("stored contract shape is empty")
	}
	var rows []contractCanonField
	if err := json.Unmarshal(raw, &rows); err != nil {
		return Shape{}, fmt.Errorf("stored contract shape is not a canonical field list: %w", err)
	}
	s := Shape{Fields: make([]ContractField, 0, len(rows))}
	seen := map[string]bool{}
	for _, r := range rows {
		dir := ContractDirection(r.D)
		if (dir != DirectionIn && dir != DirectionOut) || r.P == "" {
			return Shape{}, fmt.Errorf("stored contract shape has a malformed field (direction %q, path %q)", r.D, r.P)
		}
		k := r.D + "\x00" + r.P
		if seen[k] {
			continue
		}
		seen[k] = true
		s.Fields = append(s.Fields, ContractField{
			Path:      r.P,
			PathSnake: contractSnakePath(r.P),
			Type:      ContractType(r.T),
			Required:  r.R,
			Nullable:  r.N,
			Direction: dir,
		})
	}
	contractSortFields(s.Fields)
	return s, nil
}
