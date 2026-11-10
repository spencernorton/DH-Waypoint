// Package csamblocklisttest builds made-up torrent names for tests of the
// banned-keyword checks, so no test has to spell out a keyword.
package csamblocklisttest

import (
	"regexp"

	"github.com/spencernorton/DH-Waypoint/internal/classifier"
)

var plainToken = regexp.MustCompile(`^[a-z0-9]+$`)

// BannedName returns a made-up name that the built-in banned-keyword list
// matches.
func BannedName() string { return "Sample " + BannedToken() + " Clip" }

// BannedToken returns the first plain token of the built-in list, for tests
// that need it somewhere other than inside a name, such as an extension.
func BannedToken() string {
	src, err := classifier.CoreSource()
	if err != nil {
		panic(err)
	}
	for _, kw := range src.Keywords["banned"] {
		if plainToken.MatchString(kw) {
			return kw
		}
	}
	panic("csamblocklisttest: the built-in banned list has no plain token")
}
