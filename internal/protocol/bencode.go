package protocol

import (
	"bytes"

	"github.com/anacrolix/torrent/bencode"
)

// UnmarshalBencode is bencode.Unmarshal for input from the network. The
// library allocates a string's declared length before reading it, up to
// 128 MiB by default, so no string may claim to be longer than the input.
func UnmarshalBencode(data []byte, v any) error {
	d := bencode.NewDecoder(bytes.NewReader(data))
	d.MaxStrLen = max(int64(len(data)), 1) // zero selects the library default

	if err := d.Decode(v); err != nil {
		return err
	}

	return d.ReadEOF()
}
