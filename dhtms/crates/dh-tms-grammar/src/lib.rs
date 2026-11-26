pub mod policy_matchers;

use regex::Regex;
use std::collections::BTreeSet;
use std::sync::OnceLock;
use unicode_normalization::UnicodeNormalization;
use xxhash_rust::xxh3::xxh3_64_with_seed;

pub use policy_matchers::{
    evaluate_serving_adult, is_privacy_tag, is_private_canonical_label, is_qb_privacy,
    is_serving_media_ext, strong_adult_regex, NamePolicyMatcher,
};

const TITLE_HASH_SEED: u64 = 0x9E37_79B9_7F4A_7C15;

// Content types matching DH-Waypoint
pub const CT_UNKNOWN: u8 = 0;
pub const CT_MOVIE: u8 = 1;
pub const CT_TV_SHOW: u8 = 2;
pub const CT_MUSIC: u8 = 3;
pub const CT_EBOOK: u8 = 4;
pub const CT_COMIC: u8 = 5;
pub const CT_AUDIOBOOK: u8 = 6;
pub const CT_GAME: u8 = 7;
pub const CT_SOFTWARE: u8 = 8;
pub const CT_XXX: u8 = 9;
pub const CT_COURSE: u8 = 10;
pub const NUM_CONTENT_TYPES: usize = 11;

pub fn content_type_from_str(s: &str) -> u8 {
    match s {
        "movie" => CT_MOVIE,
        "tv_show" => CT_TV_SHOW,
        "music" => CT_MUSIC,
        "ebook" => CT_EBOOK,
        "comic" => CT_COMIC,
        "audiobook" => CT_AUDIOBOOK,
        "game" => CT_GAME,
        "software" => CT_SOFTWARE,
        "xxx" => CT_XXX,
        "course" => CT_COURSE,
        _ => CT_UNKNOWN,
    }
}

pub fn content_type_to_str(ct: u8) -> &'static str {
    match ct {
        CT_MOVIE => "movie",
        CT_TV_SHOW => "tv_show",
        CT_MUSIC => "music",
        CT_EBOOK => "ebook",
        CT_COMIC => "comic",
        CT_AUDIOBOOK => "audiobook",
        CT_GAME => "game",
        CT_SOFTWARE => "software",
        CT_XXX => "xxx",
        CT_COURSE => "course",
        _ => "unknown",
    }
}

/// Structured episode representation avoiding any `0xFFFF` u16 sentinel collision.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum EpisodeSpec {
    WholeSeason(u16),
    Episode(u16, u16),
}

/// Collision-free 64-bit episode bitmap keys with explicit namespace tags.
/// Supports arbitrary episode numbers (no 0..=200 cap) and per-entity keying.
#[inline]
pub fn ep_key_entity_episode(entity_id: u32, season: u16, episode: u16) -> u64 {
    (1u64 << 60)
        | (((entity_id as u64) & 0x0FFF_FFFF) << 32)
        | ((season as u64) << 16)
        | (episode as u64)
}

#[inline]
pub fn ep_key_entity_whole_season(entity_id: u32, season: u16) -> u64 {
    (2u64 << 60) | (((entity_id as u64) & 0x0FFF_FFFF) << 32) | (season as u64)
}

#[inline]
pub fn ep_key_entity_any_season(entity_id: u32, season: u16) -> u64 {
    (3u64 << 60) | (((entity_id as u64) & 0x0FFF_FFFF) << 32) | (season as u64)
}

#[inline]
pub fn ep_key_global_episode(season: u16, episode: u16) -> u64 {
    (4u64 << 60) | ((season as u64) << 16) | (episode as u64)
}

#[inline]
pub fn ep_key_global_whole_season(season: u16) -> u64 {
    (5u64 << 60) | (season as u64)
}

#[inline]
pub fn ep_key_global_any_season(season: u16) -> u64 {
    (6u64 << 60) | (season as u64)
}

#[derive(Debug, Clone, Default)]
pub struct ParsedRelease {
    pub title_tokens: Vec<String>,
    pub raw_tokens: Vec<String>,
    /// Primary and secondary (e.g. year-included for in-title years like "Wonder Woman 1984") strict hashes.
    pub strict_title_hashes: Vec<u64>,
    pub family_hashes: Vec<u64>,
    pub year: u16,
    pub episodes: Vec<EpisodeSpec>,
    pub resolution: u8,
    pub codec: u8,
    pub source: u8,
    pub hdr_flags: u8,
    pub audio_flags: u8,
    pub release_group: String,
    pub is_anime_cue: bool,
    pub has_exec_trap: bool,
}

pub fn resolution_from_exact_token(tok: &str) -> u8 {
    match tok.to_ascii_lowercase().as_str() {
        "v360p" | "360p" => 1,
        "v480p" | "480p" => 2,
        "v540p" | "540p" => 3,
        "v576p" | "576p" => 4,
        "v720p" | "720p" => 5,
        "v1080p" | "1080p" => 6,
        "v1440p" | "1440p" => 7,
        "v2160p" | "2160p" | "4k" | "uhd" => 8,
        "v4320p" | "4320p" | "8k" => 9,
        _ => 0,
    }
}

pub fn codec_from_exact_token(tok: &str) -> u8 {
    match tok.to_ascii_lowercase().as_str() {
        "x264" | "h264" | "h.264" | "avc" | "x264-hi10p" => 1,
        "x265" | "h265" | "h.265" | "hevc" => 2,
        "av1" => 3,
        "xvid" | "divx" => 4,
        "vp9" => 5,
        "mpeg2" | "mpeg4" | "mpeg" => 6,
        _ => 0,
    }
}

/// Exact-token source matching ONLY — eliminates substring bugs ("The Usual Suspects",
/// "The Great Gatsby", "Suits", "Ghosts", "Cats") and keeps WEB-DL distinct from WEBRip.
pub fn source_from_exact_token(tok: &str) -> u8 {
    match tok.to_ascii_lowercase().as_str() {
        "bluray" | "blu-ray" | "bdrip" | "brrip" | "bdremux" | "remux" | "bdr" => 1,
        "web-dl" | "webdl" | "web_dl" | "web.dl" => 2,
        "webrip" | "web-rip" | "web_rip" => 3,
        "hdtv" | "pdtv" | "dsr" | "sdtv" | "tvrip" => 4,
        "dvdrip" | "dvd-rip" | "dvdr" | "dvd" | "dvdscr" => 5,
        "cam" | "camrip" | "hdcam" | "ts" | "hdts" | "telesync" | "tc" | "telecine"
        | "screener" | "scr" => 6,
        _ => 0,
    }
}

pub fn is_exec_extension(ext: &str) -> bool {
    matches!(
        ext.to_ascii_lowercase().as_str(),
        "exe"
            | "msi"
            | "bat"
            | "cmd"
            | "scr"
            | "pif"
            | "com"
            | "vbs"
            | "ps1"
            | "lnk"
            | "hta"
            | "jar"
            | "apk"
    )
}

#[inline]
fn is_combining_mark(ch: char) -> bool {
    let cp = ch as u32;
    matches!(
        cp,
        0x0300..=0x036F | 0x1AB0..=0x1AFF | 0x1DC0..=0x1DFF | 0x20D0..=0x20FF | 0xFE20..=0xFE2F
    )
}

/// NFKD accent folding + Latin extended transliteration (matching `go-unidecode` on Latin diacritics:
/// "Amélie" -> "amelie", "Śląsk" -> "slask", "Øresund" -> "oresund", "Straße" -> "strasse").
pub fn fold_accents_lowercase(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for ch in s.chars() {
        match ch {
            'ł' | 'Ł' => out.push('l'),
            'ø' | 'Ø' => out.push('o'),
            'đ' | 'Đ' | 'ð' | 'Ð' => out.push('d'),
            'æ' | 'Æ' => out.push_str("ae"),
            'œ' | 'Œ' => out.push_str("oe"),
            'ß' => out.push_str("ss"),
            'þ' | 'Þ' => out.push_str("th"),
            _ => {
                for nch in ch.nfkd() {
                    if !is_combining_mark(nch) {
                        for lc in nch.to_lowercase() {
                            out.push(lc);
                        }
                    }
                }
            }
        }
    }
    out
}

/// Shared accent-folded alphanumeric tokenizer used identically by ingest AND query.
/// Keeps ordinary apostrophized names together, separates elided French
/// articles before un/une, and expands `&` to `"and"`.
pub fn tokenize_alnum(s: &str) -> Vec<String> {
    let folded = fold_accents_lowercase(s);
    let mut out = Vec::with_capacity(12);
    let mut cur = String::with_capacity(16);
    let mut chars = folded.chars();
    while let Some(ch) = chars.next() {
        if ch.is_alphanumeric() {
            cur.push(ch);
        } else if matches!(ch, '\'' | '\u{2019}' | '`') {
            if matches!(cur.as_str(), "d" | "l") {
                let next: String = chars
                    .clone()
                    .take_while(|c| c.is_alphanumeric())
                    .take(4)
                    .collect();
                if matches!(next.as_str(), "un" | "une") {
                    out.push(std::mem::take(&mut cur));
                }
            }
            continue;
        } else if ch == '&' {
            if !cur.is_empty() {
                out.push(std::mem::take(&mut cur));
            }
            out.push("and".to_string());
        } else if !cur.is_empty() {
            out.push(std::mem::take(&mut cur));
        }
    }
    if !cur.is_empty() {
        out.push(cur);
    }
    out
}

fn is_leading_article(tok: &str) -> bool {
    matches!(tok, "the" | "a" | "an" | "le" | "la" | "les" | "el" | "il")
}

fn fold_roman_exact(tok: &str) -> Option<&'static str> {
    match tok {
        "i" => Some("1"),
        "ii" => Some("2"),
        "iii" => Some("3"),
        "iv" => Some("4"),
        "v" => Some("5"),
        "vi" => Some("6"),
        "vii" => Some("7"),
        "viii" => Some("8"),
        "ix" => Some("9"),
        "x" => Some("10"),
        "xi" => Some("11"),
        "xii" => Some("12"),
        "xiii" => Some("13"),
        "xiv" => Some("14"),
        "xv" => Some("15"),
        "xvi" => Some("16"),
        "xvii" => Some("17"),
        "xviii" => Some("18"),
        "xix" => Some("19"),
        "xx" => Some("20"),
        _ => None,
    }
}

fn part_number_word(tok: &str) -> Option<&'static str> {
    match tok {
        "one" => Some("1"),
        "two" => Some("2"),
        "three" => Some("3"),
        "four" => Some("4"),
        "five" => Some("5"),
        "six" => Some("6"),
        "seven" => Some("7"),
        "eight" => Some("8"),
        "nine" => Some("9"),
        "ten" => Some("10"),
        _ => None,
    }
}

fn is_part_number(tok: &str) -> bool {
    !tok.is_empty()
        && (tok.bytes().all(|b| b.is_ascii_digit())
            || fold_roman_exact(tok).is_some()
            || part_number_word(tok).is_some())
}

/// Normalize tokens matching `internal/titlenorm/titlenorm.go` (`NormalizeTitleForMatch` / `strictTitleKey` / `FamilyKey`).
/// Returns `(strict_key, family_key)` where `strict_key` preserves leading articles and `family_key` strips one leading article when >1 token.
pub fn normalize_title_keys(raw_tokens: &[String]) -> (String, String) {
    if raw_tokens.is_empty() {
        return (String::new(), String::new());
    }
    let mut norm: Vec<String> = raw_tokens.to_vec();
    let mut i = 0usize;
    while i < norm.len() {
        if norm[i] == "pt" && i + 1 < norm.len() && is_part_number(&norm[i + 1]) {
            norm[i] = "part".to_string();
        }
        if norm[i] == "part" && i + 1 < norm.len() {
            if let Some(ar) = fold_roman_exact(&norm[i + 1]) {
                norm[i + 1] = ar.to_string();
            } else if let Some(ar) = part_number_word(&norm[i + 1]) {
                norm[i + 1] = ar.to_string();
            }
            i += 2;
            continue;
        }
        if let Some(ar) = fold_roman_exact(&norm[i]) {
            norm[i] = ar.to_string();
        }
        i += 1;
    }
    let strict = norm.join(" ");
    let family = if norm.len() > 1 && is_leading_article(&norm[0]) {
        norm[1..].join(" ")
    } else {
        strict.clone()
    };
    (strict, family)
}

#[inline]
pub fn hash_title_key(key: &str) -> u64 {
    if key.is_empty() {
        0
    } else {
        xxh3_64_with_seed(key.as_bytes(), TITLE_HASH_SEED)
    }
}

/// Compute `(strict_title_hash, family_hash)` directly from any raw title string (catalog or query).
pub fn title_hashes_from_str(title: &str) -> (u64, u64) {
    let toks = tokenize_alnum(title);
    title_hashes_from_tokens(&toks)
}

pub fn title_hashes_from_tokens(tokens: &[String]) -> (u64, u64) {
    let (sk, fk) = normalize_title_keys(tokens);
    (hash_title_key(&sk), hash_title_key(&fk))
}

/// Only unambiguous release-tech stop tokens (NO sequel numbers `0/1/2/5/7`, NO words like `cut`/`complete`/`web` alone!).
fn is_unambiguous_tech_stop(tok: &str) -> bool {
    resolution_from_exact_token(tok) != 0
        || codec_from_exact_token(tok) != 0
        || source_from_exact_token(tok) != 0
        || matches!(
            tok,
            "10bit"
                | "8bit"
                | "hdr"
                | "hdr10"
                | "hdr10plus"
                | "dv"
                | "dovi"
                | "sdr"
                | "atmos"
                | "truehd"
                | "dtshd"
                | "dts"
                | "ddp"
                | "ddp5"
                | "dd5"
                | "eac3"
                | "ac3"
                | "aac"
                | "aac2"
                | "flac"
                | "opus"
                | "ch51"
                | "ch71"
                | "ch20"
                | "proper"
                | "repack"
                | "rerip"
                | "remastered"
                | "unrated"
                | "imax"
                | "dual"
                | "multi"
                | "hdrip"
                | "webdl"
                | "webrip"
                | "bdrip"
                | "brrip"
                | "dvdrip"
                | "hdtv"
                | "nf"
                | "amzn"
                | "dsnp"
                | "atvp"
                | "hmax"
                | "mkv"
                | "mp4"
                | "avi"
        )
}

fn is_audio_codec_tok(tok: &str) -> bool {
    matches!(
        tok,
        "aac"
            | "ac3"
            | "eac3"
            | "dd"
            | "ddp"
            | "dts"
            | "dtshd"
            | "truehd"
            | "atmos"
            | "flac"
            | "opus"
    )
}

fn site_prefix_regex() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(r"(?i)^\s*(?:www\.)?[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)*\.(?:com|net|org|info|biz|me|tv|cc|to|io|xyz|club|se|eu|nz|link|app|us|uk|co|rsvp|pics|la|pl|vip|red|win|pro|site|live|online|top|fun|icu|cyou|ws|ru|in|ph|ai|gg|cx|sh|st|mov|fyi|lol|day|wtf|autos|skin)\b(?:[\s._]*-+[\s._]*|\s{2,})").unwrap()
    })
}

fn cjk_bracket_regex() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"【[^】]*】").unwrap())
}

/// Pre-normalizes compound hyphenated/dotted tags & episode ranges BEFORE splitting punctuation.
fn prenormalize_compounds(s: &str) -> String {
    static RE_COMPOUNDS: OnceLock<Vec<(Regex, &'static str)>> = OnceLock::new();
    let rules = RE_COMPOUNDS.get_or_init(|| {
        vec![
            (Regex::new(r"(?i)\bweb[\s._-]?dl\b").unwrap(), " webdl "),
            (Regex::new(r"(?i)\bweb[\s._-]?rip\b").unwrap(), " webrip "),
            (Regex::new(r"(?i)\bblu[\s._-]?ray\b").unwrap(), " bluray "),
            (Regex::new(r"(?i)\bdts[\s._-]?hd\b").unwrap(), " dtshd "),
            (Regex::new(r"(?i)\bhd[\s._-]?cam\b").unwrap(), " hdcam "),
            (Regex::new(r"(?i)\bdvd[\s._-]?rip\b").unwrap(), " dvdrip "),
            (Regex::new(r"(?i)\bh\.264\b|\bh-264\b").unwrap(), " h264 "),
            (Regex::new(r"(?i)\bh\.265\b|\bh-265\b").unwrap(), " h265 "),
            (
                Regex::new(r"(?i)\b(?:ddp|dd|aac|ac3|eac3|dts)[\s._-]?(5\.1|7\.1|2\.0)\b").unwrap(),
                " ddp ch51 ",
            ),
            (
                Regex::new(r"(?i)(?:^|[\s._\[(\-])(5\.1|7\.1|2\.0)(?:[\s._\])\-]|$)").unwrap(),
                " ch51 ",
            ),
        ]
    });
    let mut cur = s.to_string();
    for (re, rep) in rules {
        cur = re.replace_all(&cur, *rep).into_owned();
    }
    cur
}

/// Extract structured episode specifications (including ranges `S01E02-E04`, `S01E02-04`,
/// multi-ep `S01E02E03`, `1x02-04`, `Season 1`, bare `S01`) and return the byte offset
/// of the earliest episode marker for title anchoring.
fn extract_episodes_from_name(work: &str) -> (Vec<EpisodeSpec>, Option<usize>) {
    static RE_SE_RANGE: OnceLock<Regex> = OnceLock::new();
    static RE_1X_RANGE: OnceLock<Regex> = OnceLock::new();
    static RE_BARE_S: OnceLock<Regex> = OnceLock::new();
    static RE_SEASON_WORD: OnceLock<Regex> = OnceLock::new();

    let re_se = RE_SE_RANGE.get_or_init(|| {
        Regex::new(r"(?i)(?:^|[^a-z0-9])s(\d{1,2})e(\d{1,4})(?:[\s._]*-[\s._]*e?(\d{1,4})|(?:[\s._]*e(\d{1,4}))+)?(?:[^a-z0-9]|$)").unwrap()
    });
    let re_1x = RE_1X_RANGE.get_or_init(|| {
        Regex::new(r"(?i)(?:^|[^a-z0-9])(\d{1,2})x(\d{1,3})(?:-(\d{1,3}))?(?:[^a-z0-9]|$)").unwrap()
    });
    let re_bare_s = RE_BARE_S.get_or_init(|| {
        Regex::new(r"(?i)(?:^|[^a-z0-9])s(\d{1,2})(?:[\s._-]*-[\s._-]*s?(\d{1,2}))?(?:[^a-z0-9]|$)")
            .unwrap()
    });
    let re_season_word = RE_SEASON_WORD.get_or_init(|| {
        Regex::new(r"(?i)(?:^|[^a-z0-9])season[\s._-]+(\d{1,2})(?:[^a-z0-9]|$)").unwrap()
    });

    let mut eps: BTreeSet<EpisodeSpec> = BTreeSet::new();
    let mut seasons_with_explicit_eps: BTreeSet<u16> = BTreeSet::new();
    let mut earliest_pos: Option<usize> = None;

    let mut record_pos = |pos: usize| {
        earliest_pos = Some(match earliest_pos {
            Some(prev) => prev.min(pos),
            None => pos,
        });
    };

    // 1. SxxEyy, SxxEyyEzz, SxxEyy-Ezz, SxxEyy-zz
    static RE_CHAIN_TOP: OnceLock<Regex> = OnceLock::new();
    let re_chain = RE_CHAIN_TOP.get_or_init(|| Regex::new(r"(?i)e(\d{1,4})").unwrap());
    for cap in re_se.captures_iter(work) {
        let m = cap.get(0).unwrap();
        let s: u16 = match cap[1].parse() {
            Ok(v) if (1..=99).contains(&v) => v,
            _ => continue,
        };
        let e1: u16 = match cap[2].parse() {
            Ok(v) if v <= 9999 => v,
            _ => continue,
        };
        record_pos(m.start());
        seasons_with_explicit_eps.insert(s);
        eps.insert(EpisodeSpec::Episode(s, e1));

        let matched_text = m.as_str();
        // Expand explicit range "-E04" or "-04"
        if let Some(rm) = cap.get(3) {
            if let Ok(e2) = rm.as_str().parse::<u16>() {
                if e2 >= e1 && (e2 - e1) <= 100 {
                    for e in e1..=e2 {
                        eps.insert(EpisodeSpec::Episode(s, e));
                    }
                } else if e2 <= 9999 {
                    eps.insert(EpisodeSpec::Episode(s, e2));
                }
            }
        }
        // Also expand chained E02E03E04 inside matched_text
        if cap.get(4).is_some() {
            let rc = re_chain;
            for ec in rc.captures_iter(matched_text) {
                if let Ok(en) = ec[1].parse::<u16>() {
                    if en <= 9999 {
                        eps.insert(EpisodeSpec::Episode(s, en));
                    }
                }
            }
        }
    }

    // 2. 1x02 / 1x02-04
    for cap in re_1x.captures_iter(work) {
        let m = cap.get(0).unwrap();
        let s: u16 = match cap[1].parse() {
            Ok(v) if (1..=99).contains(&v) => v,
            _ => continue,
        };
        let e1: u16 = match cap[2].parse() {
            Ok(v) if v <= 999 => v,
            _ => continue,
        };
        record_pos(m.start());
        seasons_with_explicit_eps.insert(s);
        eps.insert(EpisodeSpec::Episode(s, e1));
        if let Some(rm) = cap.get(3) {
            if let Ok(e2) = rm.as_str().parse::<u16>() {
                if e2 >= e1 && (e2 - e1) <= 100 {
                    for e in e1..=e2 {
                        eps.insert(EpisodeSpec::Episode(s, e));
                    }
                }
            }
        }
    }

    // 3. Bare S01 / S01-S03 / Season 1 — ONLY add WholeSeason if no explicit episodes exist for that season!
    for cap in re_bare_s.captures_iter(work) {
        let m = cap.get(0).unwrap();
        let s1: u16 = match cap[1].parse() {
            Ok(v) if (1..=99).contains(&v) => v,
            _ => continue,
        };
        record_pos(m.start());
        let s2: u16 = cap
            .get(2)
            .and_then(|x| x.as_str().parse().ok())
            .filter(|&v| v >= s1 && v <= 99 && (v - s1) <= 50)
            .unwrap_or(s1);
        for s in s1..=s2 {
            if !seasons_with_explicit_eps.contains(&s) {
                eps.insert(EpisodeSpec::WholeSeason(s));
            }
        }
    }
    for cap in re_season_word.captures_iter(work) {
        let m = cap.get(0).unwrap();
        if let Ok(s) = cap[1].parse::<u16>() {
            if (1..=99).contains(&s) {
                record_pos(m.start());
                if !seasons_with_explicit_eps.contains(&s) {
                    eps.insert(EpisodeSpec::WholeSeason(s));
                }
            }
        }
    }

    (eps.into_iter().collect(), earliest_pos)
}

/// Hardened release name parser with exact token matching and structural anchors.
pub fn parse_release_name(name: &str) -> ParsedRelease {
    let mut pr = ParsedRelease::default();
    let trimmed = name.trim();
    if trimmed.is_empty() {
        return pr;
    }

    // 1. Strip CJK fullwidth site tags & leading www.site.tld - prefixes
    let cleaned_cjk = cjk_bracket_regex().replace_all(trimmed, " ");
    let mut work_owned = cleaned_cjk.trim().to_string();
    for _ in 0..3 {
        let next = site_prefix_regex().replace(&work_owned, "").into_owned();
        if next == work_owned {
            break;
        }
        work_owned = next.trim().to_string();
    }
    let mut work = work_owned.as_str();

    // 2. Leading "[Group] " bracket (preserve title brackets like "[REC].2007" where next char is not a letter/space)
    if work.starts_with('[') {
        if let Some(end) = work.find(']') {
            let grp = work[1..end].trim();
            let rest = work[end + 1..].trim_start();
            let rest_after_dash = rest.strip_prefix('-').map(str::trim_start).unwrap_or(rest);
            let is_numeric_only = !grp.is_empty()
                && grp
                    .chars()
                    .all(|c| c.is_ascii_digit() || matches!(c, '.' | '_' | '/' | '-' | ' '));
            let next_starts_letter = rest_after_dash
                .chars()
                .next()
                .is_some_and(|c| c.is_alphabetic());
            if !grp.is_empty() && grp.len() <= 28 && !is_numeric_only && next_starts_letter {
                pr.release_group = grp.to_ascii_lowercase();
                pr.is_anime_cue = true;
                work = rest_after_dash;
            }
        }
    }

    // 3. Extract episodes & earliest episode byte anchor BEFORE dash splitting!
    let (episodes, ep_anchor_byte) = extract_episodes_from_name(work);
    pr.episodes = episodes;

    // 4. Pre-normalize compound tags (WEB-DL -> webdl, Blu-Ray -> bluray, H.264 -> h264, 5.1 -> ch51)
    let work_norm = prenormalize_compounds(work);
    let raw_toks = tokenize_alnum(&work_norm);
    pr.raw_tokens = raw_toks.clone();

    // 5. Scan tokens for exact technical tags & candidate years
    let mut first_tech_idx: Option<usize> = None;
    let mut year_positions: Vec<(usize, u16)> = Vec::new();

    for (idx, tok) in raw_toks.iter().enumerate() {
        let t = tok.as_str();
        if pr.resolution == 0 {
            let r = resolution_from_exact_token(t);
            if r != 0 {
                pr.resolution = r;
                if idx > 0 && first_tech_idx.is_none() {
                    first_tech_idx = Some(idx);
                }
            }
        }
        if pr.codec == 0 {
            let c = codec_from_exact_token(t);
            if c != 0 {
                pr.codec = c;
                if idx > 0 && first_tech_idx.is_none() {
                    first_tech_idx = Some(idx);
                }
            }
        }
        if pr.source == 0 {
            let s = source_from_exact_token(t);
            if s != 0 {
                pr.source = s;
                if idx > 0 && first_tech_idx.is_none() {
                    first_tech_idx = Some(idx);
                }
            }
        }
        if matches!(t, "hdr" | "hdr10" | "hdr10plus" | "dv" | "dovi") {
            pr.hdr_flags |= 1;
            if idx > 0 && first_tech_idx.is_none() {
                first_tech_idx = Some(idx);
            }
        }
        if is_audio_codec_tok(t) {
            pr.audio_flags |= 1;
            if idx > 0 && first_tech_idx.is_none() {
                first_tech_idx = Some(idx);
            }
        }
        if idx > 0 && first_tech_idx.is_none() && is_unambiguous_tech_stop(t) {
            first_tech_idx = Some(idx);
        }
        if idx > 0 && t.len() == 4 {
            if let Ok(y) = t.parse::<u16>() {
                if (1920..=2029).contains(&y) {
                    year_positions.push((idx, y));
                }
            }
        }
    }

    // 6. Fixed trailing-hyphen release group extraction:
    // NEVER split on " - " ("Artist - Album"), and only treat trailing "-GROUP" as a release group
    // when preceded by release metadata (tech/year/ep) or scene-dotted formatting without spaces!
    if pr.release_group.is_empty() {
        let base = if let Some(dot) = work.rfind('.') {
            let ext = &work[dot + 1..];
            if ext.len() <= 5 && ext.bytes().all(|b| b.is_ascii_alphanumeric()) {
                &work[..dot]
            } else {
                work
            }
        } else {
            work
        };
        if let Some(dash) = base.rfind('-') {
            let before_char = base[..dash].chars().next_back();
            let after_slice = &base[dash + 1..];
            let no_space_around_dash = !matches!(before_char, Some(' ') | Some('\t') | None)
                && !after_slice.starts_with(' ');
            let is_compound_tag = matches!(
                after_slice.to_ascii_lowercase().as_str(),
                "dl" | "rip" | "ray" | "hd" | "cam" | "264" | "265"
            );
            let has_release_context = first_tech_idx.is_some()
                || !year_positions.is_empty()
                || !pr.episodes.is_empty()
                || (!base.contains(' ') && base.contains('.'));
            if no_space_around_dash
                && !is_compound_tag
                && has_release_context
                && after_slice.len() >= 2
                && after_slice.len() <= 20
                && after_slice.bytes().all(|b| b.is_ascii_alphanumeric())
                && !after_slice.bytes().all(|b| b.is_ascii_digit())
            {
                pr.release_group = after_slice.to_ascii_lowercase();
            }
        }
    }

    // 7. Compute title boundary anchor:
    // Priority: earliest episode anchor (in token count), then LAST release year before tech, then first tech tag!
    let ep_tok_idx: Option<usize> = ep_anchor_byte.map(|bpos| {
        let prefix = &work[..bpos.min(work.len())];
        let pnorm = prenormalize_compounds(prefix);
        tokenize_alnum(&pnorm).len()
    });

    // Pick the release year anchor: prefer the LAST year at or before `first_tech_idx` (protects "Wonder Woman 1984 2020 1080p"!)
    let tech_limit = first_tech_idx.unwrap_or(raw_toks.len());
    let chosen_year: Option<(usize, u16)> = year_positions
        .iter()
        .copied()
        .rfind(|&(pos, _)| pos <= tech_limit)
        .or_else(|| year_positions.last().copied());

    if let Some((_, y)) = chosen_year {
        pr.year = y;
    }

    let mut cut_idx = raw_toks.len();
    if let Some(ep_i) = ep_tok_idx {
        if ep_i > 0 {
            cut_idx = cut_idx.min(ep_i);
        }
    }
    if let Some((y_i, _)) = chosen_year {
        if y_i > 0 {
            cut_idx = cut_idx.min(y_i);
        }
    }
    if let Some(t_i) = first_tech_idx {
        if t_i > 0 {
            cut_idx = cut_idx.min(t_i);
        }
    }
    // Strip trailing scene group token if no earlier cut occurred
    if cut_idx == raw_toks.len()
        && !pr.release_group.is_empty()
        && raw_toks.len() > 1
        && raw_toks.last().map(String::as_str) == Some(pr.release_group.as_str())
    {
        cut_idx -= 1;
    }
    if cut_idx == 0 && !raw_toks.is_empty() {
        cut_idx = 1;
    }

    let title_toks: Vec<String> = raw_toks[..cut_idx].to_vec();
    let (sh, fh) = title_hashes_from_tokens(&title_toks);
    if sh != 0 {
        pr.strict_title_hashes.push(sh);
    }
    if fh != 0 {
        pr.family_hashes.push(fh);
    }

    // In-title year rescue ("Wonder.Woman.1984.1080p.WEB-DL" without a second release year):
    // Also emit the year-included title hash when the cut happened right on the ONLY year!
    if let Some((y_i, _)) = chosen_year {
        if y_i == cut_idx && year_positions.len() == 1 && y_i < raw_toks.len() {
            let with_year = &raw_toks[..=y_i];
            let (sh2, fh2) = title_hashes_from_tokens(with_year);
            if sh2 != 0 && !pr.strict_title_hashes.contains(&sh2) {
                pr.strict_title_hashes.push(sh2);
            }
            if fh2 != 0 && !pr.family_hashes.contains(&fh2) {
                pr.family_hashes.push(fh2);
            }
        }
    }

    pr.title_tokens = title_toks;
    pr
}

/// Parse DH-Waypoint JSONB episodes string e.g. `{"1": {"2": {}, "3": {}}, "2": {}}`.
/// Authoritative when non-empty: empty inner object `{}` = `WholeSeason(s)`, non-empty inner keys = `Episode(s, e)`.
pub fn parse_json_episodes(json_str: &str) -> Vec<EpisodeSpec> {
    if json_str.is_empty() || json_str == "{}" || json_str == "\\N" {
        return Vec::new();
    }
    let Ok(val) = serde_json::from_str::<serde_json::Value>(json_str) else {
        return Vec::new();
    };
    let Some(obj) = val.as_object() else {
        return Vec::new();
    };
    let mut out = BTreeSet::new();
    for (s_key, s_val) in obj {
        let Ok(season) = s_key.parse::<u16>() else {
            continue;
        };
        if let Some(ep_obj) = s_val.as_object() {
            if ep_obj.is_empty() {
                out.insert(EpisodeSpec::WholeSeason(season));
            } else {
                for e_key in ep_obj.keys() {
                    if let Ok(ep) = e_key.parse::<u16>() {
                        out.insert(EpisodeSpec::Episode(season, ep));
                    }
                }
            }
        }
    }
    out.into_iter().collect()
}

/// Authoritative episode resolution: structured PG JSON is authoritative when present;
/// only fall back to title-parsed episodes when structured JSON is empty.
pub fn resolve_episodes(json_eps: &[EpisodeSpec], parsed_eps: &[EpisodeSpec]) -> Vec<EpisodeSpec> {
    if !json_eps.is_empty() {
        json_eps.to_vec()
    } else {
        parsed_eps.to_vec()
    }
}

/// Exact port of `unlinkedYearCompatible` from `internal/database/search/search_torrent_content_unlinked.go`.
pub fn unlinked_year_compatible(
    content_type: u8,
    canonical_year: u16,
    parsed_year: u16,
    episodes: &[EpisodeSpec],
) -> bool {
    if canonical_year == 0 || parsed_year == 0 {
        return true;
    }
    let diff = (parsed_year as i32) - (canonical_year as i32);
    match content_type {
        CT_MOVIE => (-1..=1).contains(&diff),
        CT_TV_SHOW => {
            if diff < -1 {
                return false;
            }
            let is_only_s1 = !episodes.is_empty()
                && episodes.iter().all(|e| match *e {
                    EpisodeSpec::WholeSeason(s) | EpisodeSpec::Episode(s, _) => s == 1,
                });
            if is_only_s1 {
                return (-1..=1).contains(&diff);
            }
            true
        }
        _ => true,
    }
}

/// Extract normalized file extension (lowercase ASCII alphanumeric, <= 10 chars).
#[inline]
pub fn extract_ext(path: &str) -> String {
    let base = path.rsplit(['/', '\\']).next().unwrap_or(path);
    if let Some(dot) = base.rfind('.') {
        let ext = &base[dot + 1..];
        if !ext.is_empty() && ext.len() <= 10 && ext.bytes().all(|b| b.is_ascii_alphanumeric()) {
            return ext.to_ascii_lowercase();
        }
    }
    String::new()
}

/// Generate character trigrams for `pg_trgm`-compatible substring retrieval.
/// Pads word boundaries identically to PostgreSQL `pg_trgm` (`  word ` -> trigrams).
pub fn extract_trigrams(s: &str) -> Vec<[u8; 3]> {
    let mut set: BTreeSet<[u8; 3]> = BTreeSet::new();
    for tok in tokenize_alnum(s) {
        let b = tok.as_bytes();
        if b.is_empty() {
            continue;
        }
        let mut padded = Vec::with_capacity(b.len() + 3);
        padded.push(b' ');
        padded.push(b' ');
        padded.extend_from_slice(b);
        padded.push(b' ');
        for w in padded.windows(3) {
            set.insert([w[0], w[1], w[2]]);
        }
    }
    set.into_iter().collect()
}

/// Extract strictly internal trigrams of a substring query (for substring `ILIKE '%q%'` matching,
/// we only require trigrams completely inside the query string, not boundary-padded!).
pub fn extract_substring_query_trigrams(q: &str) -> Vec<[u8; 3]> {
    let folded = fold_accents_lowercase(q);
    let mut set: BTreeSet<[u8; 3]> = BTreeSet::new();
    let mut run = Vec::new();
    for ch in folded.chars() {
        if ch.is_ascii_alphanumeric() {
            run.push(ch as u8);
        } else {
            for w in run.windows(3) {
                set.insert([w[0], w[1], w[2]]);
            }
            run.clear();
        }
    }
    for w in run.windows(3) {
        set.insert([w[0], w[1], w[2]]);
    }
    set.into_iter().collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn elided_articles_do_not_create_a_different_title_word() {
        assert_eq!(
            tokenize_alnum("Histoire d’une dune"),
            ["histoire", "d", "une", "dune"]
        );
        assert_eq!(tokenize_alnum("d'un l’une"), ["d", "un", "l", "une"]);
        assert_eq!(tokenize_alnum("D'Angelo O’Neil"), ["dangelo", "oneil"]);
    }

    #[test]
    fn test_release_parser_edge_cases() {
        // 1. "ts"/"cam" substring bugs MUST be fixed
        let r1 = parse_release_name("The.Usual.Suspects.1995.1080p.BluRay.x264-GROUP");
        assert_eq!(r1.title_tokens, vec!["the", "usual", "suspects"]);
        assert_eq!(r1.year, 1995);
        assert_eq!(r1.source, 1); // BluRay, NOT cam/ts!
        assert_eq!(r1.release_group, "group");

        let r2 = parse_release_name("The.Great.Gatsby.2013.1080p.WEB-DL.x264-SPARKS");
        assert_eq!(r2.title_tokens, vec!["the", "great", "gatsby"]);
        assert_eq!(r2.source, 2); // WEB-DL (2), NOT WEBRip (3) or CAM (6)!
        assert_eq!(r2.release_group, "sparks");

        for title in [
            "Suits.S01E01.720p.HDTV",
            "Ghosts.2021.S01E01.1080p.WEB-DL",
            "Cats.2019.1080p.BluRay",
        ] {
            let r = parse_release_name(title);
            assert_ne!(r.source, 6, "{title} must NOT be tagged CAM!");
        }

        // 2. Sequel numbers preserved
        let r3 = parse_release_name("Toy.Story.2.1999.1080p.BluRay.x264");
        assert_eq!(r3.title_tokens, vec!["toy", "story", "2"]);
        assert_eq!(r3.year, 1999);

        // 3. In-title years preserved
        let r4 = parse_release_name("Wonder.Woman.1984.2020.2160p.WEB-DL.x265");
        assert_eq!(r4.title_tokens, vec!["wonder", "woman", "1984"]);
        assert_eq!(r4.year, 2020);

        let r4b = parse_release_name("Wonder.Woman.1984.1080p.WEB-DL");
        let (ww_sh, _) = title_hashes_from_str("Wonder Woman 1984");
        assert!(r4b.strict_title_hashes.contains(&ww_sh));

        // 4. Accent folding ("Amélie" == "Amelie", "Śląsk" == "Slask")
        assert_eq!(
            title_hashes_from_str("Amélie"),
            title_hashes_from_str("Amelie")
        );
        assert_eq!(
            title_hashes_from_str("Gliniarze. Śląsk"),
            title_hashes_from_str("Gliniarze Slask")
        );

        // 5. Trailing-hyphen music album must NOT create fake release group
        let r5 = parse_release_name("Pink Floyd - Animals");
        assert_eq!(r5.release_group, "");
        assert_eq!(r5.title_tokens, vec!["pink", "floyd", "animals"]);

        // 6. Episode range expansion S01E02-E04 and no WholeSeason bleed
        let r6 = parse_release_name("Show.Name.S01E02-E04.1080p.WEB-DL");
        assert_eq!(
            r6.episodes,
            vec![
                EpisodeSpec::Episode(1, 2),
                EpisodeSpec::Episode(1, 3),
                EpisodeSpec::Episode(1, 4),
            ]
        );
        let r6b = parse_release_name("Show.Name.S01.1080p.BluRay");
        assert_eq!(r6b.episodes, vec![EpisodeSpec::WholeSeason(1)]);

        // 7. Authoritative JSON overrides guessed bare S01
        let json_eps = parse_json_episodes(r#"{"1":{"2":{},"3":{}}}"#);
        let resolved = resolve_episodes(&json_eps, &[EpisodeSpec::WholeSeason(1)]);
        assert_eq!(
            resolved,
            vec![EpisodeSpec::Episode(1, 2), EpisodeSpec::Episode(1, 3)]
        );
    }

    #[test]
    fn test_exact_serving_policy_adult_and_namepolicy() {
        // Real titles that must NOT be blocked by xxx_strong
        assert!(!evaluate_serving_adult(
            "Vixen.2015.S01E01.1080p.mkv",
            1_000_000,
            "mkv",
            &[],
            false,
            false
        ));
        assert!(!evaluate_serving_adult(
            "Deeper.2020.1080p.mkv",
            1_000_000,
            "mkv",
            &[],
            false,
            false
        ));
        assert!(!evaluate_serving_adult(
            "WrestleMania.XXX.1080p.mkv",
            1_000_000,
            "mkv",
            &[],
            false,
            false
        ));
        assert!(!evaluate_serving_adult(
            "Ass.Backwards.2013.1080p.mkv",
            1_000_000,
            "mkv",
            &[],
            false,
            false
        ));

        // Strong adult keyword with media dominance MUST be blocked — even at file index 100!
        let mut files: Vec<(String, u64, String)> = (0..100)
            .map(|i| {
                (
                    format!("dir/clip_{i:03}.mkv"),
                    10_000_000,
                    "mkv".to_string(),
                )
            })
            .collect();
        files.push((
            "dir/Naughty.America.bonus.mkv".to_string(),
            10_000_000,
            "mkv".to_string(),
        ));
        assert!(evaluate_serving_adult(
            "Regular.Folder",
            1_010_000_000,
            "",
            &files,
            false,
            false
        ));

        // Archive-dominant torrent with strong keyword is NOT blocked by media-dominant adult rule
        let non_media = vec![(
            "Naughty.America.zip".to_string(),
            100_000_000,
            "zip".to_string(),
        )];
        assert!(!evaluate_serving_adult(
            "Naughty.America.zip",
            100_000_000,
            "",
            &non_media,
            false,
            false
        ));

        // NamePolicy exact checks
        let np = NamePolicyMatcher::global();
        assert!(np.is_blocked("   "));
        assert!(np.is_blocked("电影.Movie.2024.1080p"));
        assert!(np.is_blocked("Фильм.2024.1080p"));
        assert!(np.is_blocked("Fetish.Porn.2024.1080p"));
        assert!(np.is_blocked("Hard.Porn.XXX.2024"));
        assert!(!np.is_blocked("WrestleMania.XXX.2014.1080p"));
        assert!(!np.is_blocked("The.Usual.Suspects.1995.1080p"));

        // Privacy tag exact checks
        assert!(is_privacy_tag("bitgrab"));
        assert!(is_privacy_tag("  BitGrab:private  "));
        assert!(!is_privacy_tag("bitgrabber"));
        assert!(is_qb_privacy("qBittorrent", "private"));
        assert!(!is_qb_privacy("dht", "private"));
    }
}
