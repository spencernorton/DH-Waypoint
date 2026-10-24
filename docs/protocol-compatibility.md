# BitTorrent applicability and interoperability ledger

Assessed October 24, 2026. This ledger describes the active metadata crawler and
its public import/tracker paths. A wire field or a dependency's capability does
not establish end-to-end support. Tests use synthetic metadata and local peers;
they do not measure public-swarm coverage or crawl yield.

| Specification / feature | Current applicability and implementation | Interoperability evidence and remaining limits |
|---|---|---|
| [BEP 3: v1 metainfo and handshake](https://www.bittorrent.org/beps/bep_0003.html) | Core metadata indexing; `internal/protocol/metainfo` and its requester. | SHA-1 checks, bounded metadata/path validation and synthetic import tests. `TestRequesterFetchesMetadataOverBothAddressFamilies` exercises a complete handshake and verified metadata fetch. No payload-download qualification. |
| [BEP 5: DHT](https://www.bittorrent.org/beps/bep_0005.html) | Active IPv4 discovery; `internal/protocol/dht/{server,client,responder}` and `internal/dhtcrawler`. | Message round trips and responder tests cover ping, find_node, get_peers and announce_peer. Full dual-stack participation remains incomplete. |
| [BEP 9: metadata exchange](https://www.bittorrent.org/beps/bep_0009.html) | Essential; requester negotiates `ut_metadata`, enforces metadata/frame bounds and checks piece sizes, duplicates and final info hash. | `requester_test.go` and the local-peer transport fixture cover ordered/reordered pieces, invalid framing, conflicting duplicates and a non-default remote extension ID. Unknown metadata message types currently cause rejection; BEP 9 requires ignoring them. No claim of complete extension conformance. |
| [BEP 10: extension protocol](https://www.bittorrent.org/beps/bep_0010.html) | Active extension handshake and metadata exchange. | Local-peer fixture advertises remote `ut_metadata=7` while the requester advertises its own ID 1. Additional extension-handshake updates and the entire dynamic-ID range still need conformance coverage. |
| [BEP 11: peer exchange](https://www.bittorrent.org/beps/bep_0011.html) | Potential additional metadata-peer discovery. | No active `ut_pex` negotiation/ingestion found. Any future path needs bounded candidate admission, IPv4/IPv6 fixtures and existing source/connection limits. |
| [BEP 23: compact tracker peers](https://www.bittorrent.org/beps/bep_0023.html) | Optional HTTP tracker; `internal/tracker`. | Compact-peer encoding and announce-response tests. This does not establish UDP tracker service support. |
| [BEP 27: private torrents](https://www.bittorrent.org/beps/bep_0027.html) | Optional private-file/tracker path; private torrents are excluded from the public serving/processor view. | `internal/tracker` creation/download tests and REST privacy integration tests. Private-swarm policy is independent of adding a public peer transport. |
| [BEP 29: uTP](https://www.bittorrent.org/beps/bep_0029.html) | Valuable next transport experiment for metadata-peer reachability. | The active requester uses TCP; a torrent dependency alone provides no active uTP path. Requires a bounded UDP transport, connection/deadline accounting and tests against a real uTP peer before advertising support. |
| [BEP 32: IPv6 DHT](https://www.bittorrent.org/beps/bep_0032.html) | Partial: peer fetching accepts IPv6 literals; full IPv6 DHT remains a priority. | Loopback IPv4, IPv6 and mapped-IPv4 metadata fetch fixtures. DHT server factory still binds IPv4; external-IP resolution is IPv4-only. `nodes6`/`want` fields do not constitute an IPv6 routing table or working bootstrap. |
| [BEP 33: DHT scrape](https://www.bittorrent.org/beps/bep_0033.html) | Existing approximate swarm observations; `internal/protocol/dht/scrape.go`. | Bloom-filter and empty-estimate tests in `msg_test.go`. Estimates describe swarm observations, not verified current seed counts. |
| [BEP 42: DHT node IDs](https://www.bittorrent.org/beps/bep_0042.html) | Existing node-security derivation and external-IP watcher. | Reference vectors, round trips, wrong-IP rejection and watcher tests. IPv6 derivation helpers are insufficient for operational dual-stack identity/routing. |
| [BEP 43: read-only DHT nodes](https://www.bittorrent.org/beps/bep_0043.html) | Active routing admission protection. | `node_admission_test.go` and responder node-discovery tests reject read-only node admission. |
| [BEP 44: mutable/immutable DHT data](https://www.bittorrent.org/beps/bep_0044.html), [BEP 46: updating torrents](https://www.bittorrent.org/beps/bep_0046.html) | Optional feed/data features outside the immediate metadata-indexing gap. | Generic message fields exist; no complete data/feed workflow is claimed. Keep lower priority than peer reachability and metadata integrity. |
| [BEP 51: sample_infohashes](https://www.bittorrent.org/beps/bep_0051.html) | Active crawler indexing mechanism. | Sample message/responder tests; `TestNextSampleInterval_honoursPeerAdvertisedInterval` retains the remote interval. Do not shorten that interval to increase yield. |
| [BEP 52: v2/hybrid identities](https://www.bittorrent.org/beps/bep_0052.html) | Dedicated v2 workstream; separate full SHA-256 torrent identity from 20-byte node/peer IDs and wire keys. | Track the [v2 foundation](https://github.com/spencernorton/DH-Waypoint/pull/7) and its integration tests. Foundation parsing is distinct from complete acquisition, persistence and consumer compatibility. This transport change does not implement or qualify v2 indexing. |
| [BEP 55: holepunch](https://www.bittorrent.org/beps/bep_0055.html) | Conditional follow-up after uTP and useful peer introductions. | No active `ut_holepunch` path. The extension depends on BEP 10 and uTP; it needs a cooperating rendezvous peer and simultaneous-connect fixtures. |
| [MSE/PE](https://libtorrent.org/reference-Settings.html#out_enc_policy) | Compatibility with peers requiring encrypted negotiation. | Active requester uses a plaintext handshake. Any adapter/fallback must share the overall connection, CPU and time budgets. This is not an anonymity guarantee or modern authenticated end-to-end encryption. |

## Priority and acceptance

1. **Retain the independent transport slice:** literal IPv4/IPv6 TCP dialing,
   cancellation during active socket I/O, existing absolute deadlines and all
   metadata validation. The requester signature and wire-ID width stay intact.
2. **Complete IPv6 DHT as a coherent change:** separate address-family routing
   tables and bootstrap, `want`/`nodes6` handling, compact peer parsing, family
   selection, 1,024-byte outgoing DHT packet bounds, external-address/node-ID
   handling and cancellation/resource tests. Qualify on an IPv6-capable network;
   loopback peer success alone does not meet this gate.
3. **Evaluate uTP and MSE/PE with measured compatibility fixtures:** success and
   failure against TCP-only, uTP-only, encryption-required and rejecting peers;
   one overall timeout and concurrency budget across attempts; cancellation and
   malformed-handshake tests. Prefer an audited library adapter to a new
   transport implementation. Do not advertise dependency-only support or
   automatic fallback until those paths are exercised.
4. **Add PEX/holepunch only when useful:** verify candidate-source provenance,
   bounded fan-out, negotiated extension IDs and private-torrent policy. uTP
   and an actual introduction path precede a holepunch support claim.

To repeat the existing synthetic coverage:

```sh
go test -race -count=1 ./internal/protocol/... ./internal/dhtcrawler ./internal/tracker ./internal/restapi
```

Database cases require the documented disposable PostgreSQL DSN. The IPv6 peer
test reports an explicit skip if the host cannot bind IPv6 loopback; that skip
is a coverage gap, never a successful IPv6 result. Public-network qualification
and transport-yield measurements remain separate acceptance work.
