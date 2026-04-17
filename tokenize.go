package main

import (
	"strings"
	"unicode"
)

// English stopwords taken from bm25s' default list so search behaviour stays
// consistent with the reference Python implementation.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {},
	"but": {}, "by": {}, "for": {}, "if": {}, "in": {}, "into": {}, "is": {},
	"it": {}, "no": {}, "not": {}, "of": {}, "on": {}, "or": {}, "such": {},
	"that": {}, "the": {}, "their": {}, "then": {}, "there": {}, "these": {},
	"they": {}, "this": {}, "to": {}, "was": {}, "were": {}, "will": {},
	"with": {}, "i": {}, "you": {}, "he": {}, "she": {}, "we": {}, "them": {},
	"his": {}, "her": {}, "our": {}, "its": {}, "do": {}, "does": {},
	"did": {}, "have": {}, "has": {}, "had": {}, "can": {}, "could": {},
	"should": {}, "would": {}, "may": {}, "might": {},
}

// tokenize lowercases, splits on non-alphanumeric runes, and drops stopwords
// and very short tokens. The same function is used at index and query time so
// behaviour stays symmetric.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	tokens := make([]string, 0, 32)
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		tok := b.String()
		b.Reset()
		if len(tok) < 2 {
			return
		}
		if _, stop := stopwords[tok]; stop {
			return
		}
		tokens = append(tokens, tok)
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return tokens
}
