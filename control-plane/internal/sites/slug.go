package sites

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// maxSlugBase bounds the part of a slug derived from the site name. With a
// suffix it stays well within a 63-character DNS label, and the preview
// domain stays readable.
const maxSlugBase = 40

// numberedSlugs is how many numbered variants (acme-2 ... acme-100) are tried
// before falling back to a random suffix.
const numberedSlugs = 100

// Slugify derives the base of a site's slug from its name: lowercase ASCII
// letters and digits, with every other run of characters turned into a
// single hyphen. Accents are dropped rather than replacing the letter
// ("Café Crème" becomes "cafe-creme"). A name with nothing usable gives
// "site".
func Slugify(name string) string {
	var b strings.Builder
	pendingDash := false
	for _, r := range norm.NFKD.String(strings.ToLower(name)) {
		switch {
		case unicode.Is(unicode.Mn, r):
			// A combining mark that NFKD split off its letter.
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if pendingDash && b.Len() > 0 {
				if b.Len()+1 >= maxSlugBase {
					return finishSlug(b.String())
				}
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(r)
			if b.Len() >= maxSlugBase {
				return finishSlug(b.String())
			}
		default:
			pendingDash = true
		}
	}
	return finishSlug(b.String())
}

func finishSlug(s string) string {
	if s = strings.Trim(s, "-"); s == "" {
		return "site"
	}
	return s
}

// uniqueSlug returns the first free slug for base: base itself, then base-2,
// base-3, and so on, and after numberedSlugs a random suffix. A slug is free
// if no site has it and no site has the preview domain it would give. Run it
// in the transaction that inserts the site; the unique constraints catch a
// concurrent creation that picks the same slug, and the caller retries.
func uniqueSlug(ctx context.Context, sites store.Sites, base, previewBase string) (string, error) {
	for i := 1; i <= numberedSlugs+1; i++ {
		slug := base
		switch {
		case i > numberedSlugs:
			slug = base + "-" + strings.ToLower(rand.Text()[:6])
		case i > 1:
			slug = base + "-" + strconv.Itoa(i)
		}
		taken, err := sites.SlugTaken(ctx, slug, previewDomain(slug, previewBase))
		if err != nil {
			return "", err
		}
		if !taken {
			return slug, nil
		}
	}
	return "", fmt.Errorf("no free slug for %q", base)
}

func previewDomain(slug, previewBase string) string {
	return slug + "." + strings.TrimSuffix(previewBase, ".")
}
