package fts

import (
	"regexp"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/lexer"
)

func AppQueryToTsquery(str string) string {
	return appQueryToTsquery(str, false)
}

// AppQueryToCompoundExpandedTsquery parses str like AppQueryToTsquery and
// additionally expands ASCII hyphenated compounds (e.g. "spider-man" ->
// "((spider & man) | spiderman)") so queries match both split and glued
// index lexemes without altering space-separated queries.
func AppQueryToCompoundExpandedTsquery(str string) string {
	return appQueryToTsquery(str, true)
}

func appQueryToTsquery(str string, expandCompounds bool) string {
	l := queryLexer{ftsLexer: newLexer(str), expandCompounds: expandCompounds}

	var tokens []TokenValue

	for {
		token, ok := l.readQueryToken()
		if !ok {
			break
		}

		tokens = append(tokens, token)
	}

	return appQueryTokensToTsquery(tokens...)
}

// AppQueryToBM25TermGroups expands ASCII hyphenated compounds into a bounded
// slice of BM25-compatible AND-term groups (e.g. "spider-man 1080p" ->
// [["spider","man","1080p"], ["spiderman","1080p"]]) so callers can execute
// compound expansion directly inside ParadeDB BM25 without falling back to GIN.
func AppQueryToBM25TermGroups(str string) ([][]string, bool) {
	baseTerms, ok := AppQueryToBM25Terms(str)
	if !ok {
		return nil, false
	}
	l := queryLexer{ftsLexer: newLexer(str), expandCompounds: true}
	var groups [][]string
	groups = append(groups, []string{})
	hasCompound := false
	for {
		tok, ok := l.readQueryToken()
		if !ok {
			break
		}
		switch tok.Token {
		case TokenOperator:
			if Operator(tok.Value) != OperatorAnd {
				return [][]string{baseTerms}, true
			}
		case TokenPhrase:
			phrases := Tokenize(tok.Value)
			if len(phrases) != 1 || len(phrases[0]) != 1 {
				return [][]string{baseTerms}, true
			}
			w := phrases[0][0]
			for i := range groups {
				groups[i] = append(groups[i], w)
			}
		case TokenCompound:
			hasCompound = true
			rawSegs := strings.Split(tok.Value, "-")
			split := make([]string, len(rawSegs))
			var glued strings.Builder
			for i, s := range rawSegs {
				w := strings.ToLower(s)
				split[i] = w
				glued.WriteString(w)
			}
			// Cap combinatorial explosion at 4 alternative groups.
			if len(groups) >= 4 {
				for i := range groups {
					groups[i] = append(groups[i], split...)
				}
				continue
			}
			next := make([][]string, 0, len(groups)*2)
			for _, g := range groups {
				gSplit := append(append([]string(nil), g...), split...)
				gGlued := append(append([]string(nil), g...), glued.String())
				next = append(next, gSplit, gGlued)
			}
			groups = next
		default:
			return [][]string{baseTerms}, true
		}
	}
	if !hasCompound {
		return [][]string{baseTerms}, true
	}
	return groups, true
}

type queryLexer struct {
	ftsLexer
	expandCompounds bool
}

type Token string

const (
	TokenQuoted      Token = "QUOTED"
	TokenOpenParens  Token = "OPEN_PARENS"
	TokenCloseParens Token = "CLOSE_PARENS"
	TokenOperator    Token = "OPERATOR"
	TokenNegation    Token = "NEGATION"
	TokenPhrase      Token = "PHRASE"
	TokenCompound    Token = "COMPOUND"
	TokenWildcard    Token = "WILDCARD"
)

type TokenValue struct {
	StartPos int
	Length   int
	Token    Token
	Value    string
}

type Operator string

const (
	OperatorAnd        Operator = "+"
	OperatorOr         Operator = "|"
	OperatorFollowedBy Operator = "."
)

func (l *queryLexer) readQueryToken() (TokenValue, bool) {
	for {
		if l.IsEOF() {
			return TokenValue{}, false
		}

		start := l.Pos()

		if l.ReadChar('(') {
			return TokenValue{l.Pos(), 1, TokenOpenParens, "("}, true
		}

		if l.ReadChar(')') {
			return TokenValue{l.Pos(), 1, TokenCloseParens, ")"}, true
		}

		if l.ReadChar('&') {
			return TokenValue{l.Pos(), 1, TokenOperator, string(OperatorAnd)}, true
		}

		if l.ReadChar('|') {
			return TokenValue{l.Pos(), 1, TokenOperator, string(OperatorOr)}, true
		}

		if l.ReadChar('.') {
			return TokenValue{l.Pos(), 1, TokenOperator, string(OperatorFollowedBy)}, true
		}

		if l.ReadChar('!') {
			return TokenValue{l.Pos(), 1, TokenNegation, "-"}, true
		}

		if l.ReadChar('*') {
			return TokenValue{l.Pos(), 1, TokenWildcard, "*"}, true
		}

		if quoted, _ := l.readQuotedString('"'); quoted != "" {
			return TokenValue{start, l.Pos() - start, TokenQuoted, quoted}, true
		}

		if phrase := l.ReadWhile(lexer.IsWordChar); phrase != "" {
			if l.expandCompounds && isASCIIWord(phrase) {
				segs := []string{phrase}
				for len(segs) < 3 && l.ReadChar('-') {
					r, ok := l.Read()
					if !ok {
						break
					}
					if !isASCIIWordRune(r) {
						l.Backup()
						break
					}
					rest := l.ReadWhile(isASCIIWordRune)
					segs = append(segs, string(r)+rest)
				}
				if len(segs) > 1 {
					return TokenValue{start, l.Pos() - start, TokenCompound, strings.Join(segs, "-")}, true
				}
			}
			return TokenValue{start, l.Pos() - start, TokenPhrase, phrase}, true
		}

		l.Read()
	}
}

// seasonTerm is a bare season such as s51. Release names usually glue it to
// the episode (S51E01), which the tokenizer indexes as the single lexeme s51e01.
var seasonTerm = regexp.MustCompile(`^s\d+$`)

// queryLexeme quotes one query word. A bare season also matches the glued
// season-episode lexeme, so s51 finds S51E01 without matching s510, and
// single-digit seasons match both zero-padded (s05/s05e:*) and unpadded
// (s5/s5e:*) forms. A wildcard the user typed (s51*) already covers it.
func queryLexeme(word string, wildcard bool) string {
	quoted := quoteLexeme(word, false)
	if wildcard || !seasonTerm.MatchString(word) {
		return quoted
	}
	if len(word) == 2 && word[1] >= '1' && word[1] <= '9' {
		padded := "s0" + word[1:]
		return "(" + quoted + " | " + word + "e:* | " + padded + " | " + padded + "e:*)"
	}
	if len(word) == 3 && word[1] == '0' && word[2] >= '1' && word[2] <= '9' {
		unpadded := "s" + word[2:]
		return "(" + quoted + " | " + word + "e:* | " + unpadded + " | " + unpadded + "e:*)"
	}

	return "(" + quoted + " | " + word + "e:*)"
}

func appQueryTokensToTsquery(tokens ...TokenValue) string {
	var parts []string

	i := 0
outer:
	for {
		var operator Operator
		var negated bool
		for {
			if i >= len(tokens) {
				break outer
			}
			token := tokens[i]
			wildcard := len(tokens) > i+1 && tokens[i+1].Token == TokenWildcard
			addExpr := func(expr string) {
				if len(parts) > 0 {
					switch operator {
					case OperatorOr:
						parts = append(parts, "|")
					case OperatorFollowedBy:
						parts = append(parts, "<->")
					default:
						parts = append(parts, "&")
					}
				}
				if negated {
					parts = append(parts, "!")
				}
				if len(tokens) > i+1 && tokens[i+1].Token == TokenWildcard {
					expr += ":*"
					i++
				}
				parts = append(parts, expr)
				operator = ""
				negated = false
			}
			switch token.Token {
			case TokenOperator:
				operator = Operator(token.Value)
			case TokenNegation:
				negated = true
			case TokenQuoted:
				tokenized := TokenizeFlat(token.Value)
				var quotedWords []string
				for j, word := range tokenized {
					quotedWords = append(quotedWords, queryLexeme(word, wildcard && j == len(tokenized)-1))
				}
				if len(quotedWords) > 0 {
					addExpr(strings.Join(quotedWords, " <-> "))
				}
			case TokenCompound:
				rawSegs := strings.Split(token.Value, "-")
				splitWords := make([]string, 0, len(rawSegs))
				var glued strings.Builder
				for j, seg := range rawSegs {
					w := strings.ToLower(seg)
					glued.WriteString(w)
					splitWords = append(splitWords, queryLexeme(w, wildcard && j == len(rawSegs)-1))
				}
				gluedLex := quoteLexeme(glued.String(), false)
				if wildcard {
					splitWords[len(splitWords)-1] += ":*"
					gluedLex += ":*"
					i++
				}
				if len(parts) > 0 {
					switch operator {
					case OperatorOr:
						parts = append(parts, "|")
					case OperatorFollowedBy:
						parts = append(parts, "<->")
					default:
						parts = append(parts, "&")
					}
				}
				if negated {
					parts = append(parts, "!")
				}
				parts = append(parts, "(("+strings.Join(splitWords, " & ")+") | "+gluedLex+")")
				operator = ""
				negated = false
			case TokenPhrase:
				tokenized := Tokenize(token.Value)
				var phrases []string
				for p, phrase := range tokenized {
					var quotedWords []string
					for j, word := range phrase {
						last := p == len(tokenized)-1 && j == len(phrase)-1
						quotedWords = append(quotedWords, queryLexeme(word, wildcard && last))
					}
					if len(quotedWords) > 0 {
						phrases = append(phrases, strings.Join(quotedWords, " <-> "))
					}
				}
				if len(phrases) > 0 {
					addExpr(strings.Join(phrases, " & "))
				}
			case TokenOpenParens:
				var parensTokens []TokenValue
				depth := 1
				for j := i + 1; j < len(tokens); j++ {
					if tokens[j].Token == TokenOpenParens {
						depth++
					} else if tokens[j].Token == TokenCloseParens {
						depth--
						if depth == 0 {
							i = j
							break
						}
					}
					parensTokens = append(parensTokens, tokens[j])
					if j == len(tokens)-1 {
						i = j
					}
				}
				if expr := appQueryTokensToTsquery(parensTokens...); expr != "" {
					addExpr("(" + expr + ")")
				}
			case TokenCloseParens:
			case TokenWildcard:
			}
			i++
		}
	}

	return strings.Join(parts, " ")
}

func isASCIIWord(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isASCIIWordRune(rune(s[i])) {
			return false
		}
	}
	return true
}

func isASCIIWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
