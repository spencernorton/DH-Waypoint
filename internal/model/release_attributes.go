package model

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

const ReleaseAttributesParser = "release-attributes-v1"

// ReleaseAttributes contains advertised filename claims, never verified media
// tracks. A nil value means no supported explicit evidence was retained.
type ReleaseAttributes struct {
	Version          int      `json:"version"`
	Parser           string   `json:"parser"`
	SourceNameSHA256 string   `json:"sourceNameSha256"`
	HDRFormats       []string `json:"hdrFormats,omitempty"`
	AudioFormats     []string `json:"audioFormats,omitempty"`
	AudioChannels    *string  `json:"audioChannels,omitempty"`
	AudioFeatures    []string `json:"audioFeatures,omitempty"`
	Revisions        []string `json:"revisions,omitempty"`
	Encoder          *string  `json:"encoder,omitempty"`
}

const (
	hdrBitDolbyVision uint8 = 1 << iota
	hdrBitHDR
	hdrBitHDR10
	hdrBitHDR10Plus
)

const (
	audioBitAAC uint8 = 1 << iota
	audioBitAC3
	audioBitDTS
	audioBitEAC3
	audioBitFLAC
	audioBitOpus
	audioBitTrueHD
)

const (
	featureBitAtmos uint8 = 1 << iota
	featureBitDTSX
)

const (
	revBitInternal uint8 = 1 << iota
	revBitProper
	revBitReadNFO
	revBitRepack
)

const (
	claimFieldHDR uint8 = iota
	claimFieldAudio
	claimFieldFeature
	claimFieldRevision
)

var releaseAttributeClaims = []struct {
	pattern *regexp.Regexp
	field   uint8
	bit     uint8
}{
	{claimRegex(`hdr10(?:\+|[ ._-]?plus)`), claimFieldHDR, hdrBitHDR10Plus},
	{claimRegex(`hdr10`), claimFieldHDR, hdrBitHDR10},
	{claimRegex(`hdr`), claimFieldHDR, hdrBitHDR},
	{claimRegex(`dv|dovi|dolby[ ._-]?vision`), claimFieldHDR, hdrBitDolbyVision},
	{audioClaimRegex(`true[ ._-]?hd`), claimFieldAudio, audioBitTrueHD},
	{audioClaimRegex(`dts(?:[ ._-]?(?:hd|ma|hres|es|x))*`), claimFieldAudio, audioBitDTS},
	{audioClaimRegex(`e[ ._-]?ac[ ._-]?3|ddp|dd\+`), claimFieldAudio, audioBitEAC3},
	{audioClaimRegex(`ac[ ._-]?3|dd`), claimFieldAudio, audioBitAC3},
	{audioClaimRegex(`aac`), claimFieldAudio, audioBitAAC},
	{audioClaimRegex(`flac`), claimFieldAudio, audioBitFLAC},
	{audioClaimRegex(`opus`), claimFieldAudio, audioBitOpus},
	{claimRegex(`atmos`), claimFieldFeature, featureBitAtmos},
	{claimRegex(`dts[ ._-]?x`), claimFieldFeature, featureBitDTSX},
	{claimRegex(`proper`), claimFieldRevision, revBitProper},
	{claimRegex(`repack`), claimFieldRevision, revBitRepack},
	{claimRegex(`readnfo`), claimFieldRevision, revBitReadNFO},
	{claimRegex(`internal`), claimFieldRevision, revBitInternal},
}

// Precompiled encoder claim patterns (must stay hoisted at package scope to
// avoid per-call regexp.MustCompile heap churn on the classifier hot path).
var (
	encoderX264Claim = claimRegex(`x264`)
	encoderX265Claim = claimRegex(`x265`)
)

var audioChannelClaim = regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(?:true[ ._-]?hd|dts(?:[ ._-]?(?:hd|ma|hres|es|x))*|e?[ ._-]?ac[ ._-]?3|ddp?|aac|flac|opus)[ ._-]*([1-9][ ._][01]|[1-9]ch)(?:$|[^\pL\pN])`)

func claimRegex(token string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(?:` + token + `)(?:$|[^\pL\pN])`)
}

func audioClaimRegex(token string) *regexp.Regexp {
	return claimRegex(`(?:` + token + `)(?:[ ._-]?[1-9](?:[ ._][01])?(?:[ ._]?ch)?)?`)
}

// Ordered slices matching sort.Strings order for each bitmask.
var (
	hdrOrder = []struct {
		bit   uint8
		value string
	}{
		{hdrBitDolbyVision, "DOLBY_VISION"},
		{hdrBitHDR, "HDR"},
		{hdrBitHDR10, "HDR10"},
		{hdrBitHDR10Plus, "HDR10_PLUS"},
	}
	audioOrder = []struct {
		bit   uint8
		value string
	}{
		{audioBitAAC, "AAC"},
		{audioBitAC3, "AC3"},
		{audioBitDTS, "DTS"},
		{audioBitEAC3, "EAC3"},
		{audioBitFLAC, "FLAC"},
		{audioBitOpus, "OPUS"},
		{audioBitTrueHD, "TRUEHD"},
	}
	featureOrder = []struct {
		bit   uint8
		value string
	}{
		{featureBitAtmos, "ATMOS"},
		{featureBitDTSX, "DTS_X"},
	}
	revOrder = []struct {
		bit   uint8
		value string
	}{
		{revBitInternal, "INTERNAL"},
		{revBitProper, "PROPER"},
		{revBitReadNFO, "READNFO"},
		{revBitRepack, "REPACK"},
	}
)

func collectMask(mask uint8, order []struct {
	bit   uint8
	value string
}) []string {
	if mask == 0 {
		return nil
	}
	out := make([]string, 0, len(order))
	for _, entry := range order {
		if mask&entry.bit != 0 {
			out = append(out, entry.value)
		}
	}
	return out
}

func normalizeChannel(raw string) string {
	b := []byte(raw)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + ('a' - 'A')
		case c == ' ' || c == '_':
			b[i] = '.'
		}
	}
	return string(b)
}

// InferReleaseAttributes reads only a technical suffix. sourceName is retained
// as a digest so subsequent repairs can compare the exact release input.
func InferReleaseAttributes(sourceName, technicalSuffix string) *ReleaseAttributes {
	var hdrMask, audioMask, featMask, revMask uint8
	for _, claim := range releaseAttributeClaims {
		if claim.pattern.MatchString(technicalSuffix) {
			switch claim.field {
			case claimFieldHDR:
				hdrMask |= claim.bit
			case claimFieldAudio:
				audioMask |= claim.bit
			case claimFieldFeature:
				featMask |= claim.bit
			case claimFieldRevision:
				revMask |= claim.bit
			}
		}
	}
	// More specific claims subsume the generic family label.
	if hdrMask&hdrBitHDR10Plus != 0 {
		hdrMask &^= hdrBitHDR10
	}
	if hdrMask&(hdrBitHDR10|hdrBitHDR10Plus) != 0 {
		hdrMask &^= hdrBitHDR
	}
	if audioMask&audioBitEAC3 != 0 {
		audioMask &^= audioBitAC3
	}

	var channel string
	ambiguousChannels := false
	for offset := 0; offset < len(technicalSuffix); {
		match := audioChannelClaim.FindStringSubmatchIndex(technicalSuffix[offset:])
		if match == nil {
			break
		}
		value := normalizeChannel(technicalSuffix[offset+match[2] : offset+match[3]])
		if channel != "" && channel != value {
			ambiguousChannels = true
		}
		channel = value
		offset += match[3]
	}

	hasX264 := encoderX264Claim.MatchString(technicalSuffix)
	hasX265 := encoderX265Claim.MatchString(technicalSuffix)
	var encoder *string
	if hasX264 != hasX265 {
		v := "x264"
		if hasX265 {
			v = "x265"
		}
		encoder = &v
	}

	var audioChannels *string
	if channel != "" && !ambiguousChannels {
		v := channel
		audioChannels = &v
	}

	if hdrMask == 0 && audioMask == 0 && featMask == 0 && revMask == 0 &&
		audioChannels == nil && encoder == nil {
		return nil
	}

	digest := sha256.Sum256([]byte(sourceName))
	return &ReleaseAttributes{
		Version:          1,
		Parser:           ReleaseAttributesParser,
		SourceNameSHA256: hex.EncodeToString(digest[:]),
		HDRFormats:       collectMask(hdrMask, hdrOrder),
		AudioFormats:     collectMask(audioMask, audioOrder),
		AudioChannels:    audioChannels,
		AudioFeatures:    collectMask(featMask, featureOrder),
		Revisions:        collectMask(revMask, revOrder),
		Encoder:          encoder,
	}
}

