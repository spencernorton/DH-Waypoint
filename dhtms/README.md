# DH-TMS (DH Torrent Metadata Store)

`DH-TMS` is a purpose-built, compact, `mmap`-first BitTorrent metadata storage, search, and swarm-telemetry sidecar for `DH-Waypoint`. It combines established IR and columnar storage techniques (impact-quantized inverted postings with Block-Max WAND, Roaring bitmap policy pushdown, Zstd dictionary-compressed file-tree blocks, and seqlock-coherent swarm slots) with BitTorrent release-name grammar, BEP-52 v1/v2 identity aliasing, and fail-closed serving policy enforcement.

PostgreSQL remains the ACID source of truth for governance, moderation, and audit state. `DH-TMS` operates strictly as a local sidecar (`dh-tmsd`) over a `0600` Unix domain socket or loopback HTTP endpoint, requiring zero ML or external inference services.

See [`docs/adr/ADR-dh-tms-engine.md`](../docs/adr/ADR-dh-tms-engine.md) for the architecture selection record and benchmark methodology.

## Workspace Layout

- `crates/dh-tms-format`: Checked binary envelopes (magic, schema version, generation ID, CRC32C), atomic `CURRENT.json` generation manifest, mmap blob maps, and binary content catalog.
- `crates/dh-tms-grammar`: BitTorrent release-name parser (NFKD accent folding, exact-token media tags, `SxxEyy` / range expansion, whole-season vs partial-pack detection, PostgreSQL-parity adult/blocklist matcher, trigram extraction).
- `crates/dh-tms-hashdir`: Keyed SipHash/XXH3 Robin-Hood hash directory with dense 20-byte reverse v1 `info_hash` column and BEP-52 32-byte v2/hybrid alias table.
- `crates/dh-tms-core`: Packed `32 B` fixed metadata rows, separated name column, byte-capped corpus-dictionary Zstd file-tree blocks, and `1 B` packed facet column.
- `crates/dh-tms-policy`: Fail-closed serving policy plane (dense atomic deny bitset enforced at final egress, `arc_swap` Roaring snapshots, per-doc `u32` payload expiry, CRC32-checksummed generation-tagged revocation WAL, and PG governance lease).
- `crates/dh-tms-swarm`: Seqlock-coherent dense `16 B` atomic swarm slot array with framed, checksummed WAL-before-mutate durability and checkpointing.
- `crates/dh-tms-search`: Immutable generation inverted index (unsaturated `u16` impacts, true MaxScore/Block-Max WAND AND/OR/phrase, dense-term Roaring overlay, `(content_type, published_epoch DESC)` physical ordering, trigram substring index) plus a durable append-only L0 delta overlay (`l0.wal`).
- `crates/dh-tmsd`: Local HTTP/1.1 Unix-socket / loopback daemon and synthetic fixture generator (`init-fixture`, `serve`).

## Local Development & Testing

The core exports `dh_waypoint_dhtms_eligible_documents{tier="base|publication|payload"}`
from the last healthy lease observation. Each tier counts documents that passed
its policy in the sidecar. A lost lease or failed health read removes the samples.
Rebuild the DH-TMS generation after upgrading to 6.1.0 to apply the updated
apostrophe tokenization to previously indexed titles.

```bash
cd dhtms
cargo fmt --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace --release
cargo build --release --bin dh-tmsd
```

To run the Go↔Rust Unix-socket end-to-end test suite from the repository root:

```bash
DH_TMSD_BIN="$PWD/dhtms/target/release/dh-tmsd" go test -race -v ./internal/database/dhtms/...
```

## Container & Companion Compose Service

`dh-tmsd` runs as a dedicated, non-root (`UID 1000:1000`) companion container with zero host ports published, keeping a single writer by construction and sharing an internal Compose network with `dh-waypoint`:

```bash
# Build the companion image locally
docker build -f dhtms/Dockerfile -t dh-waypoint-tms:local dhtms

# Initialize a synthetic fixture generation on the Compose volume and start dh-tmsd
docker compose --profile dhtms run --rm dh-tmsd init-fixture --out /var/lib/dh-tms/db
COMPOSE_PROFILES=dhtms DH_TMS_AUTH_TOKEN=change-me docker compose up -d dh-tmsd
```
