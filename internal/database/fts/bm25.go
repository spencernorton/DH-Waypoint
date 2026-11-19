package fts

import "strings"

// AppQueryToBM25Terms translates an application query string into the exact
// lexemes for a ParadeDB BM25 conjunction (search_text &&& ?).
//
// A query is eligible only when it is a pure AND of exact single-word
// lexemes, so the BM25 conjunction matches exactly the same rows as the
// tsquery produced by AppQueryToTsquery. search_text stores the tsvector
// lexemes verbatim (indexed with pdb.whitespace('lowercase=false')), and
// Tokenize below applies the same lowercasing/transliteration that built
// those lexemes.
//
// Anything richer — quoted phrases, OR, negation, wildcards, parentheses,
// followed-by, or a bare season (which the tsquery path expands to glued
// season-episode lexemes) — is ineligible and the caller must keep the
// existing tsv @@ tsquery path.
func AppQueryToBM25Terms(str string) (terms []string, eligible bool) {
	l := queryLexer{newLexer(str)}

	var tokens []TokenValue

	for {
		token, ok := l.readQueryToken()
		if !ok {
			break
		}

		tokens = append(tokens, token)
	}

	for _, token := range tokens {
		switch token.Token {
		case TokenPhrase:
			phrases := Tokenize(token.Value)
			if len(phrases) != 1 || len(phrases[0]) != 1 {
				return nil, false
			}

			word := phrases[0][0]
			if word == "" || seasonTerm.MatchString(word) {
				return nil, false
			}

			terms = append(terms, word)
		case TokenOperator:
			if Operator(token.Value) != OperatorAnd {
				return nil, false
			}
		default:
			return nil, false
		}
	}

	if len(terms) == 0 {
		return nil, false
	}

	// Cross-check against the tsquery rendering: it must be exactly the
	// same lexemes joined by AND, with no other operator or expansion.
	tsquery := AppQueryToTsquery(str)
	if tsquery == "" || strings.ContainsAny(tsquery, "|!()") ||
		strings.Contains(tsquery, "<->") || strings.Contains(tsquery, ":*") {
		return nil, false
	}

	parts := strings.Split(tsquery, " & ")
	if len(parts) != len(terms) {
		return nil, false
	}

	for i, part := range parts {
		if unquoteLexeme(part) != terms[i] {
			return nil, false
		}
	}

	return terms, true
}

func unquoteLexeme(str string) string {
	if len(str) >= 2 && str[0] == '\'' && str[len(str)-1] == '\'' {
		return strings.ReplaceAll(str[1:len(str)-1], "''", "'")
	}

	return str
}
