package model

import (
	"regexp"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/keywords"
)

// VideoResolution represents the resolution of a video
// ENUM(V360p, V480p, V540p, V576p, V720p, V1080p, V1440p, V2160p, V4320p)
type VideoResolution string

func (v VideoResolution) Label() string {
	return v.String()[1:]
}

// videoResolutionAliases are other exact ways to write a height.
var videoResolutionAliases = map[string]VideoResolution{
	"1080i":     VideoResolutionV1080p,
	"1920x1080": VideoResolutionV1080p,
	"3840x2160": VideoResolutionV2160p,
}

// videoResolutionWords are loose words that only hint at a height: "HD" on a
// 1080p release, "UHD" naming the disc a 1080p encode came from.
var videoResolutionWords = map[string]VideoResolution{
	"2k":  VideoResolutionV1080p,
	"4k":  VideoResolutionV2160p,
	"8k":  VideoResolutionV4320p,
	"sd":  VideoResolutionV480p,
	"hd":  VideoResolutionV720p,
	"fhd": VideoResolutionV1080p,
	"uhd": VideoResolutionV2160p,
}

func createVideoResolutionRegex(aliases map[string]VideoResolution, names ...string) *regexp.Regexp {
	for res := range aliases {
		names = append(names, res)
	}

	return keywords.MustNewRegexFromKeywords(names...)
}

var videoResolutionExactRegex = createVideoResolutionRegex(
	videoResolutionAliases, namesToLower(removeEnumPrefixes(VideoResolutionNames()...)...)...)

var videoResolutionWordRegex = createVideoResolutionRegex(videoResolutionWords)

// FindExactVideoResolution returns the first exact height in input ("1080p",
// "1920x1080") and the offset where it starts, or -1 when there is none. Loose
// words such as "HD" are ignored.
func FindExactVideoResolution(input string) (NullVideoResolution, int) {
	loc := videoResolutionExactRegex.FindStringSubmatchIndex(input)
	if loc == nil {
		return NullVideoResolution{}, -1
	}

	match := input[loc[2]:loc[3]]
	if parsed, parseErr := ParseVideoResolution("V" + match); parseErr == nil {
		return NewNullVideoResolution(parsed), loc[2]
	}

	return NewNullVideoResolution(videoResolutionAliases[strings.ToLower(match)]), loc[2]
}

// InferVideoResolution reads the resolution from input. An exact height beats
// a loose word wherever each one sits: "HD 1080p" and "UHD.BluRay.1080p" are
// both 1080p.
func InferVideoResolution(input string) NullVideoResolution {
	if exact, _ := FindExactVideoResolution(input); exact.Valid {
		return exact
	}

	if match := videoResolutionWordRegex.FindStringSubmatch(input); match != nil {
		return NewNullVideoResolution(videoResolutionWords[strings.ToLower(match[1])])
	}

	return NullVideoResolution{}
}
