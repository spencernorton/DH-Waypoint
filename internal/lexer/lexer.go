package lexer

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func NewLexer(str string) Lexer {
	return Lexer{
		str: str,
	}
}

type Lexer struct {
	str     string
	bytePos int
	pos     int
}

func (l *Lexer) Pos() int {
	return l.pos
}

func (l *Lexer) Read() (rune, bool) {
	if l.bytePos >= len(l.str) {
		return 0, false
	}
	if b := l.str[l.bytePos]; b < utf8.RuneSelf {
		l.bytePos++
		l.pos++
		return rune(b), true
	}
	r, size := utf8.DecodeRuneInString(l.str[l.bytePos:])
	l.bytePos += size
	l.pos++
	return r, true
}

func (l *Lexer) Backup() {
	if l.bytePos == 0 {
		panic("lexer: cannot backup past start")
	}
	_, size := utf8.DecodeLastRuneInString(l.str[:l.bytePos])
	l.bytePos -= size
	l.pos--
}

func (l *Lexer) BackupN(n int) {
	for range n {
		l.Backup()
	}
}

func (l *Lexer) IsEOF() bool {
	return l.bytePos >= len(l.str)
}

func (l *Lexer) ReadIf(fn func(rune) bool) (rune, bool) {
	if l.bytePos >= len(l.str) {
		return 0, false
	}
	var r rune
	var size int
	if b := l.str[l.bytePos]; b < utf8.RuneSelf {
		r = rune(b)
		size = 1
	} else {
		r, size = utf8.DecodeRuneInString(l.str[l.bytePos:])
	}
	if !fn(r) {
		return 0, false
	}
	l.bytePos += size
	l.pos++
	return r, true
}

func (l *Lexer) ReadWhile(fn func(rune) bool) string {
	start := l.bytePos
	hasInvalid := false
	for l.bytePos < len(l.str) {
		var r rune
		var size int
		if b := l.str[l.bytePos]; b < utf8.RuneSelf {
			r = rune(b)
			size = 1
		} else {
			r, size = utf8.DecodeRuneInString(l.str[l.bytePos:])
			if r == utf8.RuneError && size == 1 {
				hasInvalid = true
			}
		}
		if !fn(r) {
			break
		}
		l.bytePos += size
		l.pos++
	}
	if !hasInvalid {
		return l.str[start:l.bytePos]
	}
	return strings.ToValidUTF8(l.str[start:l.bytePos], string(utf8.RuneError))
}

func (l *Lexer) ReadUntil(fn func(rune) bool) string {
	start := l.bytePos
	hasInvalid := false
	for l.bytePos < len(l.str) {
		var r rune
		var size int
		if b := l.str[l.bytePos]; b < utf8.RuneSelf {
			r = rune(b)
			size = 1
		} else {
			r, size = utf8.DecodeRuneInString(l.str[l.bytePos:])
			if r == utf8.RuneError && size == 1 {
				hasInvalid = true
			}
		}
		l.bytePos += size
		l.pos++
		if fn(r) {
			break
		}
	}
	if !hasInvalid {
		return l.str[start:l.bytePos]
	}
	return strings.ToValidUTF8(l.str[start:l.bytePos], string(utf8.RuneError))
}

func (l *Lexer) ReadInt() (int, bool) {
	str := l.ReadWhile(IsInt)
	if str == "" {
		return 0, false
	}

	n, err := strconv.Atoi(str)
	if err != nil {
		panic(err)
	}

	return n, true
}

func (l *Lexer) ReadChar(r1 rune) bool {
	_, ok := l.ReadIf(IsChar(r1))
	return ok
}

func IsInt(r rune) bool {
	return r >= '0' && r <= '9'
}

func IsChar(r1 rune) func(rune) bool {
	return func(r2 rune) bool {
		return r1 == r2
	}
}

func IsWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func IsNonWordChar(r rune) bool {
	return !IsWordChar(r)
}
