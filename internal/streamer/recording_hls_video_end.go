package streamer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/fetburner/rokuban/internal/ffargs"
)

const (
	// recordingHLSPTSModulus は MPEG-TS の PTS が巻き戻る周期（2^33 tick、約 26.5 時間）。
	recordingHLSPTSModulus = int64(1) << 33
	// recordingHLSPTSLeadSlack は、原点より前の PTS を負の値として扱う幅（60 秒）。
	// 先頭の映像パケットの PTS が、並べ替えで後ろのパケットより大きいことがあるため、
	// 原点のすぐ手前を 26.5 時間後と読まないようにする。
	recordingHLSPTSLeadSlack = 60 * mpegTSTimestampClock
)

// recordingHLSTailWindows は、ファイルの末尾から ffprobe に渡すバイト数の候補。
// 窓に映像パケットが 1 つも無ければ、次の大きさで測り直す。
var recordingHLSTailWindows = []int64{4 << 20, 32 << 20, 256 << 20}

// errRecordingHLSNoVideoPacket は、最大の窓にも映像パケットが無かったことを表す。
var errRecordingHLSNoVideoPacket = errors.New("recording HLS tail window has no video packet")

// measureRecordingHLSVideoDuration は、ファイル末尾の映像パケットの最大の pts + duration から
// 原点を引いた映像の長さ（90 kHz tick）を返す。temp と原本の両方がこの 1 つの定義を使う。
//
// 末尾の窓だけを ffprobe の標準入力に渡す。/dev/fd/N で fd を渡すと、Linux では通常ファイルを
// 先頭から開き直すので、末尾だけを渡したことにならない。Go は TS を解釈しない。
// stream の duration の推定には頼らない。
func measureRecordingHLSVideoDuration(ctx context.Context, ffprobe string, file *os.File, size, originPTS int64) (int64, error) {
	probeCtx, cancel := context.WithTimeout(ctx, recordingHLSProbeTimeout)
	defer cancel()
	ffprobe = ffargs.FFprobePath(ffprobe)
	var lastErr error
	for _, window := range recordingHLSTailWindows {
		offset := max(size-window, 0)
		cmd := exec.CommandContext(probeCtx, ffprobe,
			"-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts,duration",
			"-of", "compact=p=0", "-i", "pipe:0",
		)
		cmd.Stdin = io.NewSectionReader(file, offset, size-offset)
		output, err := cmd.Output()
		if err != nil {
			if probeCtx.Err() != nil {
				return 0, probeCtx.Err()
			}
			// 映像を含まない窓では、ffprobe がストリームを見つけられず失敗する。
			// ファイル全体を渡しても失敗するなら、それは測定の失敗である。
			lastErr = fmt.Errorf("probing the tail of recording HLS input: %w", err)
			if offset == 0 {
				return 0, lastErr
			}
			continue
		}
		if duration, ok := recordingHLSVideoDurationFromPackets(string(output), originPTS); ok {
			return duration, nil
		}
		lastErr = nil
		if offset == 0 {
			break
		}
	}
	if lastErr != nil {
		return 0, fmt.Errorf("%w: %w", errRecordingHLSNoVideoPacket, lastErr)
	}
	return 0, errRecordingHLSNoVideoPacket
}

// recordingHLSVideoDurationFromPackets は、ffprobe の compact 出力（1 行 1 パケットの
// `pts=…|duration=…`）から、原点を 0 とした映像の末尾を返す。pts は 2^33 を法として原点からの
// 差を取るので、窓が巻き戻りをまたいでも、巻き戻りの後ろだけの窓でも同じ長さになる。
// pts の無いパケット（窓の先頭で欠けた PES など）は読み飛ばす。
//
// 窓だけを渡した ffprobe は duration を N/A で返すことがある（実測: 64 KiB の窓）。その場合は、
// 最大の 2 つの pts の差（フレーム間隔）を duration とする。
func recordingHLSVideoDurationFromPackets(output string, originPTS int64) (int64, bool) {
	type packet struct {
		relative, duration int64
		hasDuration        bool
	}
	var packets []packet
	for _, line := range strings.Split(output, "\n") {
		var p packet
		hasPTS := false
		var pts int64
		for _, field := range strings.Split(strings.TrimSpace(line), "|") {
			name, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				continue
			}
			switch name {
			case "pts":
				pts, hasPTS = parsed, true
			case "duration":
				p.duration, p.hasDuration = max(parsed, 0), parsed > 0
			}
		}
		if !hasPTS {
			continue
		}
		p.relative = ((pts-originPTS+recordingHLSPTSLeadSlack)%recordingHLSPTSModulus+recordingHLSPTSModulus)%recordingHLSPTSModulus - recordingHLSPTSLeadSlack
		packets = append(packets, p)
	}
	if len(packets) == 0 {
		return 0, false
	}
	var top, second int64 = math.MinInt64, math.MinInt64
	for _, p := range packets {
		switch {
		case p.relative > top:
			top, second = p.relative, top
		case p.relative < top && p.relative > second:
			second = p.relative
		}
	}
	var interval int64
	if second != math.MinInt64 {
		interval = top - second
	}
	var end int64 = math.MinInt64
	for _, p := range packets {
		duration := p.duration
		if !p.hasDuration {
			duration = interval
		}
		end = max(end, p.relative+duration)
	}
	return end, end > 0
}
