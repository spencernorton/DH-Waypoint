package model

import (
	"fmt"
	"regexp"

	"gorm.io/gorm"
)

var tagNameRegex = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const tagNameMaxLength = 30

// UnsafeFileAttributesTag records executable or symlink flags observed in
// authenticated torrent metadata before the file list is projected to paths.
const UnsafeFileAttributesTag = "unsafe-file-attributes"

func ValidateTagName(name string) error {
	if !tagNameRegex.MatchString(name) {
		return fmt.Errorf("invalid tag name: '%s' (must be kebab-case)", name)
	}

	if len(name) > tagNameMaxLength {
		return fmt.Errorf(
			"invalid tag name: '%s' (must be no longer than %d characters)",
			name,
			tagNameMaxLength,
		)
	}

	return nil
}

func (t *TorrentTag) BeforeCreate(*gorm.DB) error {
	return ValidateTagName(t.Name)
}
