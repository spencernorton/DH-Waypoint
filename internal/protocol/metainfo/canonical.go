package metainfo

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// validateEncoding bounds recursion and validates canonical bencoding before
// custom dependency unmarshallers recurse into file trees. In particular,
// duplicate/unsorted keys cannot silently replace identity-bearing fields.
func validateEncoding(b []byte) error {
	s := encodingScanner{b: b}
	if err := s.value(0); err != nil {
		return err
	}
	if s.i != len(b) {
		return errors.New("trailing bencoded data")
	}
	return nil
}

type encodingScanner struct {
	b        []byte
	i, nodes int
}

func (s *encodingScanner) str() ([]byte, error) {
	start := s.i
	for s.i < len(s.b) && s.b[s.i] >= '0' && s.b[s.i] <= '9' {
		s.i++
	}
	if s.i == start || s.i >= len(s.b) || s.b[s.i] != ':' {
		return nil, errors.New("invalid bencoded string")
	}
	if s.i-start > 1 && s.b[start] == '0' {
		return nil, errors.New("non-canonical string length")
	}
	n, err := strconv.ParseUint(string(s.b[start:s.i]), 10, 32)
	if err != nil {
		return nil, errors.New("bencoded string length out of bounds")
	}
	s.i++
	if n > uint64(len(s.b)-s.i) {
		return nil, errors.New("truncated bencoded string")
	}
	b := s.b[s.i : s.i+int(n)]
	s.i += int(n)
	return b, nil
}

func (s *encodingScanner) value(depth int) error {
	s.nodes++
	if depth > MaxPathDepth+8 || s.nodes > 1_000_000 {
		return errors.New("bencoded metadata exceeds structural bounds")
	}
	if s.i >= len(s.b) {
		return errors.New("truncated bencoded value")
	}
	switch s.b[s.i] {
	case 'i':
		s.i++
		start := s.i
		for s.i < len(s.b) && s.b[s.i] != 'e' {
			s.i++
		}
		if s.i >= len(s.b) {
			return errors.New("truncated bencoded integer")
		}
		v := s.b[start:s.i]
		if len(v) == 0 || bytes.Equal(v, []byte("-0")) || (len(v) > 1 && v[0] == '0') || (len(v) > 2 && v[0] == '-' && v[1] == '0') {
			return errors.New("non-canonical integer")
		}
		for j, c := range v {
			if c < '0' || c > '9' {
				if j != 0 || c != '-' {
					return errors.New("invalid bencoded integer")
				}
			}
		}
		if _, err := strconv.ParseInt(string(v), 10, 64); err != nil {
			return errors.New("bencoded integer out of bounds")
		}
		s.i++
	case 'l', 'd':
		dict := s.b[s.i] == 'd'
		s.i++
		var previous []byte
		first := true
		for s.i < len(s.b) && s.b[s.i] != 'e' {
			if dict {
				key, err := s.str()
				if err != nil {
					return err
				}
				if !first && bytes.Compare(previous, key) >= 0 {
					return errors.New("duplicate or unsorted dictionary key")
				}
				previous, first = key, false
			}
			if err := s.value(depth + 1); err != nil {
				return err
			}
		}
		if s.i >= len(s.b) {
			return errors.New("truncated bencoded container")
		}
		s.i++
	default:
		if _, err := s.str(); err != nil {
			return fmt.Errorf("bencoded value: %w", err)
		}
	}
	return nil
}
