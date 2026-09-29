package chapters

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// RetimeVTT は WebVTT のキュー時刻を原本時間軸からカット後の時間軸へ写す。
//
// 字幕サイドカーは同じ ffmpeg 起動の別出力なので `-filter_complex` の trim が
// 効かない。書き出した後にここで写像する。
//
//   - keep の外に完全に収まるキューは捨てる
//   - keep の境界をまたぐキューは keep 側でクリップする
//
// キューが keep 区間を 2 つ以上にまたぐ場合は 1 本のキューとして写す（間の
// カット区間のぶんも覆う）。字幕は映像の上に重なるテキストなので、切り分けて
// も見え方は変わらない。
func RetimeVTT(data []byte, keep []Range) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var out []string
	// 先頭のヘッダ（WEBVTT 行と、それに続く空行までの任意の行）はそのまま残す。
	i := 0
	for ; i < len(lines); i++ {
		out = append(out, lines[i])
		if i > 0 && strings.TrimSpace(lines[i]) == "" {
			i++
			break
		}
	}
	for i < len(lines) {
		// 空行は読み飛ばす（キュー間の区切り）。
		if strings.TrimSpace(lines[i]) == "" {
			i++
			continue
		}
		// キュー本体は「任意の識別子行 + タイムスタンプ行 + 本文行…」。
		start := i
		for i < len(lines) && !strings.Contains(lines[i], "-->") {
			i++
		}
		if i >= len(lines) {
			return nil, fmt.Errorf("webvtt: cue starting at line %d has no timestamp", start+1)
		}
		header := lines[start:i]
		tsIndex := i
		i++
		bodyStart := i
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			i++
		}
		body := lines[bodyStart:i]

		cueStart, cueEnd, settings, err := parseVTTCueTimes(lines[tsIndex])
		if err != nil {
			return nil, err
		}
		mapped := mapCueTimes(keep, cueStart, cueEnd)
		if mapped == nil {
			continue // keep の外（CM 区間の中に収まる）
		}
		out = append(out, header...)
		out = append(out, formatVTCCue(*mapped, settings))
		out = append(out, body...)
		out = append(out, "")
	}
	// 末尾は必ず 1 つの改行で終える（空行だけが残らないように）。
	for len(out) > 1 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return []byte(strings.Join(out, "\n") + "\n"), nil
}

// mapCueTimes はキューの [start,end) を keep でクリップし、カット後の時間軸へ
// 写す。keep と交わらなければ nil。
func mapCueTimes(keep []Range, start, end int64) *Range {
	if end <= start {
		return nil
	}
	var lo, hi int64
	found := false
	for _, r := range keep {
		if r.EndMs <= start || r.StartMs >= end {
			continue
		}
		if !found {
			lo, hi = max(r.StartMs, start), min(r.EndMs, end)
			found = true
			continue
		}
		hi = max(hi, min(r.EndMs, end))
	}
	if !found {
		return nil
	}
	return &Range{StartMs: MapMs(keep, lo), EndMs: MapMs(keep, hi)}
}

// parseVTTCueTimes は "HH:MM:SS.mmm --> HH:MM:SS.mmm [settings]" 行を読む。
// settings はタイムスタンプの後ろの残り（位置指定など）で、そのまま戻す。
func parseVTTCueTimes(line string) (start, end int64, settings string, err error) {
	before, after, ok := strings.Cut(line, "-->")
	if !ok {
		return 0, 0, "", fmt.Errorf("webvtt: %q has no -->", line)
	}
	start, err = parseVTCTime(strings.TrimSpace(before))
	if err != nil {
		return 0, 0, "", err
	}
	after = strings.TrimSpace(after)
	if i := strings.IndexByte(after, ' '); i >= 0 {
		settings = strings.TrimSpace(after[i:])
		after = after[:i]
	}
	end, err = parseVTCTime(after)
	if err != nil {
		return 0, 0, "", err
	}
	return start, end, settings, nil
}

// parseVTCTime は "HH:MM:SS.mmm" または "MM:SS.mmm" を ms へ読む。
func parseVTCTime(s string) (int64, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("webvtt: bad timestamp %q", s)
	}
	var total int64
	for _, p := range parts {
		if p == "" {
			return 0, fmt.Errorf("webvtt: bad timestamp %q", s)
		}
		last := p == parts[len(parts)-1]
		whole, frac, _ := strings.Cut(p, ".")
		n, err := strconv.ParseInt(whole, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("webvtt: bad timestamp %q", s)
		}
		total = total*60 + n
		if last && frac != "" {
			// 小数は ms として読む。桁が 3 未満なら右を 0 で埋める。
			for len(frac) < 3 {
				frac += "0"
			}
			ms, err := strconv.ParseInt(frac[:3], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("webvtt: bad timestamp %q", s)
			}
			total = total*1000 + ms
			return total, nil
		}
	}
	return total * 1000, nil
}

// formatVTCCue はタイムスタンプ行を組み立てる。
func formatVTCCue(r Range, settings string) string {
	line := formatVTCTime(r.StartMs) + " --> " + formatVTCTime(r.EndMs)
	if settings != "" {
		line += " " + settings
	}
	return line
}

// formatVTCTime は ms を "HH:MM:SS.mmm" へ書く。
func formatVTCTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	s := ms / 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms%1000)
}

// ErrEmptyTimeline はカット版を作れない（keep 区間が 1 つも無い）ことを表す。
var ErrEmptyTimeline = errors.New("timeline has no keep ranges")
