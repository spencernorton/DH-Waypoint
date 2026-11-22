use ahash::AHashMap;
use dh_tms_core::{MetaRow, StreamingCoreWriter};
use dh_tms_format::catalog::{ContentRecord, MmapContentCatalog};
use dh_tms_format::{publish_generation, stage_generation_dir, CurrentManifest, FormatError};
use dh_tms_grammar::{
    evaluate_serving_adult, extract_trigrams, parse_release_name, tokenize_alnum, EpisodeSpec,
    NamePolicyMatcher, CT_MOVIE, CT_TV_SHOW, CT_UNKNOWN, CT_XXX,
};
use dh_tms_hashdir::build_and_save_hash_dir;
use dh_tms_policy::PolicyState;
use dh_tms_search::{trigram_key, CustomSearchBuilder};
use dh_tms_swarm::SwarmPlane;
use sha2::{Digest, Sha256};
use std::path::Path;

/// Build a deterministic synthetic generation for unit, contract, and Go<->Rust integration tests.
/// Covers linked movies, linked TV episodes (including S01E02-E04 ranges & whole-season packs),
/// unlinked fallback titles (including accented names & CT_UNKNOWN), BEP-52 v2 hybrid aliases,
/// an 800-file multi-file torrent, adult/name-blocked/quarantined/excluded/held policies,
/// publication approvals, and piece-bound payload proofs with read-time expiry.
pub fn build_synthetic_fixture(out_root: &Path) -> Result<CurrentManifest, FormatError> {
    let gen_id = 1u64;
    let hash_seed = 0xD470_5EED_0000_0001u64;
    let term_seed = 0xD470_7E20_0000_0001u64;
    let base_epoch = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as u32)
        .unwrap_or(1_800_000_000)
        .saturating_sub(3600);

    std::fs::create_dir_all(out_root)?;
    let staged_dir = stage_generation_dir(out_root, gen_id)?;

    let catalog_recs = vec![
        ContentRecord {
            entity_id: 1,
            content_type: CT_MOVIE,
            source: "tmdb".into(),
            id: "101".into(),
            title: "The Usual Suspects".into(),
            original_title: String::new(),
            year: 1995,
            adult: false,
            strict_title_hashes: vec![],
            family_hashes: vec![],
            alt_identifiers: vec![("imdb".into(), "tt0114814".into())],
        },
        ContentRecord {
            entity_id: 2,
            content_type: CT_MOVIE,
            source: "tmdb".into(),
            id: "102".into(),
            title: "Amélie".into(),
            original_title: "Le Fabuleux Destin d'Amélie Poulain".into(),
            year: 2001,
            adult: false,
            strict_title_hashes: vec![],
            family_hashes: vec![],
            alt_identifiers: vec![("imdb".into(), "tt0211915".into())],
        },
        ContentRecord {
            entity_id: 3,
            content_type: CT_TV_SHOW,
            source: "tmdb".into(),
            id: "201".into(),
            title: "Suits".into(),
            original_title: String::new(),
            year: 2011,
            adult: false,
            strict_title_hashes: vec![],
            family_hashes: vec![],
            alt_identifiers: vec![("imdb".into(), "tt1632701".into())],
        },
        ContentRecord {
            entity_id: 4,
            content_type: CT_MOVIE,
            source: "tmdb".into(),
            id: "103".into(),
            title: "Toy Story 2".into(),
            original_title: String::new(),
            year: 1999,
            adult: false,
            strict_title_hashes: vec![],
            family_hashes: vec![],
            alt_identifiers: vec![],
        },
    ];
    MmapContentCatalog::build_and_save(&staged_dir.join("catalog.bin"), gen_id, &catalog_recs)?;

    struct Spec {
        name: String,
        ct: u8,
        entity_id: u32,
        year: u16,
        pub_epoch: u32,
        files: Vec<(String, u64, String)>,
        is_private: bool,
        quarantined: bool,
        excluded: bool,
        held: bool,
        pub_approved: bool,
        payload_exp: u32,
        has_v2_hybrid: bool,
        seeders: i32,
        leechers: i32,
        checked_min: u32,
    }

    let mut specs: Vec<Spec> = vec![
        // 0: Linked movie, pub + payload approved + BEP-52 v2 hybrid alias!
        Spec {
            name: "The.Usual.Suspects.1995.1080p.BluRay.x264-SYNTH".into(),
            ct: CT_MOVIE,
            entity_id: 1,
            year: 1995,
            pub_epoch: base_epoch - 100,
            files: vec![
                (
                    "The.Usual.Suspects.1995.1080p.mkv".into(),
                    9_000_000_000,
                    "mkv".into(),
                ),
                ("Subs/English.srt".into(), 82_000, "srt".into()),
            ],
            is_private: false,
            quarantined: false,
            excluded: false,
            held: false,
            pub_approved: true,
            payload_exp: base_epoch + 86_400 * 10,
            has_v2_hybrid: true,
            seeders: 142,
            leechers: 11,
            checked_min: (base_epoch / 60) - 60,
        },
        // 1: Linked accented movie, pub approved
        Spec {
            name: "Amelie.2001.2160p.WEB-DL.DDP5.1.H265-SYNTH".into(),
            ct: CT_MOVIE,
            entity_id: 2,
            year: 2001,
            pub_epoch: base_epoch - 200,
            files: vec![("Amelie.2001.2160p.mkv".into(), 14_500_000_000, "mkv".into())],
            is_private: false,
            quarantined: false,
            excluded: false,
            held: false,
            pub_approved: true,
            payload_exp: 0,
            has_v2_hybrid: false,
            seeders: 55,
            leechers: 4,
            checked_min: (base_epoch / 60) - 120,
        },
        // 2: Unlinked fallback movie (CT_MOVIE)
        Spec {
            name: "Toy.Story.2.1999.1080p.BluRay.x265-GROUP".into(),
            ct: CT_MOVIE,
            entity_id: 0,
            year: 1999,
            pub_epoch: base_epoch - 300,
            files: vec![("Toy.Story.2.1999.mkv".into(), 4_200_000_000, "mkv".into())],
            is_private: false,
            quarantined: false,
            excluded: false,
            held: false,
            pub_approved: true,
            payload_exp: 0,
            has_v2_hybrid: false,
            seeders: 25,
            leechers: 2,
            checked_min: 0,
        },
        // 3: Unlinked fallback movie (CT_UNKNOWN unclassified!)
        Spec {
            name: "Amelie (2001) 1080p Remux".into(),
            ct: CT_UNKNOWN,
            entity_id: 0,
            year: 2001,
            pub_epoch: base_epoch - 350,
            files: vec![("Amelie.Remux.mkv".into(), 22_000_000_000, "mkv".into())],
            is_private: false,
            quarantined: false,
            excluded: false,
            held: false,
            pub_approved: false,
            payload_exp: 0,
            has_v2_hybrid: false,
            seeders: 10,
            leechers: 1,
            checked_min: 0,
        },
        // 4: Linked TV multi-episode range S01E02-E04, pub + payload approved
        Spec {
            name: "Suits.S01E02-E04.1080p.WEB-DL.x264-SYNTH".into(),
            ct: CT_TV_SHOW,
            entity_id: 3,
            year: 2011,
            pub_epoch: base_epoch - 50,
            files: vec![
                ("Suits.S01E02.mkv".into(), 1_500_000_000, "mkv".into()),
                ("Suits.S01E03.mkv".into(), 1_510_000_000, "mkv".into()),
                ("Suits.S01E04.mkv".into(), 1_520_000_000, "mkv".into()),
            ],
            is_private: false,
            quarantined: false,
            excluded: false,
            held: false,
            pub_approved: true,
            payload_exp: base_epoch + 86_400 * 5,
            has_v2_hybrid: false,
            seeders: 88,
            leechers: 9,
            checked_min: (base_epoch / 60) - 30,
        },
        // 5: Linked TV whole-season pack (800-file pack stress check!)
        Spec {
            name: "Suits.S02.1080p.Complete.Pack-SYNTH".into(),
            ct: CT_TV_SHOW,
            entity_id: 3,
            year: 2011,
            pub_epoch: base_epoch - 150,
            files: (0..800u32)
                .map(|i| {
                    (
                        format!("Season02/ep_{i:04}.mkv"),
                        50_000_000u64,
                        "mkv".into(),
                    )
                })
                .collect(),
            is_private: false,
            quarantined: false,
            excluded: false,
            held: false,
            pub_approved: true,
            payload_exp: 0,
            has_v2_hybrid: false,
            seeders: 44,
            leechers: 3,
            checked_min: 0,
        },
        // 6: Adult strong keyword (MUST be denied in base, pub, payload!)
        Spec {
            name: "FC2-PPV-9876543.1080p.mp4".into(),
            ct: CT_MOVIE,
            entity_id: 0,
            year: 2026,
            pub_epoch: base_epoch - 10,
            files: vec![("FC2-PPV-9876543.mp4".into(), 2_000_000_000, "mp4".into())],
            is_private: false,
            quarantined: false,
            excluded: false,
            held: false,
            pub_approved: true,
            payload_exp: base_epoch + 86_400,
            has_v2_hybrid: false,
            seeders: 10,
            leechers: 1,
            checked_min: 0,
        },
        // 7: Quarantined doc (MUST be denied!)
        Spec {
            name: "The.Usual.Suspects.1995.Quarantined.1080p".into(),
            ct: CT_MOVIE,
            entity_id: 1,
            year: 1995,
            pub_epoch: base_epoch - 20,
            files: vec![("movie.mkv".into(), 1_000_000_000, "mkv".into())],
            is_private: false,
            quarantined: true,
            excluded: false,
            held: false,
            pub_approved: true,
            payload_exp: 0,
            has_v2_hybrid: false,
            seeders: 5,
            leechers: 0,
            checked_min: 0,
        },
        // 8: Excluded doc (MUST be denied!)
        Spec {
            name: "The.Usual.Suspects.1995.Excluded.1080p".into(),
            ct: CT_MOVIE,
            entity_id: 1,
            year: 1995,
            pub_epoch: base_epoch - 25,
            files: vec![("movie.mkv".into(), 1_000_000_000, "mkv".into())],
            is_private: false,
            quarantined: false,
            excluded: true,
            held: false,
            pub_approved: true,
            payload_exp: 0,
            has_v2_hybrid: false,
            seeders: 5,
            leechers: 0,
            checked_min: 0,
        },
        // 9: Held doc (MUST be denied!)
        Spec {
            name: "Suits.S01E03.Held.1080p".into(),
            ct: CT_TV_SHOW,
            entity_id: 3,
            year: 2011,
            pub_epoch: base_epoch - 30,
            files: vec![("ep.mkv".into(), 1_000_000_000, "mkv".into())],
            is_private: false,
            quarantined: false,
            excluded: false,
            held: true,
            pub_approved: true,
            payload_exp: 0,
            has_v2_hybrid: false,
            seeders: 5,
            leechers: 0,
            checked_min: 0,
        },
    ];

    // Add 90 synthetic background movies & TV shows for pagination / RSS / stale selection testing
    for i in 0..90u32 {
        let is_tv = i % 2 == 0;
        let ct = if is_tv { CT_TV_SHOW } else { CT_MOVIE };
        let name = if is_tv {
            format!(
                "Synthetic.Series.{i:03}.S01E{:02}.1080p.WEB-DL-SYNTH",
                (i % 12) + 1
            )
        } else {
            format!(
                "Synthetic.Feature.{i:03}.{}.1080p.BluRay-SYNTH",
                2000 + (i % 25)
            )
        };
        specs.push(Spec {
            name,
            ct,
            entity_id: 0,
            year: (2000 + (i % 25)) as u16,
            pub_epoch: base_epoch - 1000 - i * 60,
            files: vec![("video.mkv".into(), 2_000_000_000, "mkv".into())],
            is_private: false,
            quarantined: false,
            excluded: false,
            held: false,
            pub_approved: i % 3 == 0,
            payload_exp: 0,
            has_v2_hybrid: false,
            seeders: (i % 50) as i32,
            leechers: (i % 10) as i32,
            checked_min: if i % 4 == 0 {
                0
            } else {
                (base_epoch / 60) - 500 - i
            },
        });
    }

    // Sort strictly by (content_type ASC, published_epoch DESC) so ct_ranges hold!
    specs.sort_by(|a, b| a.ct.cmp(&b.ct).then_with(|| b.pub_epoch.cmp(&a.pub_epoch)));

    let num_docs = specs.len();
    let ext_vocab = vec![String::new(), "mkv".into(), "mp4".into(), "srt".into()];
    let mut ext_map: AHashMap<String, u16> = AHashMap::new();
    for (idx, ext) in ext_vocab.iter().enumerate() {
        ext_map.insert(ext.clone(), idx as u16);
    }

    let mut core_w = StreamingCoreWriter::new(&staged_dir, gen_id, num_docs, Vec::new())?;
    let mut sb = CustomSearchBuilder::new(gen_id, term_seed, num_docs as u32, &staged_dir, true);
    let mut ps = PolicyState::new(num_docs);
    let mut v1_hashes: Vec<[u8; 20]> = Vec::with_capacity(num_docs);
    let mut v2_aliases: Vec<(u32, [u8; 32])> = Vec::new();
    let mut metas: Vec<MetaRow> = Vec::with_capacity(num_docs);
    let mut swarm_init: Vec<(i32, i32, u32)> = Vec::with_capacity(num_docs);

    // Compute ct_ranges
    let mut cur_ct = None;
    for (i, sp) in specs.iter().enumerate() {
        if cur_ct != Some(sp.ct) {
            if let Some(prev) = cur_ct {
                sb.ct_ranges[prev as usize].1 = i as u32;
            }
            sb.ct_ranges[sp.ct as usize].0 = i as u32;
            cur_ct = Some(sp.ct);
        }
    }
    if let Some(prev) = cur_ct {
        sb.ct_ranges[prev as usize].1 = num_docs as u32;
    }

    let np = NamePolicyMatcher::global();
    for (i, sp) in specs.iter().enumerate() {
        let d = i as u32;
        let mut h = Sha256::new();
        h.update(b"dh-tms-fixture-v1:");
        h.update(sp.name.as_bytes());
        h.update(d.to_le_bytes());
        let dig = h.finalize();
        let mut v1 = [0u8; 20];
        v1.copy_from_slice(&dig[0..20]);
        v1_hashes.push(v1);

        if sp.has_v2_hybrid {
            let mut h2 = Sha256::new();
            h2.update(b"dh-tms-fixture-v2:");
            h2.update(v1);
            let d2 = h2.finalize();
            let mut v2 = [0u8; 32];
            v2.copy_from_slice(&d2[0..32]);
            v2_aliases.push((d, v2));
        }

        let parsed = parse_release_name(&sp.name);
        let total_size: u64 = sp.files.iter().map(|(_, sz, _)| *sz).sum();
        let is_adult =
            evaluate_serving_adult(&sp.name, total_size, "", &sp.files, sp.ct == CT_XXX, false);
        let is_name_blocked = np.is_blocked(&sp.name);

        if !sp.is_private {
            ps.public.insert(d);
        }
        if sp.quarantined {
            ps.quarantined.insert(d);
        }
        if sp.excluded {
            ps.excluded.insert(d);
        }
        if sp.held {
            ps.held.insert(d);
        }
        if is_adult {
            ps.adult.insert(d);
        }
        if is_name_blocked {
            ps.name_blocked.insert(d);
        }
        if sp.pub_approved {
            ps.pub_approved.insert(d);
        }
        ps.payload_expires[i] = sp.payload_exp;

        let meta = MetaRow {
            size_bytes: total_size,
            entity_id: sp.entity_id,
            published_epoch: sp.pub_epoch,
            created_epoch: sp.pub_epoch,
            files_count: sp.files.len() as u32,
            year: if parsed.year != 0 {
                parsed.year
            } else {
                sp.year
            },
            flags_src: MetaRow::pack_flags(
                parsed.source,
                parsed.codec,
                parsed.is_anime_cue,
                true,
                false,
                parsed.hdr_flags != 0,
                !sp.files.is_empty(),
            ),
            single_ext_id: 0,
            content_type: sp.ct,
            resolution: parsed.resolution,
        };
        metas.push(meta);
        swarm_init.push((sp.seeders, sp.leechers, sp.checked_min));
        core_w.push_doc(&sp.name, &sp.files, &ext_map)?;

        sb.facet_ct[sp.ct as usize].insert(d);
        if (meta.resolution as usize) < 10 {
            sb.facet_res[meta.resolution as usize].insert(d);
        }
        if sp.entity_id != 0 {
            sb.facet_linked.insert(d);
            sb.entity_to_docs.entry(sp.entity_id).or_default().insert(d);
        }
        sb.created_day_to_docs
            .entry(sp.pub_epoch / 86_400)
            .or_default()
            .insert(d);
        for &sh in &parsed.strict_title_hashes {
            sb.strict_title_to_docs.entry(sh).or_default().insert(d);
        }
        for &fh in &parsed.family_hashes {
            sb.family_title_to_docs.entry(fh).or_default().insert(d);
        }
        let eps: Vec<EpisodeSpec> = parsed.episodes.clone();
        sb.index_doc_episodes(d, sp.entity_id, &eps);

        let ent_toks: Vec<String> = catalog_recs
            .iter()
            .find(|c| c.entity_id == sp.entity_id)
            .map(|c| {
                let mut t = tokenize_alnum(&c.title);
                if !c.original_title.is_empty() {
                    t.extend(tokenize_alnum(&c.original_title));
                }
                t
            })
            .unwrap_or_default();
        let mut file_toks = Vec::new();
        for (fp, _, _) in sp.files.iter().take(64) {
            file_toks.extend(tokenize_alnum(fp));
        }
        sb.index_doc_terms(
            d,
            &parsed.title_tokens,
            &ent_toks,
            &parsed.raw_tokens,
            &file_toks,
        );
        for tri in extract_trigrams(&sp.name) {
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
        pg_lsn: "0/1000".into(),
        created_epoch_sec: base_epoch as u64,
        max_created_epoch_sec: base_epoch as u64,
        hash_seed,
    };
    publish_generation(out_root, &staged_dir, &manifest)?;
    Ok(manifest)
}
