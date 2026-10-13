package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRecallPreferences caps how many preference memories Recall adds on top
// of the relevance hits.
const MaxRecallPreferences = 20

// maxRecallTerms bounds the number of words of a message used as a query.
const maxRecallTerms = 32

// Recall returns the memories the agent should see for a message: up to max
// memories ranked by FTS relevance to query (any word may match), plus the
// user's most recent preference memories (up to MaxRecallPreferences), with
// duplicates removed. Relevance hits come first.
func (s *Store) Recall(ctx context.Context, userID int64, query string, max int) ([]Memory, error) {
	if max <= 0 {
		return nil, nil
	}
	var out []Memory
	seen := map[int64]bool{}
	add := func(ms []Memory) {
		for _, m := range ms {
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
	}
	if terms := recallTerms(query); len(terms) > 0 {
		hits, err := s.search(ctx, userID, "", terms, " OR ", max, 0)
		if err != nil {
			return nil, fmt.Errorf("memory: recall: %w", err)
		}
		add(hits)
	}
	prefs, err := s.List(ctx, userID, KindPreference, MaxRecallPreferences, 0)
	if err != nil {
		return nil, fmt.Errorf("memory: recall: %w", err)
	}
	add(prefs)
	return out, nil
}

// recallTerms splits a natural-language message into distinct lower-case
// words worth searching for: stopwords and one- or two-letter words are
// dropped.
func recallTerms(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var out []string
	seen := map[string]bool{}
	for _, w := range words {
		if utf8.RuneCountInString(w) < 3 || stopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) == maxRecallTerms {
			break
		}
	}
	return out
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the and for are but not you your yours all any can had has have her him his how its
		let may our out own she that than then them they this those these too very was were what when where which
		who whom why will with would could should about above after again against been before being below between
		both did does doing down during each few from further here into just more most nor off once only other over
		same some such there their theirs through under until while also please tell know want like get got make
		remember me my mine i im it is be do of to in on at by or as if so no yes hey hi hello thanks thank`) {
		m[w] = true
	}
	return m
}()
