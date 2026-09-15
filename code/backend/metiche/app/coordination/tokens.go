package coordination

// Tokens: the words signal between a plan and a recorded decision.
//
// A decision is a candidate for a plan when they share at least two of these
// tokens (docs/DECISIONS.md §3.1, §3.2). The server never decides whether the
// plan contradicts the decision; the tokens only pick which pairs the plan's
// own agent is asked to judge, so the tokenizer errs towards recall: it splits
// identifiers the way a programmer reads them, keeps the short technical words
// (jwt, api, ui, db) that carry most of a decision's meaning, and drops only
// the words that carry none.
//
// Pure and deterministic: the same text always yields the same tokens in the
// same order, because the tokens are stored (decision_token) and compared
// against tokens computed later from other text.

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenMaxBytes is decision_token.token, VARCHAR(32). A longer word is cut at
// a rune boundary rather than dropped, so a long identifier still matches
// itself.
const TokenMaxBytes = 32

// Tokenize splits text into at most max lowercase tokens, first-seen order,
// without duplicates.
//
//   - Words split on anything that is not a letter or a digit, so snake_case,
//     kebab-case and path/segments come apart.
//   - camelCase and PascalCase split at the case change ("localStorage" is
//     local, storage; "HTTPServer" is http, server). Digits stay with their
//     letters ("oauth2").
//   - Single characters, bare numbers and stopwords are dropped.
//   - A trailing plural "s" is folded ("tokens" and "token" are one token),
//     except after s, u, i or a ("class", "status", "redis", "alias").
func Tokenize(text string, max int) []string {
	if max <= 0 || strings.TrimSpace(text) == "" {
		return nil
	}
	out := make([]string, 0, min(max, 16))
	seen := make(map[string]bool, 16)
	for _, word := range tokenWords(text) {
		lower := strings.ToLower(word)
		if tokenStopwords[lower] {
			continue
		}
		tok := tokenClip(tokenFoldPlural(lower))
		if !tokenKeep(tok) || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
		if len(out) >= max {
			break
		}
	}
	return out
}

// tokenWords splits on non-alphanumerics, then on case changes inside a word.
func tokenWords(text string) []string {
	var words []string
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		words = append(words, splitCamel(field)...)
	}
	return words
}

// splitCamel cuts "sessionStorage" into session, Storage and "JSONParser" into
// JSON, Parser. A run of capitals followed by a lowercase letter gives its last
// capital to the next word.
func splitCamel(word string) []string {
	runes := []rune(word)
	if len(runes) < 2 {
		return []string{word}
	}
	var parts []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := false
		switch {
		case unicode.IsLower(prev) && unicode.IsUpper(cur):
			boundary = true
		case unicode.IsDigit(prev) && unicode.IsUpper(cur):
			boundary = true
		case unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]):
			boundary = true
		}
		if boundary {
			parts = append(parts, string(runes[start:i]))
			start = i
		}
	}
	return append(parts, string(runes[start:]))
}

func tokenFoldPlural(tok string) string {
	if utf8.RuneCountInString(tok) < 4 || !strings.HasSuffix(tok, "s") {
		return tok
	}
	switch tok[len(tok)-2] {
	case 's', 'u', 'i', 'a':
		return tok
	}
	folded := tok[:len(tok)-1]
	if tokenStopwords[folded] {
		return tok
	}
	return folded
}

func tokenClip(tok string) string {
	if len(tok) <= TokenMaxBytes {
		return tok
	}
	cut := TokenMaxBytes
	for cut > 0 && !utf8.RuneStart(tok[cut]) {
		cut--
	}
	return tok[:cut]
}

func tokenKeep(tok string) bool {
	if utf8.RuneCountInString(tok) < 2 || tokenStopwords[tok] {
		return false
	}
	for _, r := range tok {
		if !unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// tokenStopwords carry no topic. Deliberately short: a plan and a decision
// sharing "auth" and "cookie" is the signal, and a stopword list that grew into
// the domain vocabulary would hide it. "never", "always" and "only" are here
// because every decision says them and they name nothing.
var tokenStopwords = func() map[string]bool {
	words := strings.Fields(`
		a about above after again against all also always am an and any are as at
		be because been before being below between both but by
		can could did do does doing done down during each either else every
		few for from further had has have having he her here hers him his how
		if in instead into is it its itself just let may me might more most must my
		neither never no nor not now of off on once one only or other our ours out over own
		per same shall she should so some such than that the their theirs them then there
		these they this those through to too two under until up upon us
		very via was we were what when where whether which while who whom why will with
		within without would yet you your yours
		use uses using used make makes making add adds adding new
	`)
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()
