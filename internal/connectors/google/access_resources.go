package google

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxAccessResources = 128
const MaxAccessResourceNameBytes = 128

// ValidateAccessResources returns an isolated canonical administrator-owned map.
// Resource matching uses TrimSpace and EqualFold, never keywords or URLs.
func ValidateAccessResources(resources map[string]string) (map[string]string, error) {
	if len(resources) > MaxAccessResources {
		return nil, errors.New("access_help.resources must contain at most 128 entries")
	}
	copy := make(map[string]string, len(resources))
	for name, email := range resources {
		if !validResourceName(name) {
			return nil, errors.New("access_help resource names must be nonblank valid UTF-8, at most 128 bytes, without control characters")
		}
		name = strings.TrimSpace(name)
		for existing := range copy {
			if strings.EqualFold(existing, name) {
				return nil, errors.New("access_help.resources contains duplicate normalized names")
			}
		}
		if !validAccessEmail(email) {
			return nil, errors.New("access_help resource groups must be plain email addresses")
		}
		copy[name] = strings.ToLower(email)
	}
	return copy, nil
}

func validResourceName(name string) bool {
	if len(name) > MaxAccessResourceNameBytes || !utf8.ValidString(name) || strings.TrimSpace(name) == "" {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Intentionally excludes display names, quoted local parts, URL delimiters,
// Unicode domains, and percent escapes. These values become Directory keys.
func validAccessEmail(email string) bool {
	if len(email) > 254 || strings.Count(email, "@") != 1 {
		return false
	}
	parts := strings.Split(email, "@")
	local, host := parts[0], parts[1]
	if len(local) == 0 || len(local) > 64 || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for _, r := range local {
		if !asciiAlnum(r) && !strings.ContainsRune("._+-", r) {
			return false
		}
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !asciiAlnum(r) && r != '-' {
				return false
			}
		}
	}
	return true
}

func asciiAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}
