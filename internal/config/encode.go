package config

// encode と CM 検出の設定。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fetburner/rokuban/internal/ffargs"
)

// EncodeConfig はエンコード設定。
//
// プロファイルはデプロイ属性（その環境の ffmpeg ビルドと HW で何ができるか）なので
// config 側に置き、DB のルールからは名前参照する（docs/configuration.md
// 「config と DB の境界」、issue #64 M3-2）。
type EncodeConfig struct {
	FFmpeg  string `yaml:"ffmpeg"`
	FFprobe string `yaml:"ffprobe"`

	// Concurrency は encode キューの MaxWorkers。0 / 未設定は Load 後の既定値 1。
	Concurrency int `yaml:"concurrency"`

	// ThumbnailConcurrency は thumbnail キューの MaxWorkers。0 / 未設定は Load 後の既定値 1。
	ThumbnailConcurrency int `yaml:"thumbnail_concurrency"`

	Profiles []EncodeProfile `yaml:"profiles"`
}

// CMDetectConfig configures optional CM section detection. JLSE executables are
// installed only in the self-built full image.
type CMDetectConfig struct {
	// Enabled snapshots this setting into each recording policy at ingest time.
	Enabled bool `yaml:"enabled"`

	// BinaryDir contains logoframe, chapter_exe, and join_logo_scp.
	BinaryDir string `yaml:"binary_dir"`
}

// CMDetectRulePath is the fixed JL standard rule set included in Dockerfile.full.
const CMDetectRulePath = "/usr/local/share/rokuban/cm_detect/JL_標準.txt"

// ValidateTools checks the CM analysis executables required by enabled workers.
// ffprobe is the encode.ffprobe path; the worker reads the original's real duration with it.
func (c CMDetectConfig) ValidateTools(ffprobe string) error {
	if _, err := exec.LookPath(ffprobe); err != nil {
		return fmt.Errorf("cm_detect needs ffprobe %q; build the self-contained image with Dockerfile.full: %w", ffprobe, err)
	}
	for _, name := range []string{"logoframe", "chapter_exe", "join_logo_scp"} {
		path := name
		if c.BinaryDir != "" {
			path = filepath.Join(c.BinaryDir, name)
		}
		if _, err := exec.LookPath(path); err != nil {
			return fmt.Errorf("cm_detect binary %q not found; build the self-contained image with Dockerfile.full: %w", path, err)
		}
	}
	if _, err := os.Stat(CMDetectRulePath); err != nil {
		return fmt.Errorf("cm_detect rule file %q not found; build the self-contained image with Dockerfile.full: %w", CMDetectRulePath, err)
	}
	return nil
}

// EncodeProfile は構造化エンコードプロファイルの定義。
//
// 自由形式の cmd 文字列は採らない（EPGStation の命令的テンプレートを繰り返さない。
// issue #64）。worker がこのフィールドから ffmpeg 引数を組み立てる（M3-3、
// M3-3 拡張版 issue #321 が HW エンコードの構造化フィールドを追加）。
//
// # 追加したキーの命名の理由（issue #321 決定コメント）
//
//  1. hwaccel はネストしたブロック（*ffargs.HWAccel）であり、hwaccel_kind の
//     ようなフラット 3 本にしない。ブロックの存在そのものが「-i の前に出す」と
//     いう主張になる（不変条件 10）。フラットだと「device だけ書いた」状態が
//     「何も出さない」と区別できず、掃除する規則が要る。ポインタなので
//     `hwaccel:`（値なし）は nil、`hwaccel: {}` は「書いた」で kind is required
//     になる（goccy/go-yaml が null をポインタの nil にデコードすることに乗った
//     挙動。TestLoad_EncodeProfileHWAccel が固定している）。
//  2. scaler は「系統の名前」であって filter 文字列ではない。`-vf` /
//     `video_filter` というキーは永久に作らない。filtergraph は第 2 の
//     コマンド言語で、`scale_vaapi=...,drawtext=...` と書けた時点で cmd を
//     別名で解禁したのと同じになる。幾何の入力は height 1 本に保つ。
//  3. height + HW スケールでソフトの scale=-2:H が出ないのは検査ではなく構造。
//     filter を作る経路が ffargs.VideoFilterArgs(scaler, height, deinterlace) の
//     1 本だけで、返るのは常に 1 本の chain。「両方 append する」コードが書けなければ
//     両方は出ない。deinterlace が有効なら解除が chain の先頭に入る。
//  4. 品質は crf / qp の 2 キー排他。quality: {mode, value} は採らない。
//     キー名がエンコーダ自身のオプション名そのものなので、系統が増えるたびに
//     腐るマッピング表が要らない。両方書いたら起動エラー（優先順位を
//     覚えさせない。ffargs.ValidateVideo）。
//  5. -global_quality / -cq / -q:v はキーにしない。extra_args が届く位置
//     （コーデック指定より後ろ）には構造を足さない、という基準（下記）に従う。
//     届く綴りを今フィールドにすると、テストされていない綴りが増えるだけ
//     （不変条件 11: 書き手のいない形は決めない）。
//  6. extra_args は改名しない。位置が変わっていないから意味も変わっていない
//     （**ただし位置は 1 点だけ変わる**: `-f`（コンテナ）の後ろから前に移した
//     ---VOD と live で「ユーザーのオプションはコーデック/品質/スケール指定の
//     後・アプリ所有の末尾の前」という 1 つの規則にするため。`-f` は許可済み
//     オプションに含まれないので、ユーザーが旧位置に依存する余地は無い）。
//     対称性のために既存の全 config を壊す価値はないので、新しい方（input 側）
//     の名前に位置を入れる: input_extra_args（ffmpeg 用語の input options の位置）。
//  7. アプリが握り続けるもの: -y / -i / 入出力パス / -f / -progress pipe:1 /
//     -loglevel error。ユーザーが書けるのは ffargs.ValidateExtraArgs が値の個数まで
//     把握する allowlist のオプション列だけで、コマンド文字列ではない。値を取らない
//     `-an` 等も明示するため、直後に 2 本目の出力パスを密輸できない。
//  8. device の存在は起動時に検査しない。公式イメージと device の無い CI が
//     落ちる。無い device を書いたプロファイルはジョブ失敗でよい（マウントは
//     k8s resources.limits / Docker --device の話でこの構造体の外）。
//  9. deinterlace は bool の系統スイッチとし、filter の綴りは scaler から導出する。
//     software なら yadif、vaapi なら deinterlace_vaapi を選ぶ。`deinterlace: yadif`
//     のように filter 名を直接書ける形にすると、scaler と矛盾する組み合わせを
//     表現でき、`-vf` を別名で解禁することになるため採らない。
//
// scaler が受け付ける値の集合は「filter の綴りを実際に確かめた系統」に限る
// （ffargs.AllowedScalers の doc コメント参照。未検証の綴りを黙って許すより
// 系統ごと除外する）。
type EncodeProfile struct {
	// Name はルール / overrides から参照する一意な名前。
	Name string `yaml:"name"`

	// Container は出力コンテナ。mp4 または mkv（拡張子と -f に対応）。
	Container string `yaml:"container"`

	// VideoCodec は -c:v に渡すコーデック名（例: libx264）。
	VideoCodec string `yaml:"video_codec"`

	// AudioCodec は -c:a に渡すコーデック名（例: aac）。
	AudioCodec string `yaml:"audio_codec"`

	// Subtitles は字幕サイドカーの形式。現在は webvtt のみを許可する。
	// MP4 に内蔵せず、エンコード成果物の隣に .vtt を置く。
	Subtitles string `yaml:"subtitles"`

	// Cut は確認済みチャプターの cut=true 区間を除いたカット版を作る。
	//
	// 切る対象（どの区間か）は録画側の事実でチャプターが持ち、この出力に
	// 切り取りを適用するかは出力の性質でプロファイルが持つ。live.enabled が false
	// の構成では cut プロファイルを選ぶとき cut でないプロファイルを 1 つ以上含める
	// こと（ValidateCutSelection）。原本 HLS が使えず確認の再生に encode が要るため。
	// live.enabled が true なら原本 HLS で確認できるので cut だけでも選べる。
	Cut bool `yaml:"cut"`

	// Height はスケール先の高さ。0 または省略ならスケールしない。
	Height int `yaml:"height"`

	// Scaler はスケール filter の系統（既定 ""=software。ffargs.Scaler）。
	// height が 0 のときに書くと起動エラー（何も主張しないキーを黙って無視
	// しない。不変条件 10 と同じ形）。
	Scaler ffargs.Scaler `yaml:"scaler"`

	// Deinterlace はインターレース解除を有効にする系統スイッチ。既定 false は
	// 現行互換で、filter の実体は Scaler から導出する（software は yadif、vaapi は
	// deinterlace_vaapi）。filtergraph 文字列を直接受け取るキーにしないのは、scaler
	// と矛盾する組み合わせを設定できないようにし、`-vf` を別名で解禁しないため。
	Deinterlace bool `yaml:"deinterlace"`

	// CRF は品質指定（任意。未設定は nil）。qp との同時指定は起動エラー。
	CRF *int `yaml:"crf"`

	// QP は品質指定（任意。未設定は nil）。VAAPI 等 crf を解さないエンコーダ用。
	// crf との同時指定は起動エラー（優先順位を実行時に決めさせない）。
	QP *int `yaml:"qp"`

	// Preset はエンコーダの preset（任意。空なら付けない）。
	Preset string `yaml:"preset"`

	// HWAccel は -i より前に出す唯一のブロック（任意。nil なら何も出さない）。
	HWAccel *ffargs.HWAccel `yaml:"hwaccel"`

	// InputExtraArgs は -i の直前に追加する許可済み引数（任意。入力側）。
	InputExtraArgs []string `yaml:"input_extra_args"`

	// ExtraArgs は組み立てた ffmpeg 引数に追加する許可済み引数（任意。出力側 ---
	// コーデック/品質/スケール指定の後、アプリ所有の末尾（-f/-progress/出力
	// パス）の前）。自由形式のコマンド全体は受け取らない。
	ExtraArgs []string `yaml:"extra_args"`
}

// Profile は name に一致するプロファイルを返す。見つからなければ ok=false。
func (c EncodeConfig) Profile(name string) (EncodeProfile, bool) {
	for _, p := range c.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return EncodeProfile{}, false
}

// ProfileNames は定義済みプロファイル名を定義順で返す。
func (c EncodeConfig) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		names = append(names, p.Name)
	}
	return names
}

// CutProfileNames は cut: true のプロファイル名を定義順で返す。
func (c EncodeConfig) CutProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		if p.Cut {
			names = append(names, p.Name)
		}
	}
	return names
}

// CutProfileSet は cut: true のプロファイル名の集合を返す。常に non-nil
// （空設定でも non-nil を返す ProfileNames と同じ規約）。
func (c EncodeConfig) CutProfileSet() map[string]struct{} {
	set := make(map[string]struct{}, len(c.Profiles))
	for _, p := range c.Profiles {
		if p.Cut {
			set[p.Name] = struct{}{}
		}
	}
	return set
}

// ValidateCutSelection は、live が無効な構成で「cut のプロファイルを選ぶなら
// cut でないプロファイルを 1 つ以上含む」ことを検査する。原本 HLS が無いと
// 確認に再生が要り、再生に encode が要り、encode に確認が要る循環になるため。
// live が有効なら原本 HLS で確認できるので cut だけの選択も許す。
// ルール検証・override 検証・ingest の凍結・POST /api/recordings/{id}/encode-profiles
// の 4 経路がこの同じ判定を使う。
//
// cut が空の集合なら常に nil（cut プロファイルが 1 つも定義されていない構成では
// この規則は何も主張しない）。names が空でも nil。live が有効なら常に nil。
func ValidateCutSelection(names []string, cut map[string]struct{}, liveEnabled bool) error {
	if liveEnabled || len(cut) == 0 {
		return nil
	}
	sawCut := false
	for _, name := range names {
		if _, ok := cut[name]; ok {
			sawCut = true
			continue
		}
		return nil
	}
	if !sawCut {
		return nil
	}
	return fmt.Errorf("encodeProfiles contains only cut profiles; a profile with cut unset is required so the recording can be reviewed before trimming")
}

// ValidateTools は ffmpeg / ffprobe が PATH（または絶対パス）で解決できることを
// 検査する。worker ロールの起動時だけ呼ぶ（不変条件 4: ffmpeg/ffprobe の exec は
// worker / streamer パッケージのみ。api は呼ばない）。
func (c EncodeConfig) ValidateTools() error {
	if _, err := exec.LookPath(c.FFmpeg); err != nil {
		return fmt.Errorf("encode.ffmpeg %q not found in PATH: %w", c.FFmpeg, err)
	}
	if _, err := exec.LookPath(c.FFprobe); err != nil {
		return fmt.Errorf("encode.ffprobe %q not found in PATH: %w", c.FFprobe, err)
	}
	for _, p := range c.Profiles {
		if p.Subtitles == "webvtt" {
			decoders, err := ffmpegDecoders(c.FFmpeg)
			if err != nil {
				return err
			}
			if err := validateLibARIBCaption(decoders, "encode"); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func (c *EncodeConfig) applyDefaults() {
	// 0 / 未設定だけ既定に寄せる。負値は validate で弾く（黙って 1 にしない）。
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.ThumbnailConcurrency == 0 {
		c.ThumbnailConcurrency = 1
	}
}

// validate はプロファイル定義の妥当性を検査する（Load 時）。
func (c EncodeConfig) validate() error {
	if c.Concurrency < 1 {
		return fmt.Errorf("encode.concurrency must be >= 1, got %d", c.Concurrency)
	}
	if c.ThumbnailConcurrency < 1 {
		return fmt.Errorf("encode.thumbnail_concurrency must be >= 1, got %d", c.ThumbnailConcurrency)
	}
	seen := make(map[string]struct{}, len(c.Profiles))
	for i, p := range c.Profiles {
		if p.Name == "" {
			return fmt.Errorf("encode.profiles[%d].name is required", i)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("encode.profiles: duplicate name %q", p.Name)
		}
		seen[p.Name] = struct{}{}

		switch p.Container {
		case "mp4", "mkv":
		default:
			return fmt.Errorf("encode.profiles[%d] (%s): container must be mp4 or mkv, got %q",
				i, p.Name, p.Container)
		}
		if p.VideoCodec == "" {
			return fmt.Errorf("encode.profiles[%d] (%s): video_codec is required", i, p.Name)
		}
		if p.AudioCodec == "" {
			return fmt.Errorf("encode.profiles[%d] (%s): audio_codec is required", i, p.Name)
		}
		if p.Subtitles != "" && p.Subtitles != "webvtt" {
			return fmt.Errorf("encode.profiles[%d] (%s): subtitles must be webvtt, got %q",
				i, p.Name, p.Subtitles)
		}
		if p.Height < 0 {
			return fmt.Errorf("encode.profiles[%d] (%s): height must be >= 0, got %d",
				i, p.Name, p.Height)
		}
		if err := validateEncodeProfileFFArgs(p); err != nil {
			return fmt.Errorf("encode.profiles[%d] (%s): %w", i, p.Name, err)
		}
	}
	return nil
}

// validateEncodeProfileFFArgs は VOD プロファイル 1 件ぶんの ffargs 検査
// （scaler/height/crf/qp、hwaccel ブロック、extra_args/input_extra_args の
// allowlist）をまとめる。live.profiles 側も同じ ffargs 関数を通すことで、
// 片側だけ直る事故を防ぐ（issue #321 決定コメント §5）。
func validateEncodeProfileFFArgs(p EncodeProfile) error {
	var errs []string
	if err := ffargs.ValidateVideo(p.Scaler, p.Height, p.CRF, p.QP); err != nil {
		errs = append(errs, err.Error())
	}
	if err := p.HWAccel.Validate(); err != nil {
		errs = append(errs, err.Error())
	}
	if err := validateCutProfile(p); err != nil {
		errs = append(errs, err.Error())
	}
	// extra_args と input_extra_args の両方を検査し、1 回のエラーに全件出す
	// （どちらか片方だけを検査する実装ミスをテストで検出できるように）。
	if err := ffargs.ValidateExtraArgs("extra_args", p.ExtraArgs); err != nil {
		errs = append(errs, err.Error())
	}
	if err := ffargs.ValidateExtraArgs("input_extra_args", p.InputExtraArgs); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// validateCutProfile は cut: true のプロファイルだけに掛かる制約を検査する。
//
// FFmpeg の trim / setpts / concat は HW フレームを受け取り、そのコンテキストを
// 後段へ渡す。根拠は FFmpeg n5.1.6 / n9.0 の trim.c、avf_concat.c、avfilter.c。
// 実測は ffmpeg 9.0.2 の VideoToolbox 経路（trim → setpts → concat → scale_vt →
// h264_videotoolbox）で、VAAPI 実機では未検証（issue #1064）。
//
// 起動時には HW フレームへ CPU filter をつなぐ形と、ソフトウェアフレームを VAAPI
// filter に渡す形を拒否する。救済経路では CPU decode / filter の後ろに
// `format=nv12,hwupload` を置き、VAAPI エンコードだけを使う:
//
//   - `hwaccel.output_format` と VAAPI 以外の scaler で height / deinterlace がある —
//     後段の CPU filter は HW フレームを処理できない
//   - `scaler: vaapi` で filter があるのに `hwaccel.output_format: vaapi` が無い —
//     VAAPI filter にソフトウェアフレームが渡る。救済経路の upload は filter の後ろ
//   - `hwaccel.kind` が vaapi 以外 — scaler と救済経路が VAAPI にしかない
//   - `hwaccel.kind: vaapi` で device が無い — `-hwaccel_device` / `-vaapi_device` に
//     渡すものが無い
//   - `hwaccel.output_format` が vaapi 以外 — output_format があると hwupload を
//     付けないので、ソフトウェア形式のフレームを HW へ上げる手段が無い
//   - `extra_args` の `-map` — ストリームの並びはアプリが握る（live と同じ理由）
func validateCutProfile(p EncodeProfile) error {
	if !p.Cut {
		return nil
	}
	var errs []string
	hasVideoFilters := p.Height > 0 || p.Deinterlace
	hasHWFrames := p.HWAccel != nil && p.HWAccel.OutputFormat != ""
	if hasHWFrames && hasVideoFilters && p.Scaler != ffargs.ScalerVAAPI {
		errs = append(errs, "cut profiles with hwaccel.output_format and height/deinterlace require scaler \"vaapi\" (CPU filters cannot process hardware frames)")
	}
	if hasVideoFilters && p.Scaler == ffargs.ScalerVAAPI &&
		(p.HWAccel == nil || p.HWAccel.Kind != "vaapi" || p.HWAccel.OutputFormat != "vaapi") {
		errs = append(errs, "cut profiles with scaler \"vaapi\" and height/deinterlace require hwaccel.kind \"vaapi\" and hwaccel.output_format \"vaapi\" (the rescue path uploads after the filters)")
	}
	if p.HWAccel != nil {
		switch p.HWAccel.Kind {
		case "vaapi":
			if p.HWAccel.Device == "" {
				errs = append(errs, "cut profiles with hwaccel.kind \"vaapi\" require hwaccel.device (it becomes -hwaccel_device or -vaapi_device)")
			}
			if p.HWAccel.OutputFormat != "" && p.HWAccel.OutputFormat != "vaapi" {
				errs = append(errs, fmt.Sprintf("cut profiles support only hwaccel.output_format \"vaapi\", got %q", p.HWAccel.OutputFormat))
			}
		default:
			errs = append(errs, fmt.Sprintf("cut profiles support only hwaccel.kind \"vaapi\", got %q", p.HWAccel.Kind))
		}
	}
	for i, a := range p.ExtraArgs {
		if a == "-map" {
			errs = append(errs, fmt.Sprintf("extra_args[%d]: \"-map\" is not allowed in cut profiles (the application owns stream selection)", i))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
