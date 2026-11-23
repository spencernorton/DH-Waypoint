use ahash::{AHashMap, AHashSet};
use dh_tms_core::{MetaRow, StreamingCoreWriter};
use dh_tms_format::catalog::{ContentRecord, MmapContentCatalog};
use dh_tms_format::{publish_generation, stage_generation_dir, CurrentManifest, FormatError};
use dh_tms_grammar::{
    evaluate_serving_adult, extract_trigrams, parse_release_name, tokenize_alnum, EpisodeSpec,
    NamePolicyMatcher, CT_UNKNOWN, CT_XXX,
};
use dh_tms_hashdir::{build_and_save_hash_dir, parse_info_hash_hex, InfoHash};
use dh_tms_policy::PolicyState;
use dh_tms_search::{trigram_key, CustomSearchBuilder};
use dh_tms_swarm::SwarmPlane;
use serde::Deserialize;
use std::io::BufRead;
use std::path::Path;
use std::time::{SystemTime, UNIX_EPOCH};

const MAX_FILE_TOKENS_PER_DOC: usize = 2048;
const MAX_GENERATIONS_TO_KEEP: usize = 2;

#[derive(Debug, Clone, Deserialize)]
pub struct BuildFileInput {
    pub path: String,
    #[serde(default)]
    pub size: u64,
    #[serde(default)]
    pub extension: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct BuildCatalogRecord {
    pub entity_id: u32,
    pub content_type: u8,
    pub source: String,
    pub id: String,
    pub title: String,
    #[serde(default)]
    pub original_title: String,
    #[serde(default)]
    pub year: u16,
    #[serde(default)]
    pub adult: bool,
    #[serde(default)]
    pub alt_identifiers: Vec<(String, String)>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct BuildDocRecord {
    /// 40-hex v1 info_hash (or empty for pure-v2 torrents when info_hash_v2 is set).
    #[serde(default)]
    pub info_hash_v1: String,
    /// Optional 64-hex BEP-52 v2 info_hash (hybrid or pure-v2).
    #[serde(default)]
    pub info_hash_v2: String,
    pub name: String,
    #[serde(default)]
    pub size_bytes: u64,
    #[serde(default = "default_ct")]
    pub content_type: u8,
    #[serde(default)]
    pub entity_id: u32,
    #[serde(default)]
    pub year: u16,
    #[serde(default)]
    pub published_epoch: u32,
    #[serde(default)]
    pub created_epoch: u32,
    #[serde(default)]
    pub resolution: u8,
    #[serde(default)]
    pub files: Vec<BuildFileInput>,
    #[serde(default)]
    pub is_private: bool,
    #[serde(default)]
    pub quarantined: bool,
    #[serde(default)]
    pub privacy_tainted: bool,
    #[serde(default)]
    pub excluded: bool,
    #[serde(default)]
    pub held: bool,
    /// When authoritative_policy is true, adult and name_blocked are taken verbatim from
    /// Go's serving/namepolicy evaluation (S2-2: parity by construction).
    #[serde(default)]
    pub authoritative_policy: bool,
    #[serde(default)]
    pub adult: bool,
    #[serde(default)]
    pub name_blocked: bool,
    #[serde(default)]
    pub pub_approved: bool,
    #[serde(default)]
    pub payload_expires_epoch: u32,
    #[serde(default = "default_neg1")]
    pub seeders: i32,
    #[serde(default = "default_neg1")]
    pub leechers: i32,
    #[serde(default)]
    pub checked_epoch_min: u32,
}

fn default_ct() -> u8 {
    CT_UNKNOWN
}

fn default_neg1() -> i32 {
    -1
}

#[derive(Debug, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum NdjsonLine {
    Header {
        #[serde(default = "default_lsn")]
        pg_lsn: u64,
        #[serde(default = "default_lease_ttl")]
        lease_ttl_ms: u64,
        #[serde(default)]
        pg_lsn_text: String,
    },
    Catalog(BuildCatalogRecord),
    Doc(BuildDocRecord),
}

fn default_lsn() -> u64 {
    1
}

fn default_lease_ttl() -> u64 {
    60_000
}

#[derive(Debug, Clone)]
pub struct BuildOutcome {
    pub manifest: CurrentManifest,
    pub pg_lsn: u64,
    pub lease_ttl_ms: u64,
}

fn now_sec() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(1_800_000_000)
}

/// Initialize a valid empty generation (`generation_id=0`, `num_docs=0`) so `dh-tmsd serve`
/// boots cleanly on a brand-new volume without manual fixture initialization.
pub fn build_empty_generation(out_root: &Path) -> Result<CurrentManifest, FormatError> {
    let gen_id = 0u64;
    let hash_seed = 0xD470_5EED_0000_0000u64;
    let term_seed = 0xD470_7E20_0000_0000u64;
    let now = now_sec();

    std::fs::create_dir_all(out_root)?;
    let staged_dir = stage_generation_dir(out_root, gen_id)?;
    MmapContentCatalog::build_and_save(&staged_dir.join("catalog.bin"), gen_id, &[])?;

    let core_w = StreamingCoreWriter::new(&staged_dir, gen_id, 0, Vec::new())?;
    core_w.finish(&[], &[String::new()])?;
    build_and_save_hash_dir(&staged_dir, gen_id, hash_seed, &[], &[])?;
    PolicyState::new(0).save(&staged_dir, gen_id)?;
    SwarmPlane::create(&staged_dir, gen_id, &[])
        .map_err(|e| FormatError::CorruptPayload(e.to_string()))?;
    let sb = CustomSearchBuilder::new(gen_id, term_seed, 0, &staged_dir, true);
    sb.build_and_save(&staged_dir, 0)?;

    let manifest = CurrentManifest {
        generation_id: gen_id,
        engine_kind: "custom-v0.3".into(),
        num_docs: 0,
        pg_lsn: "0/0".into(),
        created_epoch_sec: now,
        max_created_epoch_sec: now,
        hash_seed,
    };
    publish_generation(out_root, &staged_dir, &manifest)?;
    Ok(manifest)
}

/// Build and atomically publish a generation from an NDJSON stream of header, catalog, and doc lines.
pub fn build_generation_from_ndjson<R: BufRead>(
    reader: R,
    out_root: &Path,
    next_gen_id: u64,
) -> Result<BuildOutcome, FormatError> {
    let mut pg_lsn = 1u64;
    let mut lease_ttl_ms = 60_000u64;
    let mut pg_lsn_text = String::new();
    let mut catalog_recs: Vec<ContentRecord> = Vec::new();
    let mut docs: Vec<BuildDocRecord> = Vec::new();

    for (line_no, line_res) in reader.lines().enumerate() {
        let line = line_res?;
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let parsed: NdjsonLine = serde_json::from_str(trimmed).map_err(|e| {
            FormatError::CorruptPayload(format!("ndjson line {}: {e}", line_no + 1))
        })?;
        match parsed {
            NdjsonLine::Header {
                pg_lsn: lsn,
                lease_ttl_ms: ttl,
                pg_lsn_text: txt,
            } => {
                pg_lsn = lsn;
                lease_ttl_ms = ttl.max(1_000);
                pg_lsn_text = txt;
            }
            NdjsonLine::Catalog(c) => {
                catalog_recs.push(ContentRecord {
                    entity_id: c.entity_id,
                    content_type: c.content_type,
                    source: c.source,
                    id: c.id,
                    title: c.title,
                    original_title: c.original_title,
                    year: c.year,
                    adult: c.adult,
                    strict_title_hashes: vec![],
                    family_hashes: vec![],
                    alt_identifiers: c.alt_identifiers,
                });
            }
            NdjsonLine::Doc(d) => docs.push(d),
        }
    }

    let manifest = build_generation_records(
        out_root,
        next_gen_id,
        if pg_lsn_text.is_empty() {
            format!("0/{pg_lsn:X}")
        } else {
            pg_lsn_text
        },
        &catalog_recs,
        docs,
    )?;

    Ok(BuildOutcome {
        manifest,
        pg_lsn,
        lease_ttl_ms,
    })
}

fn build_generation_records(
    out_root: &Path,
    gen_id: u64,
    pg_lsn_text: String,
    catalog_recs: &[ContentRecord],
    mut docs: Vec<BuildDocRecord>,
) -> Result<CurrentManifest, FormatError> {
    let now = now_sec();
    let hash_seed = 0xD470_5EED_0000_0000u64 ^ gen_id.wrapping_mul(0x9E37_79B9_7F4A_7C15);
    let term_seed = 0xD470_7E20_0000_0000u64 ^ gen_id.wrapping_mul(0xBF58_476D_1CE4_E5B9);

    // Deduplicate by canonical v1/v2 info_hash (last write wins) before physical sort.
    let mut seen_v1: AHashSet<[u8; 20]> = AHashSet::with_capacity(docs.len());
    let mut seen_v2: AHashSet<[u8; 32]> = AHashSet::new();
    let mut parsed_docs: Vec<(BuildDocRecord, [u8; 20], Option<[u8; 32]>)> =
        Vec::with_capacity(docs.len());

    for d in docs.drain(..).rev() {
        let v1_opt = if d.info_hash_v1.is_empty() {
            None
        } else {
            match parse_info_hash_hex(&d.info_hash_v1) {
                Some(InfoHash::V1(b)) => Some(b),
                _ => {
                    return Err(FormatError::CorruptPayload(format!(
                        "invalid info_hash_v1: {}",
                        d.info_hash_v1
                    )))
                }
            }
        };
        let v2_opt = if d.info_hash_v2.is_empty() {
            None
        } else {
            match parse_info_hash_hex(&d.info_hash_v2) {
                Some(InfoHash::V2(b)) => Some(b),
                _ => {
                    return Err(FormatError::CorruptPayload(format!(
                        "invalid info_hash_v2: {}",
                        d.info_hash_v2
                    )))
                }
            }
        };
        if v1_opt.is_none() && v2_opt.is_none() {
            return Err(FormatError::CorruptPayload(
                "doc missing both info_hash_v1 and info_hash_v2".into(),
            ));
        }
        if let Some(v1) = v1_opt {
            if !seen_v1.insert(v1) {
                continue;
            }
        }
        if let Some(v2) = v2_opt {
            if !seen_v2.insert(v2) {
                continue;
            }
        }
        parsed_docs.push((d, v1_opt.unwrap_or([0u8; 20]), v2_opt));
    }

    // Physical order: (content_type ASC, published_epoch DESC, info_hash ASC) for O(1) ct_ranges RSS.
    parsed_docs.sort_by(|a, b| {
        a.0.content_type
            .cmp(&b.0.content_type)
            .then_with(|| b.0.published_epoch.cmp(&a.0.published_epoch))
            .then_with(|| a.1.cmp(&b.1))
    });

    std::fs::create_dir_all(out_root)?;
    let staged_dir = stage_generation_dir(out_root, gen_id)?;
    MmapContentCatalog::build_and_save(&staged_dir.join("catalog.bin"), gen_id, catalog_recs)?;

    let mut entity_tokens: AHashMap<u32, Vec<String>> = AHashMap::with_capacity(catalog_recs.len());
    for c in catalog_recs {
        let mut t = tokenize_alnum(&c.title);
        if !c.original_title.is_empty() {
            t.extend(tokenize_alnum(&c.original_title));
        }
        entity_tokens.insert(c.entity_id, t);
    }

    let num_docs = parsed_docs.len();
    let mut ext_vocab: Vec<String> = vec![String::new()];
    let mut ext_map: AHashMap<String, u16> = AHashMap::new();
    ext_map.insert(String::new(), 0);
    for (doc, _, _) in &parsed_docs {
        for f in &doc.files {
            let ext = f.extension.to_ascii_lowercase();
            if !ext_map.contains_key(&ext) && ext_vocab.len() < u16::MAX as usize {
                let id = ext_vocab.len() as u16;
                ext_vocab.push(ext.clone());
                ext_map.insert(ext, id);
            }
        }
    }

    let mut core_w = StreamingCoreWriter::new(&staged_dir, gen_id, num_docs, Vec::new())?;
    let mut sb = CustomSearchBuilder::new(gen_id, term_seed, num_docs as u32, &staged_dir, true);
    let mut ps = PolicyState::new(num_docs);
    let mut v1_hashes: Vec<[u8; 20]> = Vec::with_capacity(num_docs);
    let mut v2_aliases: Vec<(u32, [u8; 32])> = Vec::new();
    let mut metas: Vec<MetaRow> = Vec::with_capacity(num_docs);
    let mut swarm_init: Vec<(i32, i32, u32)> = Vec::with_capacity(num_docs);
    let mut max_created = 0u64;

    let mut cur_ct: Option<u8> = None;
    for (i, (doc, _, _)) in parsed_docs.iter().enumerate() {
        let ct = doc.content_type.min(11);
        if cur_ct != Some(ct) {
            if let Some(prev) = cur_ct {
                sb.ct_ranges[prev as usize].1 = i as u32;
            }
            sb.ct_ranges[ct as usize].0 = i as u32;
            cur_ct = Some(ct);
        }
    }
    if let Some(prev) = cur_ct {
        sb.ct_ranges[prev as usize].1 = num_docs as u32;
    }

    let np = NamePolicyMatcher::global();
    let mut file_tuples: Vec<(String, u64, String)> = Vec::new();
    let mut file_toks: Vec<String> = Vec::new();

    for (i, (doc, v1, v2_opt)) in parsed_docs.into_iter().enumerate() {
        let d = i as u32;
        let ct = doc.content_type.min(11);
        v1_hashes.push(v1);
        if let Some(v2) = v2_opt {
            v2_aliases.push((d, v2));
        }

        file_tuples.clear();
        file_tuples.reserve(doc.files.len());
        let mut files_sum = 0u64;
        for f in &doc.files {
            files_sum = files_sum.saturating_add(f.size);
            file_tuples.push((f.path.clone(), f.size, f.extension.to_ascii_lowercase()));
        }
        let total_size = if doc.size_bytes > 0 {
            doc.size_bytes
        } else {
            files_sum
        };

        let parsed = parse_release_name(&doc.name);
        let (is_adult, is_name_blocked) = if doc.authoritative_policy {
            (doc.adult, doc.name_blocked)
        } else {
            let ad = evaluate_serving_adult(
                &doc.name,
                total_size,
                "",
                &file_tuples,
                ct == CT_XXX,
                false,
            );
            let nb = np.is_blocked(&doc.name);
            (ad || doc.adult, nb || doc.name_blocked)
        };

        if !doc.is_private {
            ps.public.insert(d);
        }
        if doc.quarantined {
            ps.quarantined.insert(d);
        }
        if doc.privacy_tainted {
            ps.privacy_tainted.insert(d);
        }
        if doc.excluded {
            ps.excluded.insert(d);
        }
        if doc.held {
            ps.held.insert(d);
        }
        if is_adult {
            ps.adult.insert(d);
        }
        if is_name_blocked {
            ps.name_blocked.insert(d);
        }
        if doc.pub_approved {
            ps.pub_approved.insert(d);
        }
        ps.payload_expires[i] = doc.payload_expires_epoch;

        let created_ep = if doc.created_epoch > 0 {
            doc.created_epoch
        } else {
            doc.published_epoch
        };
        max_created = max_created.max(created_ep as u64);

        let res = if doc.resolution != 0 {
            doc.resolution
        } else {
            parsed.resolution
        };
        let meta = MetaRow {
            size_bytes: total_size,
            entity_id: doc.entity_id,
            published_epoch: doc.published_epoch,
            created_epoch: created_ep,
            files_count: file_tuples.len() as u32,
            year: if doc.year != 0 { doc.year } else { parsed.year },
            flags_src: MetaRow::pack_flags(
                parsed.source,
                parsed.codec,
                parsed.is_anime_cue,
                true,
                false,
                parsed.hdr_flags != 0,
                !file_tuples.is_empty(),
            ),
            single_ext_id: 0,
            content_type: ct,
            resolution: res,
        };
        metas.push(meta);
        swarm_init.push((doc.seeders, doc.leechers, doc.checked_epoch_min));
        core_w.push_doc(&doc.name, &file_tuples, &ext_map)?;

        sb.facet_ct[ct as usize].insert(d);
        if (res as usize) < 10 {
            sb.facet_res[res as usize].insert(d);
        }
        if doc.entity_id != 0 {
            sb.facet_linked.insert(d);
            sb.entity_to_docs
                .entry(doc.entity_id)
                .or_default()
                .insert(d);
        }
        if created_ep > 0 {
            sb.created_day_to_docs
                .entry(created_ep / 86_400)
                .or_default()
                .insert(d);
        }
        for &sh in &parsed.strict_title_hashes {
            sb.strict_title_to_docs.entry(sh).or_default().insert(d);
        }
        for &fh in &parsed.family_hashes {
            sb.family_title_to_docs.entry(fh).or_default().insert(d);
        }
        let eps: Vec<EpisodeSpec> = parsed.episodes.clone();
        sb.index_doc_episodes(d, doc.entity_id, &eps);

        let ent_toks: &[String] = entity_tokens
            .get(&doc.entity_id)
            .map(Vec::as_slice)
            .unwrap_or(&[]);
        // Index all file tokens up to a generous per-doc cap so multi-file packs (>12 files)
        // achieve full PG tsv text recall parity without mega-torrent memory blowup.
        file_toks.clear();
        for (fp, _, _) in &file_tuples {
            if file_toks.len() >= MAX_FILE_TOKENS_PER_DOC {
                break;
            }
            for t in tokenize_alnum(fp) {
                file_toks.push(t);
                if file_toks.len() >= MAX_FILE_TOKENS_PER_DOC {
                    break;
                }
            }
        }
        sb.index_doc_terms(
            d,
            &parsed.title_tokens,
            ent_toks,
            &parsed.raw_tokens,
            &file_toks,
        );
        for tri in extract_trigrams(&doc.name) {
            sb.trigram_to_docs
                .entry(trigram_key(tri))
                .or_default()
                .insert(d);
        }
    }

    core_w.finish(&metas, &ext_vocab)?;
    build_and_save_hash_dir(&staged_dir, gen_id, hash_seed, &v1_hashes, &v2_aliases)?;
    ps.save(&staged_dir, gen_id)?;
    SwarmPlane::create(&staged_dir, gen_id, &swarm_init)
        .map_err(|e| FormatError::CorruptPayload(e.to_string()))?;
    sb.build_and_save(&staged_dir, num_docs as u32)?;

    let manifest = CurrentManifest {
        generation_id: gen_id,
        engine_kind: "custom-v0.3".into(),
        num_docs: num_docs as u64,
        pg_lsn: pg_lsn_text,
        created_epoch_sec: now,
        max_created_epoch_sec: if max_created > 0 { max_created } else { now },
        hash_seed,
    };
    publish_generation(out_root, &staged_dir, &manifest)?;
    prune_old_generations(out_root, gen_id);
    Ok(manifest)
}

fn prune_old_generations(out_root: &Path, current_gen: u64) {
    let gens_dir = out_root.join("generations");
    let Ok(entries) = std::fs::read_dir(&gens_dir) else {
        return;
    };
    let mut dirs: Vec<(u64, std::path::PathBuf)> = Vec::new();
    for ent in entries.flatten() {
        let name = ent.file_name();
        let s = name.to_string_lossy();
        if let Some(hex) = s.strip_prefix("gen-") {
            if let Ok(id) = u64::from_str_radix(hex, 16) {
                if id != current_gen {
                    dirs.push((id, ent.path()));
                }
            }
        }
    }
    dirs.sort_by_key(|(id, _)| *id);
    let keep_old = MAX_GENERATIONS_TO_KEEP.saturating_sub(1);
    if dirs.len() > keep_old {
        let drop_n = dirs.len() - keep_old;
        for (_, path) in dirs.into_iter().take(drop_n) {
            let _ = std::fs::remove_dir_all(path);
        }
    }
}
