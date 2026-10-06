package streamer

// hls_args.go は FFmpeg の HLS 引数の組み立てを持つ。

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/ffargs"
)

// BuildLiveFFmpegArgs は設定済みの全プロファイルを 1 回の ffmpeg 起動で HLS に
// 出す引数を組み立てる（issue #91 の決定 1: 1 チューナーから複数プロファイル）。
//
// 自由形式の cmd 文字列は受け取らない（encode.BuildFFmpegArgs と同じ方針）。
// ランタイムでは ffprobe が数えた音声 ES 数を使い、複数なら 2 本目も map する。
// この公開 builder は dual mono 用の 1 ES として引数を組み立てる。字幕は通常 map
// しない。Captions=true の専用経路だけ optional に ARIB caption を map し、
// libaribcaption で WebVTT にする。既定経路は Debian 系 ffmpeg でも従来どおり動く。
//
// **音声はプロファイルごとに 3 本の代替音声レンディション（標準 / 主 / 副）で出す。**
// 標準はフィルタ無し（`DEFAULT=YES`）。単一 ES は主 / 副を pan し、複数 ES は
// 2 本目を副へ割り当てる。選ぶのはプレイヤーで、サーバーは選択を知らない
// （docs/api/media.md §音声）。
//
// argv の順序（issue #321 決定コメント §3）:
//
//	-hide_banner -nostats -loglevel error
//	[cfg.HWAccel ブロック]                          # 入力 1 本ぶん、1 回だけ
//	-probesize 5M -analyzeduration 3M
//	[cfg.InputExtraArgs…]
//	-f mpegts -i pipe:0
//	  ── プロファイルごとに繰り返し ──
//	  -map 0:v:0 -map 0:a:0 ×2 -map 0:a:0|0:a:1  -c:v  -c:a [single ES の pan]
//	  [-vf <deinterlace[, scaler が決めた scale]>]  [-crf|-qp]  [-preset]
//	  （captions 経路では `-c:a:N` / `-filter:v:N` / `-filter:a:N` を使う）
//	  -force_key_frames expr:…
//	  [profile.extra_args…]                         # ユーザー（出力側）
//	  -var_stream_map … -master_pl_name NAME.m3u8 -f hls ... NAME.%v.m3u8  # アプリ所有の末尾
//
// **既定経路はプロファイルごとの出力のまま、各出力が自分の master（`NAME.m3u8`）を
// 持つ。** 1 つの master にまとめると `hls_time` が 1 つになり、プロファイルごとの
// `segment_seconds` が表現できなくなる（captions 経路はそのため検証で揃えさせている）。
// variant のファイル名は `NAME.<n>.m3u8` で、プロファイル名に `.` は使えないので
// 別プロファイルの master と衝突しない。
//
// **Captions=true のときは 1 つの master playlist（%v 展開）を出す形に分岐する。**
// withSubtitles は Captions=true のときだけ効き、起動前の ffprobe 判定結果を渡す
// （false なら字幕 map / rendition を完全に省き、字幕の無い番組でも映像・音声の
// HLS を継続できる）。Captions=false のときは無視される。
func BuildLiveFFmpegArgs(cfg LiveConfig, dir string, withSubtitles bool) []string {
	return buildHLSFFmpegArgsForPlaylistType(cfg, dir, withSubtitles, hlsLivePlaylist, "pipe:0", 0, 1)
}

// BuildChaseFFmpegArgs は live と同じ画質・音声 rendition を出し、EVENT playlist にする。
// EVENT は録画履歴全体を残すため delete_segments を使わない。
//
// **追っかけもライブ・原本 HLS と同じ音声選択（標準 / 主 / 副）を提供する。** EVENT は
// segment を保持するためライブ窓のずれは起きない。実ブラウザの切替・シーク・再開の
// 判定は web/e2e/chase-audio.mjs が担う。
func BuildChaseFFmpegArgs(cfg LiveConfig, dir string, withSubtitles bool) []string {
	return buildHLSFFmpegArgsForPlaylistType(cfg, dir, withSubtitles, hlsEventPlaylist, "pipe:0", 0, 1)
}

// BuildOriginalVODFFmpegArgs converts the original MPEG-2 TS into an EVENT HLS
// playlist that grows while ffmpeg converts and gains ENDLIST at EOF. It is not
// `-hls_playlist_type vod`: ffmpeg 9.0.2 writes no .m3u8 until it exits in that
// mode (measured), so a recording longer than playlistStartupTimeout would never
// become playable. Every segment is kept until shared idle GC, and the output
// has the same profile, audio rendition, and optional subtitle graph as live.
// offsetSeconds is mapped down to the 30000/1001 fps input frame grid before
// accurate input-side -ss, and the video encoder gets -bf 0. Measured only on
// the synthetic MPEG-2 fixture of web/e2e/recording-playback-timeline.mjs with
// libx264 (Chrome shows hls.js frames 66.73 ms late without -bf 0; WebKit's
// native HLS showed no difference). Unverified: the captions path's effect in a
// browser, hardware encoders, and recordings whose audio lead has a phase other
// than the fixture's (audio start 10.4067 s, video start 11.1007 s; an integer
// -ss showed frames 33.37 ms early there). fd 3 is the original opened by the Go
// process and passed via Cmd.ExtraFiles, so unlinking its canonical path cannot
// break the session.
func BuildOriginalVODFFmpegArgs(cfg LiveConfig, dir string, withSubtitles bool, offsetSeconds int64) []string {
	return buildHLSFFmpegArgsForPlaylistType(
		cfg, dir, withSubtitles, hlsOriginalEventPlaylist, originalVODFFmpegInputPath, offsetSeconds, 1,
	)
}

// appendMPEGTSInput は MPEG-TS 入力（`-f mpegts [-ss N] -i path`）を args に足す。
// offsetSeconds > 0 のときだけ入力側シークを付ける。
func appendMPEGTSInput(args []string, inputPath string, offsetSeconds int64) []string {
	return appendMPEGTSInputWithSeek(args, inputPath, offsetSeconds, strconv.FormatInt(offsetSeconds, 10))
}

func appendOriginalVODMPEGTSInput(args []string, inputPath string, offsetSeconds int64) []string {
	if offsetSeconds <= 0 {
		return appendMPEGTSInput(args, inputPath, 0)
	}
	frame := offsetSeconds * chapters.FrameNumerator / chapters.FrameDenominator
	seekSeconds := float64(frame) * float64(chapters.FrameDenominator) / float64(chapters.FrameNumerator)
	return appendMPEGTSInputWithSeek(args, inputPath, offsetSeconds, fmt.Sprintf("%.9f", seekSeconds))
}

func appendMPEGTSInputWithSeek(args []string, inputPath string, offsetSeconds int64, seek string) []string {
	args = append(args, "-f", "mpegts")
	if offsetSeconds > 0 {
		args = append(args, "-ss", seek)
	}
	return append(args, "-i", inputPath)
}

type hlsPlaylistType uint8

func buildHLSFFmpegArgsForPlaylistType(
	cfg LiveConfig,
	dir string,
	withSubtitles bool,
	playlistType hlsPlaylistType,
	inputPath string,
	offsetSeconds int64,
	audioStreamCount int,
) []string {
	originalVOD := playlistType == hlsOriginalEventPlaylist
	if cfg.Captions {
		return buildLiveCaptionFFmpegArgsForPlaylistType(
			cfg, dir, withSubtitles, playlistType, inputPath, offsetSeconds, audioStreamCount,
		)
	}
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
	}
	args = append(args, cfg.HWAccel.Args()...)
	args = append(args,
		// pipe の MPEG-TS は PAT/PMT が揃うまで寸法 0x0 に見える窓がある。
		// 既定 probesize だと誤判定しやすいので少し延ばす。playlistStartupTimeout
		// （15s）を食いつぶさないよう、analyzeduration は数秒に留める。
		"-probesize", "5M",
		"-analyzeduration", "3M",
	)
	args = append(args, cfg.InputExtraArgs...)
	if originalVOD {
		args = appendOriginalVODMPEGTSInput(args, inputPath, offsetSeconds)
	} else {
		args = appendMPEGTSInput(args, inputPath, offsetSeconds)
	}
	for _, p := range cfg.Profiles {
		// 映像・音声だけ。字幕 / データ放送は捨てる（上記 arib_caption）。
		// -map は output 単位のオプションなので、ループの前に 1 組だけ置くと
		// 最初の .m3u8 にしか適用されず、2 本目以降は自動ストリーム選択に戻る。
		args = append(args, "-map", "0:v:0")
		args = appendAudioRenditionMaps(args, audioStreamCount)
		args = append(args, "-c:v", p.VideoCodec, "-c:a", p.AudioCodec)
		args = appendAudioRenditionFilters(args, 0, audioStreamCount)
		if originalVOD {
			// With B frames, Chrome/hls.js showed frames 2 frames (66.73 ms) behind the
			// original MP4 timeline on the synthetic fixture (e2e
			// recording-playback-timeline, libx264). Unverified for hardware encoders.
			args = append(args, "-bf", "0")
		}
		if filter, ok := ffargs.VideoFilterArgs(p.Scaler, p.Height, p.Deinterlace); ok {
			args = append(args, "-vf", filter)
		}
		args = append(args, ffargs.QualityArgs(p.CRF, p.QP)...)
		if p.Preset != "" {
			args = append(args, "-preset", p.Preset)
		}
		// キーフレームをセグメント境界に合わせる。合わせないと HLS のセグメント
		// カットが GOP 境界を無視し、再生開始位置がずれる/コマ落ちする。
		args = append(args, "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", p.SegmentSeconds))
		if len(p.ExtraArgs) > 0 {
			args = append(args, p.ExtraArgs...)
		}
		playlistSize := strconv.Itoa(p.PlaylistSize)
		playlistOptions := []string{}
		if playlistType != hlsLivePlaylist {
			// EVENT playlists grow from the head until ffmpeg sees EOF. list_size 0
			// and the absence of delete_segments retain every segment for seeking.
			playlistSize = "0"
			playlistOptions = []string{"-hls_playlist_type", "event"}
		}
		// 出力ファイル名は master（NAME.m3u8）と variant（NAME.<n>.m3u8）。
		// 字幕付きは playlist.m3u8 と playlist_<n>.m3u8。
		variants := append([]string{"v:0,agroup:aud"}, audioRenditionEntries(0, "aud")...)
		args = append(args, "-var_stream_map", strings.Join(variants, " "), "-master_pl_name", p.Name+".m3u8")
		segmentFile, playlistFile := p.Name+".%v_seg%05d.ts", p.Name+".%v.m3u8"
		args = append(args,
			"-f", "hls",
			"-hls_time", strconv.Itoa(p.SegmentSeconds),
			"-hls_list_size", playlistSize,
		)
		args = append(args, playlistOptions...)
		args = append(args,
			// delete_segments: プレイリスト長を超えた古いセグメントを削除する
			//（プロセスが落ちても残骸を溜め続けない。正常系の掃除）。
			// temp_file: 一時ファイルに書いてから rename するので、配信側が
			// 書き込み途中のファイルを読むことがない。追っかけ再生は
			// delete_segments を使わない（BuildChaseFFmpegArgs）。
			"-hls_flags", hlsFlagsForPlaylistType(playlistType),
			"-hls_segment_filename", filepath.Join(dir, "segments", segmentFile),
			// hls_base_url: プレイリストの各セグメント行に付ける接頭辞。
			// **これが無いと ffmpeg は basename だけを書く**（実機で確認済み）。
			// HLS クライアントはプレイリスト自身の URL 基準で相対解決するため、
			// basename のままだと `.../live/h264_seg00001.ts` を要求してしまい、
			// このサーバーが実際に配信するルート（`.../live/segments/{name}`）と
			// 食い違って 404 になる。`-hls_segment_filename` が書き込む物理パス
			// （`segments/` サブディレクトリ）と、プレイリストが指す論理 URI を
			// 一致させるための必須フラグ（issue #91 のレビューで発見）。
			"-hls_base_url", "segments/",
			filepath.Join(dir, playlistFile),
		)
	}
	return args
}

// appendAudioRenditionMaps appends input maps for the standard, main, and sub
// renditions. A single audio ES carries both channels of dual-mono audio; with
// separate ESs, the first is standard/main and the second is sub.
func appendAudioRenditionMaps(args []string, audioStreamCount int) []string {
	inputs := [3]string{"0:a:0", "0:a:0", "0:a:0"}
	if audioStreamCount >= 2 {
		inputs[2] = "0:a:1"
	}
	for _, input := range inputs {
		args = append(args, "-map", input)
	}
	return args
}

// appendAudioRenditionFilters adds the pan filters used when main/sub are the
// left and right channels of one dual-mono audio ES. Separate audio ESs already
// contain the selected language, so those renditions keep the source channels.
func appendAudioRenditionFilters(args []string, firstAudioOutput, audioStreamCount int) []string {
	if audioStreamCount >= 2 {
		return args
	}
	return append(args,
		"-filter:a:"+strconv.Itoa(firstAudioOutput+1), dualMonoPans[0],
		"-filter:a:"+strconv.Itoa(firstAudioOutput+2), dualMonoPans[1],
	)
}

// dualMonoPans は二重音声の主（L）/ 副（R）を両耳へ写す出力側のフィルタ。
//
// 既定のデコード（`-dual_mono_mode` 無し）は二重音声を L = 主 / R = 副のステレオで
// 出すので、出力側で片側を両耳へ写せば主 / 副になる。**`-dual_mono_mode` は入力
// （デコーダ）側のオプションなので 1 回の起動で両方を出せないが、これなら出せる。**
// 実測（ffmpeg 9.0.2）: モノラル AAC 2 本の SCE を 1 フレームに継いだ二重音声で、
// この pan の出力は `-dual_mono_mode main|sub` の出力とバイト一致した。二重音声で
// ない通常のステレオに当てると片側のチャンネルだけになる（利用者が選んだときだけ）。
var dualMonoPans = [2]string{"pan=stereo|c0=c0|c1=c0", "pan=stereo|c0=c1|c1=c1"}

// audioRenditionEntries は 1 プロファイルぶんの音声レンディション（標準 / 主 / 副）の
// `-var_stream_map` 項目を返す。first はその最初の音声出力ストリームの index。
//
// **並び順が UI との契約である。** master の `NAME` は ffmpeg が `audio_<n>` で固定し、
// n はプロファイル数でずれる（`name:` で変わるのは URI だけ。実測）。フロントは
// グループ内の順序（0 = 標準 / 1 = 主 / 2 = 副）で選ぶ（web/src/lib/live.ts
// liveAudioTrackIndex）。
func audioRenditionEntries(first int, group string) []string {
	return []string{
		fmt.Sprintf("a:%d,agroup:%s,default:yes", first, group),
		fmt.Sprintf("a:%d,agroup:%s", first+1, group),
		fmt.Sprintf("a:%d,agroup:%s", first+2, group),
	}
}

// hlsFlags は `-hls_flags` の値を返す。
//
// **ライブは program_date_time が要る。** 無いと hls.js（1.7.1 / 1.7.3 / canary）は、
// 前に聴いた音声レンディションへ**ライブの窓（list_size × hls_time）より後で**戻ると
// 再生が止まる（バッファが空になり、音声は前のトラックのまま。手元に残った古い
// playlist が今の窓と重ならず、PDT 無しでは揃えられない）。窓の内側ですぐ戻る分には
// 止まらない。判定は `web/e2e/live-audio.mjs` の ①（各トラックを 15 秒聴いてから
// 戻る。PDT を外すと標準へ戻る所で落ち、付けると通る。WebAudio で左右の周波数を
// 測る）。追っかけは EVENT playlist で segment を消さず窓がスライドしないので、PDT が
// 無くても各トラックを 15 秒聴いて戻っても止まらない（`web/e2e/chase-audio.mjs`。
// hls.js が何で位置を揃えているかは測っていない）。
func hlsFlags(eventPlaylist bool) string {
	if eventPlaylist {
		return "temp_file"
	}
	return "delete_segments+temp_file+program_date_time"
}

func hlsFlagsForPlaylistType(playlistType hlsPlaylistType) string {
	return hlsFlags(playlistType != hlsLivePlaylist)
}

// buildLiveCaptionFFmpegArgsForPlaylistType は HLS を 1 つの master playlist として出力する。
// %v はプロファイルごとの video/audio variant を表す。withSubtitles は起動前の
// ffprobe 判定結果で、false の場合は字幕 map / rendition を完全に省き、字幕なし
// 番組でも映像・音声の HLS を継続できる。
//
// **音声レンディションはプロファイルごとのグループ（`agroup:a<N>`）に入れる。**
// プロファイルごとに `audio_codec` / `extra_args` が違いうるので、1 グループに
// まとめるとそれが表現できない。プロファイル N の音声は a:3N（標準）/ a:3N+1（主）/
// a:3N+2（副）。video variant が先に並ぶので字幕 playlist は `subtitles_0.m3u8` のまま。
//
// **per-stream 指定子は必ず型付き（`:v:N` / `:a:N`）にする。** この経路の出力
// ストリーム順は v0, a0..a2, [s0,] v1, a3..a5 で、`-preset:N` / `-vf:N`（型無しの
// グローバル出力ストリーム index）は 2 本目以降のプロファイルでは音声側を指して
// しまい、preset もフィルタも掛からない（実 ffmpeg で測定・固定: レビュー指摘）。
//
// **フィルタは `-filter:v:N`（`-vf` の完全形）にする。**`-vf:v:N`（`-vf` に型
// 付き specifier を重ねる書き方）は ffmpeg 9.0.1 で単一出力・複数 video map の
// 構成において機能しない（specifier が意図通り分離されず、最後に指定した
// フィルタが両方の video ストリームに適用されて警告が出ることを実測で確認。
// `-c:v:N` や `-preset:v:N` のような型を伴わない他オプションでの `:v:N` 付与は
// 問題なく機能する --- `-vf`/`-filter:v` だけの挙動）。
func buildLiveCaptionFFmpegArgsForPlaylistType(
	cfg LiveConfig,
	dir string,
	withSubtitles bool,
	playlistType hlsPlaylistType,
	inputPath string,
	offsetSeconds int64,
	audioStreamCount int,
) []string {
	originalVOD := playlistType == hlsOriginalEventPlaylist
	args := []string{"-hide_banner", "-nostats", "-loglevel", "error"}
	args = append(args, cfg.HWAccel.Args()...)
	args = append(args, "-probesize", "5M", "-analyzeduration", "3M")
	args = append(args, cfg.InputExtraArgs...)
	if withSubtitles {
		// **ARIB 字幕は duration を持たない。** これが無いと WebVTT の終了時刻が
		// 全 cue で約 1193 時間になり、字幕が一度出たら消えず積み重なる（実測:
		// NHK Eテレの実 TS で `00:21.605 --> 1193:03:08.900`）。
		// 入力側オプションなので -i より前に置く。
		args = append(args, "-fix_sub_duration")
	}
	if originalVOD {
		args = appendOriginalVODMPEGTSInput(args, inputPath, offsetSeconds)
	} else {
		args = appendMPEGTSInput(args, inputPath, offsetSeconds)
	}

	var variants, audioVariants []string
	for i, p := range cfg.Profiles {
		args = append(args, "-map", "0:v:0")
		args = appendAudioRenditionMaps(args, audioStreamCount)
		if i == 0 && withSubtitles {
			args = append(args, "-map", "0:s:0?")
		}
		a := 3 * i
		args = append(args, "-c:v:"+strconv.Itoa(i), p.VideoCodec)
		if originalVOD {
			args = append(args, "-bf:v:"+strconv.Itoa(i), "0")
		}
		for output := 0; output < 3; output++ {
			stream := a + output
			args = append(args, "-c:a:"+strconv.Itoa(stream), p.AudioCodec)
			if audioStreamCount < 2 && output > 0 {
				args = append(args, "-filter:a:"+strconv.Itoa(stream), dualMonoPans[output-1])
			}
		}
		if filter, ok := ffargs.VideoFilterArgs(p.Scaler, p.Height, p.Deinterlace); ok {
			args = append(args, "-filter:v:"+strconv.Itoa(i), filter)
		}
		args = append(args, ffargs.QualityArgs(p.CRF, p.QP)...)
		if p.Preset != "" {
			args = append(args, "-preset:v:"+strconv.Itoa(i), p.Preset)
		}
		args = append(args, "-force_key_frames:v:"+strconv.Itoa(i), fmt.Sprintf("expr:gte(t,n_forced*%d)", p.SegmentSeconds))
		if i == 0 && withSubtitles {
			// -fix_sub_duration だけだと「次の字幕が来るまで現在の cue を出さない」
			// ので、ライブでは画面に出ている字幕がセグメントに載らない（実測:
			// 同じ 30 秒で cue 5 本 → 4 本に減る）。heartbeat を映像 variant 0 に
			// 付けると random access point で cue を分割して吐くため、途中参加した
			// 視聴者にも現在の字幕が届く（実測: 同じ 30 秒で 8 本、セグメント境界で
			// 分割される）。値を取らないフラグである。
			args = append(args, "-fix_sub_duration_heartbeat:v:0")
		}
		args = append(args, p.ExtraArgs...)
		group := "a" + strconv.Itoa(i)
		mapping := fmt.Sprintf("v:%d,agroup:%s", i, group)
		audioVariants = append(audioVariants, audioRenditionEntries(a, group)...)
		if i == 0 && withSubtitles {
			mapping += ",s:0,sgroup:subs"
		}
		variants = append(variants, mapping)
	}
	playlistSize := strconv.Itoa(cfg.Profiles[0].PlaylistSize)
	playlistOptions := []string{}
	if playlistType != hlsLivePlaylist {
		playlistSize = "0"
		playlistOptions = []string{"-hls_playlist_type", "event"}
	}
	args = append(args,
		"-var_stream_map", strings.Join(append(variants, audioVariants...), " "),
		"-master_pl_name", "playlist.m3u8",
		"-f", "hls",
		"-hls_time", strconv.Itoa(cfg.Profiles[0].SegmentSeconds),
		"-hls_list_size", playlistSize,
	)
	args = append(args, playlistOptions...)
	args = append(args,
		"-hls_flags", hlsFlagsForPlaylistType(playlistType),
		"-hls_base_url", "segments/",
		"-hls_segment_filename", filepath.Join(dir, "segments", "%v_seg%05d.ts"),
	)
	if withSubtitles {
		// hls_subtitle_path は字幕プレイリストのファイルパスであり、VTT
		// セグメント自体は muxer が通常の出力ディレクトリ（dir）へ書く。
		// master からの相対 URI は segments/ を付けるため、配信側では
		// .m3u8/.vtt を dir 直下から読む。
		//
		// **%v が要る。** variant が 2 本以上あると ffmpeg は
		// `-hls_subtitle_path` にも %v（またはサブディレクトリでの %v）を
		// 要求し、無いと `hls` マルチプレクサの初期化自体に失敗して
		// **HLS 出力を一切書かずに終了する**（実測: `More than 1 variant
		// streams are present, %v is expected...` で exit 234。字幕付き
		// ライブは複数プロファイルが既定の構成であり、%v を欠くと captions
		// 有効化そのものが機能しなくなる致命的な回帰だったため、G の一部として
		// ここで直す）。字幕 rendition は 1 本しか無い（variant 0 の s:0 だけを
		// map している）ので、実際に作られる字幕 playlist は `subtitles_0.m3u8`
		// 1 本だけで、他の variant 分のファイルは作られない（実測: 2 プロファイル
		// で `ls` したところ subtitles_0.m3u8 のみ）。
		//
		// **`sgroup:subs` を全 variant に付けてはならない。** ffmpeg 9.0.1 は
		// SIGSEGV で落ちる（実測: exit 139、master が .tmp のまま残る）。
		// variant 0 だけに付けても master の EXT-X-STREAM-INF は**全 variant**に
		// `SUBTITLES="subs"` を付けるので、プロファイルを切り替えても字幕
		// rendition は失われない（実測: 2/3 プロファイルで確認）。
		args = append(args, "-c:s", "webvtt", "-hls_subtitle_path", filepath.Join(dir, "subtitles_%v.m3u8"))
	}
	args = append(args, filepath.Join(dir, "playlist_%v.m3u8"))
	return args
}
