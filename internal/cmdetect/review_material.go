package cmdetect

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	"github.com/fetburner/rokuban/internal/chapters"
)

// parseLogoFrameRanges は logoframe の -oa にある開始・終了イベントを ms 区間へ変換する。
// Dockerfile.full が pin する tobitti0/logoframe の readme.txt はイベント項目と開始・終了が対になる契約を記す。
// 出力の空白区切りはDockerfile.fullがpinする版のソースで確かめた。終端フレーム包含は未検証。
func parseLogoFrameRanges(output string, durationMs int64) ([]chapters.Range, error) {
	frames := make([]frameRange, 0)
	var open *int64
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("invalid logoframe interval line %q", line)
		}
		frame, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing logoframe frame %q: %w", fields[0], err)
		}
		marker := strings.ToUpper(fields[1])
		if marker != "S" && marker != "E" {
			return nil, fmt.Errorf("invalid logoframe marker %q", fields[1])
		}
		if marker == "S" {
			if open != nil {
				return nil, fmt.Errorf("logoframe start without preceding end")
			}
			start := frame
			open = &start
			continue
		}
		if open == nil || frame < *open {
			return nil, fmt.Errorf("logoframe end without matching start")
		}
		frames = append(frames, frameRange{start: *open, end: frame + 1})
		open = nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading logoframe output: %w", err)
	}
	if open != nil {
		return nil, fmt.Errorf("logoframe start has no matching end")
	}
	return chapterRangesFromFrames(frames, durationMs), nil
}

// parseStructureOutput は Dockerfile.full が pin する join_logo_scp v5.1.1 の -oscp を読む。
// readme の6項目を使う。空白区切りとラベル文字列はpinした版の出力で確かめた値で、文書の契約ではない。
// ラベルはAuto系コマンド時のみ出るため5項目も受け付ける。終端フレームは要素に含める。pinした版で実録画1本を
// 検出し、終端の次のフレームが次の要素の開始と一致して要素が隙間なく続くことを確かめた（文書の契約ではない）。
func parseStructureOutput(output string, durationMs int64) ([]chapters.CMElement, error) {
	frames := make([]frameRange, 0)
	var errors []int64
	var labels []string
	scanner := bufio.NewScanner(strings.NewReader(output))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 5 && len(fields) != 6 {
			return nil, fmt.Errorf("join_logo_scp structure line %d has %d fields, want 5 or 6", lineNo, len(fields))
		}
		start, startErr := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
		end, endErr := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if startErr != nil || endErr != nil {
			return nil, fmt.Errorf("parsing join_logo_scp frame interval at line %d", lineNo)
		}
		for _, index := range []int{2, 3, 4} {
			if _, err := strconv.ParseInt(strings.TrimSpace(fields[index]), 10, 64); err != nil {
				return nil, fmt.Errorf("parsing join_logo_scp numeric field %d at line %d: %w", index+1, lineNo, err)
			}
		}
		errorFrames, _ := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
		if end < start {
			return nil, fmt.Errorf("join_logo_scp interval at line %d ends before it starts", lineNo)
		}
		frames = append(frames, frameRange{start: start, end: end + 1})
		errors = append(errors, errorFrames)
		label := ""
		if len(fields) == 6 {
			label = fields[5]
		}
		labels = append(labels, label)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading join_logo_scp structure output: %w", err)
	}
	converted := chapterRangesFromFrames(frames, durationMs)
	if len(converted) != len(frames) {
		return nil, fmt.Errorf("join_logo_scp structure interval lies outside recording duration")
	}
	elements := make([]chapters.CMElement, len(frames))
	for i, interval := range converted {
		elements[i] = chapters.CMElement{StartMs: interval.StartMs, EndMs: interval.EndMs, ErrorFrames: errors[i], Label: labels[i]}
	}
	return elements, nil
}
