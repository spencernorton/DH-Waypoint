# BitTorrent v2 metadata indexing

DH-Waypoint indexes v1, pure v2 and hybrid torrent metadata through discovery,
peer metadata exchange and validated `.torrent` import. The complete SHA-256
identity survives acquisition, persistence, exact lookup and magnet generation.
No classifier, model provider or external processor is required to index and
query the raw catalogue. The backend does not download payload files.

## Identities and storage

`protocol.InfoHash` distinguishes 20-byte SHA-1 from 32-byte SHA-256. Its
internal algorithm tag is never serialized. `protocol.ID` remains exactly
20 bytes for DHT nodes, peer IDs and peer/tracker wire keys. Its constructors
reject oversized values rather than truncate them.

The catalogue keeps its existing `bytea info_hash` primary and foreign keys.
A v1 torrent retains its SHA-1 key. A pure v2 torrent uses its complete SHA-256
key. A validated hybrid retains its SHA-1 key and has a unique, nullable
`torrents.info_hash_v2` alias. Full-v2 lookup resolves either hybrid identity
to the same record. Existing v1 rows are not rewritten or assigned invented
SHA-256 hashes. Names, sizes and truncated-prefix coincidences never establish
an association.

Both hybrid hashes must match the same original info bytes. The validator also
checks file names, byte lengths, ordering and piece alignment, including BEP 47
padding. This validates the metadata relationship; it does not prove that
actual payload bytes satisfy both piece hash formats.

## Acquisition and validation

The DHT remains a 160-bit protocol. Its untyped discovery key is resolved only
after peer metadata validates against SHA-1 or the truncated SHA-256. Pure v2
metadata is persisted under the complete SHA-256, never under its discovery
prefix. Known full identities use `metainforequester.RequestInfoHash` through
the ordinary reputation, rate-limit, deadline and metrics chain; verification
checks all 32 bytes even when two claimed hashes share the same wire prefix.

`ParseInfoBytes` hashes the original info bytes before decoding. Canonical
bencoding, duplicate keys, supported metadata version, v2 tree shape, file
lengths, roots, paths, alternate names and resource bounds are checked before
indexing. Info dictionaries are capped at 10 MiB, files at 100,000, path depth
at 64, path components at 255 UTF-8 bytes, and piece length at 16 MiB. V2 pieces
must be a power of two and at least 16 KiB. Existing database-safe UTF-8 and
control-character restrictions apply to v2 paths too.

BEP 9 transfers only the info dictionary. The validation result separately
records whether outer piece layers were available and verified. Large-file
BEP 9 acquisition indexes authenticated roots and file metadata; it does not
claim to have verified unavailable layers or file data. `ParseTorrentBytes`
and complete-file import validate every required outer piece layer against
its Merkle root, including zero hashes that balance an incomplete tree.

Magnet exact topics use `urn:btih:` for SHA-1 and `urn:btmh:1220` followed by
all 64 SHA-256 hex characters for v2. A hybrid magnet contains both exact
topics. Parsing a magnet produces claims; only metadata verification establishes
their relationship.

## API and processor compatibility

- `GET /api/v2/search`: raw catalogue browse or literal name substring search,
  independent of classification. Use `hash` for exact full-hash lookup and
  `after` for a canonical-key cursor. Pages contain 1–100 rows. Text is at
  least three characters and at most 4096 UTF-8 bytes. Wildcards are literal.
  Counts are omitted, and requests have a 30-second query budget.
- `GET /api/v2/torrents/{hash}`: complete identities, name, size, file status
  and a full magnet. A validated hybrid resolves by either full hash.
- `GET /api/v2/torrents/{hash}/files`: retained file inventory with `limit`
  and `after_index`. An over-threshold inventory is explicitly partial.
- `POST /import/torrent`: one complete torrent file, with optional `hash`
  expectation. Validation derives identity, name, size and private status from
  original bytes. The body is capped at 20 MiB. Existing import/serving policy
  still applies; this endpoint shares the operator access boundary of `/import`.
- `GET /api/v2/openapi.yaml`: [v2 contract](reference/openapi-v2.yaml).

REST v1, GraphQL `Hash20` and Torznab retain SHA-1 semantics and filter pure v2
records. GraphQL refuses to serialize a v2 prefix as `Hash20`. The existing
NDJSON `/import` path retains v1 behavior; bare v2 hash/name claims require
verified metadata through the new import path. The private tracker and torrent
creator remain v1-only. Modern full-v2 catalogue consumers use REST v2.

The `dh-waypoint.processor.v1` job schema and strict decoder are unchanged.
V1 processors receive only SHA-1 jobs, including the validated v1 side of a
hybrid. A service opts in with manifest/status contract
`dh-waypoint.processor.v2`, discovered at the existing `/v1/manifest` and
`/v1/status` bootstrap paths. V2 processing uses `/v2/process`. Its envelope
has the same fields; `input.info_hash` is a complete 40- or 64-character
canonical digest. Use `processorapi/v2` validation, which preserves all v1
bounds and binding checks. V2 feedback uses its own contract and
`/v2/feedback`; full identity job exports carry a v2 contract. The raw v2
catalogue API exposes a hybrid's full identity pair. A v1-only LLM adapter can
continue operating on v1/hybrid jobs while a v2-capable service is registered.

Private-source facts and explicit exclusions against a validated hybrid alias
protect the canonical record as well. No identity association grants eligibility
or overrides private flags, quarantine or processor restrictions.

## Migration and rollback

Migration 70 adds complete identity constraints and the unique v2 alias. Active
processor, verdict and recovery ledgers accept only full 20- or 32-byte keys.
Retired inference-work tables retain their historical constraints. Peer IDs and
private tracker wire keys remain 20 bytes. Historical migrations are unchanged.

Migration 71 builds the raw-name trigram GIN index concurrently. Qualify build
time, I/O and disk headroom on a restored catalogue before production upgrade.
An interrupted concurrent build can leave an invalid index: inspect
`pg_index.indisvalid`, remove the failed index with `DROP INDEX CONCURRENTLY`,
and retry the unapplied migration. Do not mark an invalid build successful.

A v1-only database can roll back these additions. Once any v2 alias, full key
or retained v2 evidence exists, downgrade is refused before dropping identities
or narrowing constraints. Restore a matching backup instead of running an older
binary against v2 state. No production migration or deployment is part of this
source change.

## Verification and limits

Synthetic tests cover v1/pure-v2/hybrid metadata, malformed and mismatched
hashes/trees, repeated hybrid padding, canonical encoding and bounds, magnets,
full-prefix collision separation, raw no-plugin queries, privacy/eligibility,
file pagination, processor negotiation and migration/downgrade guards. An
isolated test module interoperates over loopback with the independent
anacrolix peer implementation for all three formats and asserts no payload
exchange. It uses the peer's magnet/discovery registration path.

Run the ordinary public checks, PostgreSQL regressions with a disposable test
DSN, and `go test -race -count=1 -timeout=2m ./...` in `tools/protocolinterop`.
The separate module keeps the complete peer client's test dependencies out of
the backend's runtime dependency graph. CI also runs that interoperability test.

This is metadata indexing, not a payload downloader, piece-layer exchange
client or complete v2 torrent-file reconstruction service. BEP 9 does not carry
outer layers. Existing transport constraints still apply; uTP, encrypted-peer
fallback, full dual-stack DHT and hybrid handshake upgrades are separate
capabilities. This work does not establish catalogue-scale latency or deployed
crawl yield.

## Specification sources

- [BEP 52](https://www.bittorrent.org/beps/bep_0052.html): v2 metadata,
  identities, file trees, Merkle layers and truncated wire keys.
- [BEP 52 upgrade path](https://github.com/bittorrent/bittorrent.org/blob/master/beps/bep_0052.rst#upgrade-path):
  hybrid consistency and padding.
- [BEP 9](https://www.bittorrent.org/beps/bep_0009.html): info-only metadata
  exchange and magnet topics.
- [BEP 10](https://www.bittorrent.org/beps/bep_0010.html): peer-local extension IDs.
- [BEP 5](https://www.bittorrent.org/beps/bep_0005.html): 160-bit DHT keys.
- [BEP 47](https://www.bittorrent.org/beps/bep_0047.html): padding and attributes.
