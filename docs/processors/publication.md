# Required publication review

Publication can require positive policy evidence independently of processor
admission. Set `SERVING_REQUIRED_PROCESSOR` and
`SERVING_REQUIRED_PROCESSOR_ARTIFACT_DIGEST` together. The artifact is a complete
64-character lowercase SHA-256 identity. A startup configuration with a missing
or malformed pin is rejected. Generic installations can leave both unset.

Only a completed, applied policy step from the pinned active processor execution
can approve the current source revision. An application record, canonical match,
classifier result, shadow evaluation, timeout or partial pipeline cannot approve
publication. Existing applications are not grandfathered. Material changes
invalidate approval in the same transaction; restoring an earlier filename or
recrawling a deleted hash does not restore an old approval. Volatile seed counts
and observation timestamps do not invalidate it.

Approval needs file evidence: a size and a complete, consistent file list. While
a processor is required, a torrent without that evidence is not sent to any
processor and stays unpublished. A reprocess request for it ends `held` with the
stored reason `missing_payload_evidence`, `incomplete_file_evidence` or
`inconsistent_file_evidence`. The admission counter counts each refusal under the
single outcome `file_evidence`, a metric label that is never stored as a reason
(see [operational controls](operations.md)). A change to the torrent's size or
file list queues it for review, but a held request stays held: a result bound to
a request needs a new request once the files arrive.

A job found stale when it is prepared, or when its result arrives, returns its
torrent to the review queue at the torrent's current source revision, without
raising it, so a core upgrade does not leave queued torrents waiting for the
catch-up scan. Jobs made stale by a registry or admission change are not
re-queued: that change restarts the catch-up scan instead. Without a required
processor there is neither the file-evidence gate nor the re-queue.

The consumer policy applies before filtering, counts, grouping and pagination to
GraphQL, REST, file queries, Torznab and authenticated public-hash grants. Required
Torznab and grant responses advertise `X-Publication-Review:
required-publication-review-v1` and `Cache-Control: no-store`. Downstream consumers
must check the actual response contract and avoid retaining release lists across
revocation. External clients can retain previously obtained magnets; the server
cannot retract those copies.

## Piece-bound format evidence

For a stronger media profile, also set
`SERVING_REQUIRED_PAYLOAD_VERIFIER_ARTIFACT_DIGEST` and the separate secret
`PROCESSORS_PAYLOAD_VERIFIER_TOKEN`. These require the processor gate. The verifier
is independently operated: its artifact must bind the collector, decoder and
their runtime dependencies. The backend does not run downloaded payloads or ship
a decoder service. Never give a parser the verifier token or operator token.

`POST /api/publication/v1/lease` takes `{"artifact":"<pinned-digest>"}` with the
dedicated bearer credential. A successful response provides a two-minute nonce,
current source projection, required identity claims and strict byte budgets.
No work returns 204. `POST /api/publication/v1/result` must echo the contract,
hash, revision, nonce and verifier artifact. Results are `approved`, `rejected`
or `deferred`, with a bounded reason code. Only `approved` includes `evidence`:
raw bencoded `info`, complete piece bytes, one decoder report for every primary
media file and any required availability claim. Byte arrays are base64 in JSON.
Expired, mismatched, replayed or concurrently invalidated results cannot grant
publication. The operator credential cannot use this purpose-specific API.

The core independently validates exact metainfo identity, file layout, piece
SHA-1 hashes, sample SHA-256 bindings, container signatures and complete report
coverage. Prefix and tail probes are at most 2 MiB each; metainfo is limited to
2 MiB and all complete pieces to 16 MiB. V1 and matching hybrid identities are
supported; pure v2 payload proofs, unknown profiles and excessive budgets remain
held. Executable, script, archive, symlink and unsafe-path media payloads cannot
be approved by a permissive model. Movie/TV evidence additionally binds the
current trusted content ID and every claimed episode's release availability;
day-only dates remain unavailable until that UTC day has ended.

The dedicated verifier must actually decode the bound samples inside a
credential-free, network-disabled, non-root, read-only, resource-limited sandbox.
Container signatures and filename extensions alone are insufficient. The
authenticated report is part of this trust boundary. Format samples and trusted
availability constraints do not prove the identity of depicted content or the
absence of malware in unsampled bytes. Unverifiable releases remain unavailable.

Approved evidence expires after seven days; deferred work becomes due again
after five minutes. A bounded walk re-queues due work only while the release's
publication approval is current, so on a large queue a retry can take a few
minutes longer. Source changes, artifact changes, negative policy and holds take effect
immediately in consumer SQL. `X-Publication-Payload: piece-bound-media-format-v1`
on the actual response confirms that this stronger gate is configured. Missing
headers or upstream failures must fail closed in downstream applications.

## Holds and recovery

The existing operator bearer can inspect
`GET /api/processors/v1/publication/<hash>` and set
`PUT /api/processors/v1/publication/<hash>/hold` with `request_id` (64 lowercase
hex characters), `expected_source_revision`, `held` and a bounded `reason` code.
Exact retries are idempotent; conflicting or stale requests return conflict.
Hold events are append-only. Releasing a hold is not an approval: both applicable
reviews must run again. Private source data is not exposed by these endpoints.

Migrations 73 and 74 add separate evidence tables without changing physical
torrent snapshots. Downgrades refuse to erase retained approvals or hold audits.
Rehearse upgrades on isolated restored state and retain matching backups. Do not
roll back to a binary that bypasses the configured publication boundary.
