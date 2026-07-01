package tune

import "strings"

// splitLines + trimSpace helpers (string versions of strings equivalents so
// the tune package has no stdlib surprises across Go versions).
func splitLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func trimSpace(s string) string {
	return strings.TrimSpace(s)
}