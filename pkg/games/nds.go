package games

import (
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

const ndsExt = ".nds"

func NDSFileName(rawURL string, requested string, fallback string) string {
	name := strings.TrimSpace(requested)
	if name == "" {
		if u, err := url.Parse(rawURL); err == nil {
			if u.Opaque != "" {
				name, _ = url.PathUnescape(path.Base(u.Opaque))
			} else {
				name, _ = url.PathUnescape(path.Base(u.Path))
			}
		}
	}
	if name == "" || name == "." || name == "/" {
		name = fallback
	}

	name = filepath.Base(name)
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ndsExt {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}

	name = SafeName(name, strings.TrimSuffix(fallback, filepath.Ext(fallback)))
	return name + ndsExt
}

func GameNameFromFile(fileName string) string {
	return strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
}

func SafeName(name string, fallback string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = fallback
	}

	var b strings.Builder
	lastDash := false
	for _, r := range name {
		if isSafeNameRune(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	out := strings.Trim(b.String(), " .-")
	if out == "" {
		return fallback
	}
	return out
}

func isSafeNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' ||
		r >= 'A' && r <= 'Z' ||
		r >= '0' && r <= '9' ||
		r == ' ' ||
		r == '.' ||
		r == '_' ||
		r == '-' ||
		r == '(' ||
		r == ')' ||
		r == '[' ||
		r == ']'
}
