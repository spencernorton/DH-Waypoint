// Command metainfo_inventory audits a directory of hash-named original torrent
// files with the native import parser. It neither contacts peers nor imports.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
)

type result struct {
	Hash          string `json:"hash,omitempty"`
	CatalogueSize *uint  `json:"catalogue_size,omitempty"`
	State         string `json:"state"`
	Size          int64  `json:"size"`
	Files         int    `json:"files"`
	SHA256        string `json:"sha256,omitempty"`
}

func inspect(root *os.Root, entry os.DirEntry) result {
	expected, err := protocol.ParseInfoHash(strings.TrimSuffix(entry.Name(), ".torrent"))
	if err != nil || !entry.Type().IsRegular() {
		return result{State: "invalid_name"}
	}
	out := result{Hash: expected.String(), State: "read_error"}
	f, err := root.Open(entry.Name())
	if err != nil {
		return out
	}
	defer f.Close()
	const limit = 2 * metainfo.MaxMetaInfoBytes
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return out
	}
	if len(raw) > limit {
		out.State = "invalid_size"
		return out
	}
	parsed, err := metainfo.ParseTorrentBytes(raw, &expected)
	if err != nil {
		out.State = "invalid_metainfo"
		return out
	}
	out.Size = parsed.Info.TotalLength()
	out.Files = max(1, len(parsed.Files))
	switch {
	case parsed.Info.Private != nil && *parsed.Info.Private:
		out.State = "private"
	case parsed.HasUnsafeFileAttributes():
		out.State = "unsafe_attributes"
	default:
		out.State = "validated"
		// The native catalogue excludes BEP 47 padding from its indexed size.
		projected := model.TorrentFromMetadata(parsed, "metainfo_import", false, metainfo.MaxFilesCount)
		out.CatalogueSize = &projected.Size
		sum := sha256.Sum256(raw)
		out.SHA256 = hex.EncodeToString(sum[:])
	}
	return out
}

func inventory(directory string, destination io.Writer) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	out := bufio.NewWriter(destination)
	enc := json.NewEncoder(out)
	for {
		entries, err := dir.ReadDir(256)
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".torrent") {
				if err := enc.Encode(inspect(root, entry)); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return out.Flush()
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: metainfo_inventory DIRECTORY")
		os.Exit(2)
	}
	if err := inventory(os.Args[1], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
