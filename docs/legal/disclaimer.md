# Legal Disclaimer & Operator Compliance Guide

**This document is for informational purposes only and does not constitute legal advice.** The authors, contributors, and maintainers of DH-Waypoint are not attorneys. Operators are solely responsible for understanding and complying with the copyright, telecommunications, and data-protection laws that apply to their jurisdiction, infrastructure, network configuration, and intended use case.

---

## 1. Software Scope and Capability Architecture

**DH-Waypoint is a self-hosted BitTorrent DHT metadata indexing and cataloging backend.** It is distributed as open-source Go source code and container images under the MIT License:
- **Core DHT Discovery & Metadata Indexing:** In its default configuration, DH-Waypoint participates in the Mainline BitTorrent Distributed Hash Table (`BEP-5` / `BEP-51`), retrieves public `.torrent` info dictionaries (`BEP-9` / `BEP-10`), and stores structured catalog records (infohashes, filenames, file sizes, and swarm cardinality) in a local PostgreSQL database controlled by the operator.
- **No Payload Hosting or File Transfer:** DH-Waypoint does not download, store, seed, cache, or distribute media payloads or content files (`piece` data).
- **Optional Built-In Tracker Subsystem (`docs/private-tracker.md`):** DH-Waypoint includes an optional built-in BitTorrent tracker module that is **disabled by default**. If an operator explicitly enables the built-in tracker, that instance processes tracker announce/scrape requests and maintains active peer session records in accordance with the operator's configuration.
- **Optional Client Evidence Integration (`docs/evidence.md`):** Operators may optionally configure downstream index consumers or download clients to submit grab/import outcome webhooks (`POST /evidence/arr/:instance`) to their local PostgreSQL database.

---

## 2. BitTorrent Protocol and Neutral Dual-Use Technology

The BitTorrent protocol (`BEP-3`, `BEP-5`, `BEP-9`, `BEP-10`, `BEP-15`, `BEP-51`) is a standardized peer-to-peer data distribution technology used to distribute Linux distributions, open-source software packages, Creative Commons media, public-domain archives (such as the Internet Archive, Project Gutenberg, and LibriVox), academic datasets, and commercial software updates.

DH-Waypoint is designed as a general-purpose indexing, classification, and catalog-hygiene engine. It includes built-in pre-fetch child sexual abuse material (CSAM) double-hash blocklists (`docs/csam-defense.md`), executable/malware extension filters, and cryptographic quarantine/tombstone controls.

---

## 3. Copyright Law and Secondary Liability Considerations

Legal treatment of peer-to-peer metadata indexing varies significantly by jurisdiction and depends on how an instance is configured, curated, and exposed:
- **Self-Hosted Private Operation vs. Public Exposure:** Whether operating a metadata index is lawful depends on applicable national and regional copyright frameworks (including 17 U.S.C. § 512 in the United States, Directive (EU) 2019/790 and CJEU jurisprudence on communication to the public in the European Union, and equivalent national statutes).
- **Downstream Retrieval Responsibility:** Indexing metadata is distinct from downloading or redistributing copyrighted works. Any content retrieval performed by an operator's download client is governed by the legal status of the underlying work and whether the operator holds a license or lawful authorization in their jurisdiction.
- **No Legal Immunity:** Deploying open-source indexing software does not confer safe-harbor status or legal immunity against third-party copyright or secondary-liability claims. Operators who expose search APIs beyond a private network or aggregate unauthorized third-party works may face civil or criminal liability depending on local law.

---

## 4. Notice, Takedown, and Local Catalog Removal

**The DH-Waypoint project does not operate a hosted indexing service and cannot access, modify, or remove records from self-hosted operator databases.**
- Each operator controls their own PostgreSQL instance, network exposure, blocklists, and retention rules.
- Catalogue deletion and crawler blocking are disabled in current releases ([catalogue removal](../OPERATIONS.md#catalogue-removal)). Operators who receive a copyright or compliance notice regarding their deployment can withdraw specific infohashes from Torznab and GraphQL results with `NAME_POLICY_ENABLED` and `NAME_POLICY_EXCLUDED_INFO_HASHES` ([configuration](../configuration.md), [release-name eligibility](../RELEASE_NAME_POLICY.md)); the rows stay in the operator's own PostgreSQL database.

---

## 5. Data Protection and Privacy (GDPR / CCPA)

While DH-Waypoint does not phone home or transmit operator telemetry to the project maintainers, **operators must evaluate their own data-protection obligations**:
- **Peer IP Addresses During Metadata Exchange and Scraping:** Participating in the Mainline DHT (`BEP-5`), exchanging `BEP-9` metadata with peers, or enabling the optional built-in tracker (`docs/private-tracker.md`) involves processing peer IP addresses and ports in memory, caches (`peerrep`), or database tables (`tracker_peers` when the optional tracker is enabled). Under European Union law (GDPR Recital 30 and CJEU *Breyer*, Case C-582/14), dynamic IP addresses combined with network timestamps may constitute personal data.
- **Operator Obligations:** Operators who run DH-Waypoint in jurisdictions subject to the GDPR, UK GDPR, CCPA, or similar statutes—particularly if exposing endpoints to third parties—should establish a lawful basis for processing, configure appropriate data-retention and log-rotation intervals, and provide required privacy disclosures.

---

## 6. Software License vs. Lawful Use

The MIT License grants permissions solely with respect to the copyright in the DH-Waypoint source code and binaries. It does not authorize copyright infringement, circumvention of technological protection measures, violation of network terms of service, or non-compliance with data-protection statutes. **The authors and maintainers of DH-Waypoint do not condone, encourage, or support copyright infringement.**
