package reconciler

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var unsafeChars = regexp.MustCompile(`[/\\:\*\?"<>\|` + "\x00-\x1f\x7f" + `]`)

func sanitizeComponent(s string, maxLen int) string {
	s = unsafeChars.ReplaceAllString(s, "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.TrimSpace(s)
	if maxLen > 0 && utf8.RuneCountInString(s) > maxLen {
		runes := []rune(s)
		s = string(runes[:maxLen])
	}
	if s == "" || s == "." {
		s = "_"
	}
	return s
}

func generateContentPath(title string, startAt time.Time, serviceID int) string {
	date := startAt.Format("20060102")
	timeStr := startAt.Format("150405")
	safeTitle := sanitizeComponent(title, 80)
	return fmt.Sprintf("%s/%s_%s_%d.m2ts", date, timeStr, safeTitle, serviceID)
}
