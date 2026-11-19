package fts_test

import (
	"strings"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/fts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:gosmopolitan
func TestAppQueryToBM25Terms(t *testing.T) {
	t.Parallel()

	eligible := []struct {
		name  string
		input string
		want  []string
	}{
		{"1 word", "foo", []string{"foo"}},
		{"2 words lowercased", "foo Bar", []string{"foo", "bar"}},
		{"3 words", "foo bar baz", []string{"foo", "bar", "baz"}},
		{"explicit and", "foo & bar", []string{"foo", "bar"}},
		{"resolution term", "1080p", []string{"1080p"}},
		{"full episode is one lexeme", "Shrek s51e01", []string{"shrek", "s51e01"}},
		{"hyphen splits like tsquery", "spider-man 1080p", []string{"spider", "man", "1080p"}},
		{"year term", "Synthetic Title 2026", []string{"synthetic", "title", "2026"}},
		{"ukrainian transliteration", "зроби мені бутерброд", []string{"zrobi", "meni", "buterbrod"}},
		{"single CJK lexeme keeps exact case", "明", []string{"Ming"}},
		{"duplicate terms kept", "foo foo", []string{"foo", "foo"}},
	}
	for _, tt := range eligible {
		t.Run("eligible/"+tt.name, func(t *testing.T) {
			t.Parallel()
			terms, ok := fts.AppQueryToBM25Terms(tt.input)
			require.True(t, ok, "input %q", tt.input)
			assert.Equal(t, tt.want, terms)

			// The BM25 terms must be exactly the tsquery AND operands so
			// both predicates match the same row set.
			tsquery := fts.AppQueryToTsquery(tt.input)
			assert.NotContains(t, tsquery, "<->")
			assert.NotContains(t, tsquery, "|")
			assert.Len(t, strings.Split(tsquery, " & "), len(terms))
		})
	}

	ineligible := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"whitespace only", "  "},
		{"quoted phrase", "\"make me a sandwich\""},
		{"quoted single word", "\"foo\""},
		{"or", "sandwich | panini"},
		{"negation", "sandwich !cheese"},
		{"wildcard", "mayo*"},
		{"followed by dot", "make . sandwich"},
		{"parens", "(sandwich panini)"},
		{"bare season", "Shrek s51"},
		{"bare season single digit", "Shrek s5"},
		{"bare season zero padded", "Shrek s05"},
		{"chinese multi lexeme phrase", "给我做一个三明治"},
		{"mixed operators", "\"make me a \" . (sandwich | panini) !cheese mayo*"},
	}
	for _, tt := range ineligible {
		t.Run("ineligible/"+tt.name, func(t *testing.T) {
			t.Parallel()
			terms, ok := fts.AppQueryToBM25Terms(tt.input)
			assert.False(t, ok, "input %q", tt.input)
			assert.Nil(t, terms)
		})
	}
}
