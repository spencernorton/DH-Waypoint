package fts_test

import (
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/fts"
	"github.com/stretchr/testify/assert"
)

//nolint:gosmopolitan
func TestAppQueryToTsquery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"1 word", "foo", "foo"},
		{"2 words", "foo Bar", "foo & bar"},
		{"3 words", "foo bar baz", "foo & bar & baz"},
		{
			"quotes, operators, parens, wildcards", "\"make me a \" . (sandwich | panini) !cheese mayo*",
			"make <-> me <-> a <-> (sandwich | panini) & ! cheese & mayo:*",
		},
		{"unmatched quotes", "\"make me a sandwich", "make <-> me <-> a <-> sandwich"},
		{
			"unmatched parens", "\"make me a \" . (sandwich | panini",
			"make <-> me <-> a <-> (sandwich | panini)",
		},
		{"Ukrainian", "зроби мені бутерброд", "zrobi & meni & buterbrod"},
		{"Chinese", "给我做一个三明治", "Gei <-> Wo <-> Zuo <-> Yi <-> Ge <-> San <-> Ming <-> Zhi"},
		{"Arabic", "اصنع لي شطيرة", "'Sn`' & ly & 'shTyr@'"},
		{"Arabic (quoted)", "\"اصنع لي شطيرة\"", "'Sn`' <-> ly <-> 'shTyr@'"},
		{"ampersand prefix", "&eacute;", "eacute"},
		{"bare season matches glued episode", "Shrek S51", "shrek & (s51 | s51e:*)"},
		{"quoted bare season", "\"Shrek s51\"", "shrek <-> (s51 | s51e:*)"},
		{"negated bare season", "!s1", "! (s1 | s1e:*)"},
		{"typed season wildcard", "Shrek s51*", "shrek & s51:*"},
		{"quoted season wildcard", "\"Shrek s51\"*", "shrek <-> s51:*"},
		{"season inside quoted wildcard phrase", "\"s51 shrek\"*", "(s51 | s51e:*) <-> shrek:*"},
		{"full episode unchanged", "Shrek s51e01", "shrek & s51e01"},
		{"season word unchanged", "season s", "season & s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, fts.AppQueryToTsquery(tt.input))
		})
	}
}
