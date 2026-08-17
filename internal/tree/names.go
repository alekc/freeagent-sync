// Package tree builds the browsable views of the archive: the records as
// JSON files, and the attachments as a tree of named symlinks.
//
// Both are derived. SQLite stays the source of truth and these are
// regenerated from it, because writing them alongside the archive would let a
// crash leave the two disagreeing with no way to tell which was right.
package tree

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// maxNameLength keeps a generated name inside the 255-byte limit every common
// filesystem imposes, with room for a collision suffix and an extension.
const maxNameLength = 120

// unsafeRunes is everything that must not reach a path component: separators,
// the Windows-reserved set, and anything that would confuse a shell.
var unsafeRunes = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]+`)

// collapse turns runs of separators and spaces into a single hyphen.
var collapse = regexp.MustCompile(`[\s_-]+`)

// Slug makes a string safe as one path component: lowercase, hyphenated, no
// separators, bounded length. Empty input, or input that sanitises away to
// nothing, yields "unnamed" rather than an empty component.
func Slug(s string) string {
	s = unsafeRunes.ReplaceAllString(s, "-")
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == ' ' {
			return unicode.ToLower(r)
		}
		return '-'
	}, s)
	s = collapse.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")

	if len(s) > maxNameLength {
		s = strings.Trim(s[:maxNameLength], "-.")
	}
	if s == "" {
		return "unnamed"
	}
	return s
}

// FileName slugs a file name while preserving its extension, which is what
// makes a receipt open in the right application when clicked.
func FileName(name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)

	slug := Slug(base)
	if ext == "" {
		return slug
	}
	// The extension goes through the same sanitiser, so a crafted name cannot
	// smuggle a separator through the part that is not slugged.
	return slug + "." + Slug(strings.TrimPrefix(ext, "."))
}

// uniqueNamer hands out names that do not collide, appending a short digest
// when one already exists. Deterministic: the same inputs in the same order
// always produce the same names, so a rebuilt tree is not a diff.
type uniqueNamer struct {
	taken map[string]bool
}

func newUniqueNamer() *uniqueNamer {
	return &uniqueNamer{taken: map[string]bool{}}
}

// unique returns name, or name with the digest woven in if it is taken. Case
// is folded when checking, because macOS and Windows filesystems do not
// distinguish two names that differ only by it.
func (u *uniqueNamer) unique(dir, name, digest string) string {
	key := strings.ToLower(filepath.Join(dir, name))
	if !u.taken[key] {
		u.taken[key] = true
		return name
	}

	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	short := digest
	if len(short) > 8 {
		short = short[:8]
	}
	withDigest := base + "-" + short + ext

	key = strings.ToLower(filepath.Join(dir, withDigest))
	u.taken[key] = true
	return withDigest
}
