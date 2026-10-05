package config

// ライブ視聴の設定。

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/ffargs"
)

// LiveConfig はライブ視聴（HLS streamer、issue #91）の設定。
//
// **DB を引かない。** ライブセッションはインメモリの使い捨てで（crash-only の
// 唯一の例外。docs/overview.md §設計原則）、認可はリバースプロキシ委譲、同時上限も
// プロセスローカル。config だけで完結する（docs/configuration.md「config と DB の
// 境界」）。
type LiveConfig struct {
	// Enabled が false ならライブ視聴のルートを一切登録しない。既定 false。
	//
	// **ffmpeg の LookPath 検査もこれが true のときだけ行う**（cmd/rokuban/server.go）。
	// 公式イメージ（ffmpeg 無し、docs/overview.md §イメージ戦略）で streamer ロールを
	// 起動する構成（録画配信 / サムネイルのみ）を、ライブを設定していないという理由で
	// 壊さない。
	Enabled bool `yaml:"enabled"`

	FFmpeg string `yaml:"ffmpeg"`
	// FFprobe は字幕ストリームの有無をライブ起動前に判定するために使う。
	// Captions が false なら実行しない。
	FFprobe string `yaml:"ffprobe"`

	// Captions は ARIB 字幕を HLS の字幕レンディションとして出力する。
	// libaribcaption を含む ffmpeg が必要で、既定は false。
	Captions bool `yaml:"captions"`

	// SegmentDir は HLS セグメント/プレイリストの書き出し先。**録画バッファ
	// （mirakc recording.basedir）と同じディスクに置かない**（視聴が録画の I/O を
	// 飽和させうる。docs/operations.md §5「ライブのセグメントを録画バッファと同じ
	// ディスクに置かない」）。tmpfs 前提（k8s なら `emptyDir: {medium: Memory}`）。
	SegmentDir string `yaml:"segment_dir"`

	// MaxSessions はこのプロセスが同時に持てるライブセッション（≒ ffmpeg プロセス）数。
	//
	// **プロセスローカルな上限であり、グローバルな天井ではない。** グローバルな天井は
	// チューナー数で、裁定者は mirakc（docs/operations.md §5「既定を 1 にする根拠と、
	// 増やす判定基準」）。レプリカを増やしてもこの値は上がらない。0 なら既定値（4）。
	MaxSessions int `yaml:"max_sessions"`

	// IdleTimeout はサービス単位の idle GC の猶予。そのサービスへのセグメント要求が
	// この時間来なければ ffmpeg を止める（docs/api.md §ライブ視聴の HLS。「クライアント
	// 1 人ごとの生存」は追わない）。0 なら既定値（30s）。
	IdleTimeout time.Duration `yaml:"idle_timeout"`

	// TunerPriority は mirakc への各ライブ要求に載せる X-Mirakurun-Priority。
	//
	// ruler が生成する schedule の既定 priority（10）より低く保つことで、チューナー
	// 枯渇時に mirakc が録画側を常に勝たせる（docs/recording/delegation.md §2
	// 「チューナー調停」、issue #91 の決定コメント）。0 なら既定値（1）。
	//
	// **`TunerPriority < rules.priority` はここでは検証しない。** 前者は config
	// （この構造体）、後者は DB（ユーザーが自由に編集できる）で、両者を跨いで
	// 検証する権威がどちらの層にも無い。ルールの priority を既定 10 未満に下げる
	// 運用では、この既定値のままだとライブが録画に勝つ（docs/api.md §ライブ視聴の
	// HLS §実装 参照）。
	TunerPriority int `yaml:"tuner_priority"`

	// HWAccel は -i より前に出す唯一のブロック（任意。nil なら何も出さない）。
	//
	// **プロファイル毎ではなく live セクション直下に置く。** ライブは 1 回の
	// ffmpeg で入力 1 本・出力 N 本であり、-hwaccel は入力側のオプション。
	// プロファイル毎に持たせると「プロファイル 2 つが別の hwaccel を要求する」
	// という表現できない設定が書けてしまう --- セクション直下に置けばそれが
	// 表現不可能になる（不変条件 10「CHECK で禁止するより表現不可能にする」。
	// issue #321 決定コメント §1）。
	HWAccel *ffargs.HWAccel `yaml:"hwaccel"`

	// InputExtraArgs は `-i` の直前に追加する許可済み引数（任意。入力側。
	// HWAccel と同じ理由でプロファイル毎ではなく live セクション直下）。
	InputExtraArgs []string `yaml:"input_extra_args"`

	Profiles []LiveProfile `yaml:"profiles"`
}

// LiveProfile は HLS トランスコードの構造化プロファイル。
//
// **`encode.profiles`（VOD 派生物）を流用しない。** HLS はセグメント長・プレイリスト
// 長・キーフレーム間隔という VOD には無い制約を持ち、共有構造体に足すと VOD 側に
// 無関係なフィールドが増える。ISDB-T 地上波の映像は MPEG-2 で、ブラウザの HLS
// 経路（hls.js/MSE）は事実上再生できないため、H.264 へのトランスコードは前提とする
// （mirakc フィルタ + `-c copy` では受信端末を満たさない。issue #91 の決定コメント）。
// 自由形式の cmd 文字列は採らない（encode.profiles と同じ方針）。
//
// **scaler / crf / qp はプロファイル毎に持つ**（HWAccel/InputExtraArgs とは対照的
// --- これらは出力側オプションなので出力ごとに違ってよい。issue #321 決定コメント §1）。
type LiveProfile struct {
	// Name はクエリ（`?profile=`）から参照する一意な名前。ライブのセグメント
	// ファイル名の接頭辞にも使う（1 プロセス内で複数プロファイルの出力を同じ
	// サービスディレクトリに平置きするため。internal/streamer 参照）ため、
	// パス成分として安全な文字だけに制限する（validate）。
	Name string `yaml:"name"`

	VideoCodec string `yaml:"video_codec"`
	AudioCodec string `yaml:"audio_codec"`

	// Height はスケール先の高さ。0 または省略ならスケールしない。
	Height int `yaml:"height"`

	// Scaler はスケール filter の系統（既定 ""=software。ffargs.Scaler）。
	// height が 0 のときに書くと起動エラー。
	Scaler ffargs.Scaler `yaml:"scaler"`

	// Deinterlace はインターレース解除を有効にする系統スイッチ。既定 false は
	// 現行互換で、filter の実体は Scaler から導出する（software は yadif、vaapi は
	// deinterlace_vaapi）。filtergraph 文字列を直接受け取るキーにしないのは、scaler
	// と矛盾する組み合わせを設定できないようにし、`-vf` を別名で解禁しないため。
	Deinterlace bool `yaml:"deinterlace"`

	// CRF は品質指定（任意。未設定は nil）。qp との同時指定は起動エラー。
	CRF *int `yaml:"crf"`

	// QP は品質指定（任意。未設定は nil）。crf との同時指定は起動エラー。
	QP *int `yaml:"qp"`

	Preset string `yaml:"preset"`

	// SegmentSeconds は 1 セグメントの長さ。0 なら既定値（2）。
	SegmentSeconds int `yaml:"segment_seconds"`

	// PlaylistSize はプレイリストに保持するセグメント数（-hls_list_size）。
	// 古いセグメントは削除する（-hls_flags delete_segments）。0 なら既定値（6）。
	PlaylistSize int `yaml:"playlist_size"`

	// ExtraArgs は組み立てた ffmpeg 引数に追加する許可済み引数（任意。
	// 出力側 --- コーデック/品質/スケール指定の後、`-f hls` の前）。
	ExtraArgs []string `yaml:"extra_args"`
}

// ValidateTools は ffmpeg が PATH（または絶対パス）で解決できることを検査する。
// live.enabled が true の streamer ロール起動時だけ呼ぶ
// （不変条件 4、LiveConfig.Enabled のコメント参照）。
func (c LiveConfig) ValidateTools() error {
	if _, err := exec.LookPath(c.FFmpeg); err != nil {
		return fmt.Errorf("live.ffmpeg %q not found in PATH: %w", c.FFmpeg, err)
	}
	if c.Captions {
		if _, err := exec.LookPath(c.FFprobe); err != nil {
			return fmt.Errorf("live.ffprobe %q not found in PATH: %w", c.FFprobe, err)
		}
	}
	if !c.Enabled && !c.Captions {
		return nil
	}
	// 1 回の `ffmpeg -decoders` の出力を両方の判定に使う。
	decoders, err := ffmpegDecoders(c.FFmpeg)
	if err != nil {
		return err
	}
	if c.Enabled {
		if err := validateFFmpegDecoder(decoders, "mpeg2video", "live source MPEG-2 TS"); err != nil {
			return err
		}
	}
	if c.Captions {
		if err := validateLibARIBCaption(decoders, "live"); err != nil {
			return err
		}
	}
	return nil
}

func (c *LiveConfig) applyDefaults() {
	if c.MaxSessions == 0 {
		c.MaxSessions = 4
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.TunerPriority == 0 {
		c.TunerPriority = 1
	}
	for i := range c.Profiles {
		if c.Profiles[i].SegmentSeconds == 0 {
			c.Profiles[i].SegmentSeconds = 2
		}
		if c.Profiles[i].PlaylistSize == 0 {
			c.Profiles[i].PlaylistSize = 6
		}
	}
}

// liveProfileNamePattern は LiveProfile.Name のパス安全な文字集合
// （セグメントファイル名の接頭辞に使うため。英数字・ハイフン・アンダースコアのみ）。
var liveProfileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validate は live 設定の妥当性を検査する（Load 時、applyDefaults の後）。
func (c LiveConfig) validate() error {
	// **値域は enabled に関わらず見る。** `enabled: false` のまま値だけ先に
	// 書いておく構成は実在し（`config.compose.yml` が「後で true にする」形で
	// 出荷している）、そこに書き間違えた負値が入ると、ライブを有効にした日に
	// 初めて起動しなくなる。設定ファイルの誤りは書いた時点で出す。
	if c.MaxSessions < 1 {
		return fmt.Errorf("live.max_sessions must be >= 1, got %d", c.MaxSessions)
	}
	if c.TunerPriority < 0 {
		return fmt.Errorf("live.tuner_priority must be >= 0, got %d", c.TunerPriority)
	}
	// プロファイル・セグメント先の必須性は enabled のときだけ（未設定の
	// プロファイルを検査対象にしない）。
	if !c.Enabled {
		return nil
	}
	if len(c.Profiles) == 0 {
		return fmt.Errorf("live.profiles is required when live.enabled is true")
	}
	if c.SegmentDir == "" {
		return fmt.Errorf("live.segment_dir is required when live.enabled is true")
	}
	if c.IdleTimeout <= 0 {
		return fmt.Errorf("live.idle_timeout must be > 0, got %v", c.IdleTimeout)
	}
	if err := c.HWAccel.Validate(); err != nil {
		return fmt.Errorf("live.hwaccel: %w", err)
	}
	if err := ffargs.ValidateExtraArgs("live.input_extra_args", c.InputExtraArgs); err != nil {
		return err
	}
	if err := rejectLiveStreamSelection("live.input_extra_args", c.InputExtraArgs); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(c.Profiles))
	for i, p := range c.Profiles {
		if p.Name == "" {
			return fmt.Errorf("live.profiles[%d].name is required", i)
		}
		if !liveProfileNamePattern.MatchString(p.Name) {
			return fmt.Errorf("live.profiles[%d].name %q must match %s",
				i, p.Name, liveProfileNamePattern.String())
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("live.profiles: duplicate name %q", p.Name)
		}
		seen[p.Name] = struct{}{}

		if p.VideoCodec == "" {
			return fmt.Errorf("live.profiles[%d] (%s): video_codec is required", i, p.Name)
		}
		if p.AudioCodec == "" {
			return fmt.Errorf("live.profiles[%d] (%s): audio_codec is required", i, p.Name)
		}
		if p.Height < 0 {
			return fmt.Errorf("live.profiles[%d] (%s): height must be >= 0, got %d", i, p.Name, p.Height)
		}
		if p.SegmentSeconds < 1 {
			return fmt.Errorf("live.profiles[%d] (%s): segment_seconds must be >= 1, got %d",
				i, p.Name, p.SegmentSeconds)
		}
		if p.PlaylistSize < 1 {
			return fmt.Errorf("live.profiles[%d] (%s): playlist_size must be >= 1, got %d",
				i, p.Name, p.PlaylistSize)
		}
		if err := ffargs.ValidateVideo(p.Scaler, p.Height, p.CRF, p.QP); err != nil {
			return fmt.Errorf("live.profiles[%d] (%s): %w", i, p.Name, err)
		}
		if err := ffargs.ValidateExtraArgs("extra_args", p.ExtraArgs); err != nil {
			return fmt.Errorf("live.profiles[%d] (%s): %w", i, p.Name, err)
		}
		if err := rejectLiveStreamSelection("extra_args", p.ExtraArgs); err != nil {
			return fmt.Errorf("live.profiles[%d] (%s): %w", i, p.Name, err)
		}
	}
	if c.Captions {
		first := c.Profiles[0]
		for i, p := range c.Profiles[1:] {
			if p.SegmentSeconds != first.SegmentSeconds || p.PlaylistSize != first.PlaylistSize {
				return fmt.Errorf("live.profiles[%d] (%s): segment_seconds and playlist_size must match the first profile when live.captions is enabled", i+1, p.Name)
			}
		}
	}
	return nil
}

// liveStreamSelectionArgs は live の extra_args / input_extra_args で拒否する
// ストリーム選択のオプション（allowlist には VOD のために入っている）。
//
// **live はストリームの並びをアプリが `-var_stream_map` で持つ**（映像 1 本 + 音声
// rendition 3 本 + 字幕。internal/streamer の BuildLiveFFmpegArgs）。並びを変えると
// ffmpeg が起動時に落ち、利用者には 504 しか見えない（実測 ffmpeg 9.0.2: `-an` で
// `Unable to map stream at a:0`、`-map` の追加で `Unable to find mapping variant
// stream`）。`-vn` / `-sn` は映像 / 字幕の map を同じ形で壊す。
var liveStreamSelectionArgs = map[string]bool{"-an": true, "-vn": true, "-sn": true, "-map": true}

// rejectLiveStreamSelection は args に liveStreamSelectionArgs が含まれていればエラーを返す。
// ValidateExtraArgs を通った後に呼ぶ（値のトークンがオプション名と一致することは無い）。
func rejectLiveStreamSelection(label string, args []string) error {
	var errs []string
	for i, a := range args {
		if liveStreamSelectionArgs[a] {
			errs = append(errs, fmt.Sprintf("%s[%d]: %q changes the stream layout the live HLS output depends on", label, i, a))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
