# BitTorrent v2 metadata indexing

The identity/validation foundation distinguishes a complete torrent identity
from a discovery key. `protocol.InfoHash` retains either a 20-byte SHA-1 or a
32-byte SHA-256 digest. `protocol.ID` remains 20 bytes for DHT node IDs, peer
IDs and peer/tracker wire keys. A truncated SHA-256 digest is never stored or
advertised as a SHA-1 torrent identity.

`metainfo.ParseInfoBytes` checks a complete expected hash against the original
info bytes, then validates canonical encoding, file layouts and resource
bounds. `ParseDiscoveredInfo` resolves an untyped 20-byte discovery key to
validated full identities. A hybrid pair is emitted only when the same info
dictionary has consistent v1 and v2 names, order, lengths and piece alignment.
Names and common hash prefixes do not establish a hybrid association.

`ParseTorrentBytes` also verifies the outer v2 piece layers against each
referenced Merkle root. BEP 9 transfers only the info dictionary: its result
separately records whether the piece layers have been verified. Indexing
metadata does not establish that a peer has the payload, or that actual file
bytes match either hash format. This backend does not download payload data.

Magnet exact topics use `urn:btih:` for SHA-1 and `urn:btmh:1220` followed by
the complete SHA-256 digest for v2. A verified hybrid may include both topics.
Parsing a magnet produces claims which must still be verified against metadata.

## Specification sources

- [BEP 52](https://www.bittorrent.org/beps/bep_0052.html): v2 metadata,
  SHA-256 identities, file trees, Merkle layers and truncated wire keys.
- [BEP 52 source and upgrade path](https://github.com/bittorrent/bittorrent.org/blob/master/beps/bep_0052.rst#upgrade-path):
  hybrid layout consistency and padding requirements.
- [BEP 9](https://www.bittorrent.org/beps/bep_0009.html): info-only metadata
  exchange, 16 KiB blocks and magnet exact topics.
- [BEP 10](https://www.bittorrent.org/beps/bep_0010.html): peer-local extension
  message IDs; the receiving and sending IDs need not be equal.
- [BEP 5](https://www.bittorrent.org/beps/bep_0005.html): the 160-bit DHT key
  space, which is separate from a full v2 torrent identity.
- [BEP 47](https://www.bittorrent.org/beps/bep_0047.html): hybrid padding
  files and extended file attributes.

## Integration status

This first increment supplies identity types, parsers, magnet helpers and
synthetic validation regressions. The existing crawler, catalogue schema,
imports and public APIs still use their v1 paths. These foundation helpers
alone do not constitute end-to-end v2 support. Acquisition, persistence and
explicit API/processor compatibility are the next integration increment.
