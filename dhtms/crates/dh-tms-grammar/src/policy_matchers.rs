use regex::Regex;
use std::sync::OnceLock;

/// Exact 23 media extensions from `internal/classifier/classifier.core.yml` (`video` + `image`),
/// matching `classifier.CoreAdultServingEvidence()` in `DH-Waypoint`.
pub fn is_serving_media_ext(ext: &str) -> bool {
    matches!(
        ext.to_ascii_lowercase().as_str(),
        // video (12)
        "mp4" | "mkv" | "avi" | "iso" | "mov" | "wmv" | "flv" | "m4v" | "mpg" | "mpeg" | "ts" | "vob"
        // image (11)
        | "jpg" | "jpeg" | "png" | "gif" | "bmp" | "svg" | "dds" | "psd" | "tif" | "tiff" | "ico"
    )
}

/// Exact 37 `xxx_strong` patterns from `internal/classifier/classifier.core.yml`,
/// compiled using `internal/keywords/parser.go` semantics:
///   word boundary = `(^|[^\p{L}0-9]+)` ... `([^\p{L}0-9]+|$)`
///   `*` = `[\p{L}0-9]*`, `#` = `[0-9]`, ` ` = `[^\p{L}0-9]`, letters case-insensitive.
pub fn strong_adult_regex() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| {
        let keywords: &[&str] = &[
            "blowjobs?",
            "bukkake",
            "creampie*",
            "cumshots?",
            "deepthroat*",
            "gangbang*",
            "*gloryhole*",
            "handjobs?",
            "nubile*",
            "private ?gold",
            "siterip",
            "*wank*",
            "bang ?bros",
            "bang ?bus",
            "brazzers",
            "cum ?louder",
            "digital ?playground",
            "fake ?hub",
            "jules ?jordan",
            "legal ?porno",
            "met ?art",
            "mofos",
            "naughty ?america",
            "nubile ?films",
            "reality ?kings",
            "sex ?art",
            "team ?skeet",
            "twistys",
            "caribbeancom",
            "fc# ?ppv",
            "heyzo",
            "1pondo",
            "paco ?paco ?mama",
            "tokyo ?hot",
        ];
        let mut arms = Vec::with_capacity(keywords.len());
        for kw in keywords {
            arms.push(compile_keyword_pattern(kw));
        }
        let full = format!(
            r"(?i)(?:^|[^\p{{L}}0-9]+)(?:{})(?:[^\p{{L}}0-9]+|$)",
            arms.join("|")
        );
        Regex::new(&full).expect("valid xxx_strong regex")
    })
}

fn compile_keyword_pattern(kw: &str) -> String {
    let mut out = String::new();
    let chars: Vec<char> = kw.chars().collect();
    let mut i = 0usize;
    while i < chars.len() {
        let ch = chars[i];
        let piece = match ch {
            '*' => r"[\p{L}0-9]*".to_string(),
            '#' => "[0-9]".to_string(),
            ' ' => r"[^\p{L}0-9]".to_string(),
            c => regex::escape(&c.to_string()),
        };
        out.push_str(&piece);
        if i + 1 < chars.len() && matches!(chars[i + 1], '?' | '+') {
            out.push(chars[i + 1]);
            i += 2;
        } else {
            i += 1;
        }
    }
    out
}

#[inline]
fn strip_trailing_ext<'a>(text: &'a str, ext: &str) -> &'a str {
    if ext.is_empty() {
        return text;
    }
    let strip_chars = ext.chars().count() + 1;
    let total_chars = text.chars().count();
    if total_chars <= strip_chars {
        return "";
    }
    let keep = total_chars - strip_chars;
    match text.char_indices().nth(keep) {
        Some((byte_idx, _)) => &text[..byte_idx],
        None => text,
    }
}

/// Exact `serving.Policy` adult predicate parity (`internal/serving/policy.go:194-227`):
/// 1. `content_type == CT_XXX` or `content_adult == true`, OR
/// 2. Media-byte dominance (`single`: `size > 0 && ext in media`; `multi`: `sum(+media, -nonmedia) > 0`)
///    AND `strong_adult_regex` matches `base_name` OR ANY `file_base_path` (all files checked, zero truncation!).
pub fn evaluate_serving_adult(
    name: &str,
    total_size: u64,
    single_ext: &str,
    files: &[(String, u64, String)],
    is_ct_xxx: bool,
    content_adult: bool,
) -> bool {
    if is_ct_xxx || content_adult {
        return true;
    }
    let media_dominant = if files.is_empty() {
        total_size > 0 && is_serving_media_ext(single_ext)
    } else {
        let mut signed_sum: i128 = 0;
        for (_, sz, ext) in files {
            let s = *sz as i128;
            if is_serving_media_ext(ext) {
                signed_sum += s;
            } else {
                signed_sum -= s;
            }
        }
        signed_sum > 0
    };
    if !media_dominant {
        return false;
    }
    let re = strong_adult_regex();
    let base_name = if files.is_empty() && !single_ext.is_empty() {
        strip_trailing_ext(name, single_ext)
    } else {
        name
    };
    if re.is_match(base_name) {
        return true;
    }
    for (path, _, ext) in files {
        let base_path = if !ext.is_empty() {
            strip_trailing_ext(path, ext)
        } else {
            path.as_str()
        };
        if re.is_match(base_path) {
            return true;
        }
    }
    false
}

/// Exact Rust port of `internal/namepolicy/policy.go` (`release-name-policy-v1`).
pub struct NamePolicyMatcher {
    terms: Vec<(&'static str, Regex, bool)>,
}

const TAG_WHITESPACE: &str = "\t\n\x0b\x0c\r \u{0085}\u{00a0}\u{1680}\u{2000}\u{2001}\u{2002}\u{2003}\u{2004}\u{2005}\u{2006}\u{2007}\u{2008}\u{2009}\u{200a}\u{2028}\u{2029}\u{202f}\u{205f}\u{3000}";

fn build_namepolicy_term_regex(words: &[&str]) -> Regex {
    let delims_raw = format!("{TAG_WHITESPACE}._-+:/\\()[]{{}}!,;=?&'\"|#%@");
    let seps_raw = format!("{TAG_WHITESPACE}._-+");
    let delims = regex::escape(&delims_raw);
    let seps = regex::escape(&seps_raw);
    let mut variants = Vec::with_capacity(words.len());
    for w in words {
        let mut letters = Vec::new();
        for ch in w.chars() {
            if ch.is_ascii_lowercase() {
                letters.push(format!("[{}{}]", ch, ch.to_ascii_uppercase()));
            } else {
                letters.push(regex::escape(&ch.to_string()));
            }
        }
        variants.push(letters.join(&format!("[{seps}]*")));
    }
    let pat = format!("(^|[{delims}])({})($|[{delims}])", variants.join("|"));
    Regex::new(&pat).expect("valid namepolicy term regex")
}

#[inline]
fn is_han(ch: char) -> bool {
    let cp = ch as u32;
    matches!(
        cp,
        0x2E80..=0x2EFF
            | 0x2F00..=0x2FDF
            | 0x3005
            | 0x3007
            | 0x3021..=0x3029
            | 0x3038..=0x303B
            | 0x3400..=0x4DBF
            | 0x4E00..=0x9FFF
            | 0xF900..=0xFAFF
            | 0x20000..=0x2A6DF
            | 0x2A700..=0x2B73F
            | 0x2B740..=0x2B81F
            | 0x2B820..=0x2CEAF
            | 0x2CEB0..=0x2EBEF
            | 0x2F800..=0x2FA1F
            | 0x30000..=0x3134F
            | 0x31350..=0x323AF
    )
}

#[inline]
fn is_cyrillic(ch: char) -> bool {
    let cp = ch as u32;
    matches!(
        cp,
        0x0400..=0x04FF
            | 0x0500..=0x052F
            | 0x1C80..=0x1C8F
            | 0x1D2B
            | 0x1D78
            | 0x2DE0..=0x2DFF
            | 0xA640..=0xA69F
            | 0xFE2E..=0xFE2F
    )
}

impl NamePolicyMatcher {
    pub fn global() -> &'static Self {
        static NP: OnceLock<NamePolicyMatcher> = OnceLock::new();
        NP.get_or_init(|| {
            let specs: &[(&'static str, &[&str], bool)] = &[
                (
                    "fetish",
                    &["fetish", "fetishxxx", "fetishporn", "fetishporno"],
                    true,
                ),
                ("blowjob", &["blowjob"], true),
                ("gangbang", &["gangbang"], true),
                ("bukkake", &["bukkake"], true),
                ("cumshot", &["cumshot"], true),
                ("gaping_anal", &["gapinganal"], true),
                (
                    "porn",
                    &["porn", "p0rn", "pr0n", "fetishporn", "pornxxx"],
                    false,
                ),
                ("porno", &["porno", "fetishporno", "pornoxxx"], false),
                ("xxx", &["xxx", "fetishxxx", "pornxxx", "pornoxxx"], false),
                ("hentai", &["hentai"], false),
                ("milf", &["milf"], false),
                ("anal", &["anal"], false),
                ("adult", &["adult"], false),
                ("nsfw", &["nsfw"], false),
                ("hardcore", &["hardcore"], false),
                ("sex", &["sex", "s3x"], false),
            ];
            let terms = specs
                .iter()
                .map(|&(name, words, strong)| (name, build_namepolicy_term_regex(words), strong))
                .collect();
            Self { terms }
        })
    }

    /// Returns true if the name is BLOCKED (excluded) by `release-name-policy-v1`.
    pub fn is_blocked(&self, name: &str) -> bool {
        if name.trim().is_empty() {
            return true;
        }
        for ch in name.chars() {
            if is_han(ch) || is_cyrillic(ch) {
                return true;
            }
        }
        let mut count = 0usize;
        let mut strong = false;
        let mut matched_porn = false;
        let mut matched_porno = false;
        let mut matched_xxx = false;
        for (tname, re, is_strong) in &self.terms {
            if re.is_match(name) {
                count += 1;
                if *is_strong {
                    strong = true;
                }
                match *tname {
                    "porn" => matched_porn = true,
                    "porno" => matched_porno = true,
                    "xxx" => matched_xxx = true,
                    _ => {}
                }
            }
        }
        let explicit_pair = (matched_porn && matched_xxx) || (matched_porno && matched_xxx);
        (count >= 2 && strong) || explicit_pair
    }
}

/// Exact `catalogueguard.PrivacyTag` (`bitgrab` or `bitgrab[:/_-]*`).
pub fn is_privacy_tag(tag_name: &str) -> bool {
    let s = tag_name
        .trim_matches(|c: char| TAG_WHITESPACE.contains(c))
        .to_ascii_lowercase();
    if s == "bitgrab" {
        return true;
    }
    if let Some(rest) = s.strip_prefix("bitgrab") {
        if let Some(first) = rest.chars().next() {
            return matches!(first, ':' | '/' | '-' | '_');
        }
    }
    false
}

/// Exact `catalogueguard.QBPrivacySQL` (`source == qbittorrent` AND `category in (private, bitgrab)`).
pub fn is_qb_privacy(source: &str, category: &str) -> bool {
    let s = source
        .trim_matches(|c: char| TAG_WHITESPACE.contains(c))
        .to_ascii_lowercase();
    let c = category
        .trim_matches(|c: char| TAG_WHITESPACE.contains(c))
        .to_ascii_lowercase();
    s == "qbittorrent" && matches!(c.as_str(), "private" | "bitgrab")
}

/// Exact `torrent_canonical_labels` privacy check (`category in (private, bitgrab)`).
pub fn is_private_canonical_label(category: &str) -> bool {
    let c = category
        .trim_matches(|c: char| TAG_WHITESPACE.contains(c))
        .to_ascii_lowercase();
    matches!(c.as_str(), "private" | "bitgrab")
}
