package lexer

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestLexerASCIIAndUnicodeOperations(t *testing.T) {
	l := NewLexer("Amélie.2025!日本語-42")
	require.False(t, l.IsEOF())
	require.Equal(t, 0, l.Pos())

	require.Equal(t, "Amélie", l.ReadWhile(IsWordChar))
	require.Equal(t, 6, l.Pos())
	require.True(t, l.ReadChar('.'))
	require.Equal(t, 7, l.Pos())

	n, ok := l.ReadInt()
	require.True(t, ok)
	require.Equal(t, 2025, n)
	require.Equal(t, 11, l.Pos())

	require.Equal(t, "!日", l.ReadUntil(IsChar('日')))
	require.Equal(t, 13, l.Pos())

	l.BackupN(2)
	require.Equal(t, 11, l.Pos())
	r, ok := l.Read()
	require.True(t, ok)
	require.Equal(t, '!', r)

	require.Equal(t, "日本語", l.ReadWhile(IsWordChar))
	require.Equal(t, "-", l.ReadWhile(IsNonWordChar))

	n, ok = l.ReadInt()
	require.True(t, ok)
	require.Equal(t, 42, n)
	require.True(t, l.IsEOF())

	_, ok = l.Read()
	require.False(t, ok)
}

func TestLexerInvalidUTF8AndBackupPanic(t *testing.T) {
	l := NewLexer(string([]byte{'a', 0xff, 'b'}))
	got := l.ReadWhile(func(r rune) bool { return r != 'b' })
	require.Equal(t, "a"+string(utf8.RuneError), got)
	require.Equal(t, 2, l.Pos())

	empty := NewLexer("")
	require.True(t, empty.IsEOF())
	require.Panics(t, func() { empty.Backup() })
}

func TestLexerZeroAllocHotPath(t *testing.T) {
	input := "The.Matrix.1999.2160p.UHD.BluRay.REMUX.HEVC.Atmos-GROUP"
	allocs := testing.AllocsPerRun(100, func() {
		l := NewLexer(input)
		for !l.IsEOF() {
			_ = l.ReadWhile(IsWordChar)
			_ = l.ReadWhile(IsNonWordChar)
		}
	})
	require.Equal(t, float64(0), allocs)
}
