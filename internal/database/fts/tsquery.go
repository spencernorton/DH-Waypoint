package fts

import (
	"regexp"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/lexer"
)

func AppQueryToTsquery(str string) string {
	l := queryLexer{newLexer(str)}

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

type queryLexer struct {
	ftsLexer
}

type Token string

const (
	TokenQuoted      Token = "QUOTED"
	TokenOpenParens  Token = "OPEN_PARENS"
	TokenCloseParens Token = "CLOSE_PARENS"
	TokenOperator    Token = "OPERATOR"
	TokenNegation    Token = "NEGATION"
	TokenPhrase      Token = "PHRASE"
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
					}
					if tokens[j].Token == TokenCloseParens {
						depth--
						if depth == 0 {
							break
						}
					}
					parensTokens = append(parensTokens, tokens[j])
				}
				i += len(parensTokens)
				parensExpr := appQueryTokensToTsquery(parensTokens...)
				if len(parensExpr) > 0 {
					addExpr("(" + parensExpr + ")")
				}
			}
			i++
		}
	}

	return strings.Join(parts, " ")
}
