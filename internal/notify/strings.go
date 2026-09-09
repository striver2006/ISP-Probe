package notify

import "strings"

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func lastSeg(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 || i == len(p)-1 {
		return ""
	}
	return p[i+1:]
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
