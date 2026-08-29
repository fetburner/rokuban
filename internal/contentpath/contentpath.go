// Package contentpath はファイル名テンプレート（text/template）の展開と、
// パス成分のサニタイズを行う。
//
// api（ルール作成/更新時にテンプレートを検証して 400 を返す）と reconciler
// （実際に予約行のスナップショットから content_path を組み立てる）の両方から
// 使われるため、独立パッケージに切り出してある。reconciler だけに置くと api が
// reconciler を import することになり、コントロールプレーンの構成要素
// （宣言的同期ループ）に薄い検証ロジックが引きずられて依存する逆転が起きる。
package contentpath

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

// jst はファイル名テンプレートの日時フィールド（Data.StartAt 等）を解決する
// ための固定タイムゾーン。日本には夏時間がないので time.LoadLocation("Asia/Tokyo")
// （OS の tzdata に依存）を使わず FixedZone で足りる。
var jst = time.FixedZone("JST", 9*60*60)

// dowKanji は time.Weekday（Sunday=0 起点）に対応する曜日の一字表記。
var dowKanji = [7]string{"日", "月", "火", "水", "木", "金", "土"}

var unsafeChars = regexp.MustCompile(`[/\\:\*\?"<>\|` + "\x00-\x1f\x7f" + `]`)

// sanitizeComponent は 1 つのパス成分に収まるよう文字列を無害化する
// （区切り文字・制御文字の除去、".." の破壊、長さ制限）。結果が空文字や "."
// になった場合は "_" を返すため、呼び出し側で「空文字のままにしたい」場合は
// 事前に空チェックすること（NewData の sanitizeOrEmpty 参照）。
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

// sanitizeOrEmpty は sanitizeComponent を通すが、入力が空文字なら空文字のまま
// 返す。sanitizeComponent("") は "_" を返すため、素通しすると NULL 由来の
// channel/channelType 等に意図しない "_" が現れてしまう。
func sanitizeOrEmpty(s string, maxLen int) string {
	if s == "" {
		return ""
	}
	return sanitizeComponent(s, maxLen)
}

// SanitizeContentPath はパス全体（"/" 区切り）を成分ごとにサニタイズし、先頭の
// "/" を落として絶対パス化を防ぐ。ContentPath（ユーザーによるフルパスの直接
// 指定）とテンプレート展開結果の両方について、最終防衛線として必ず通す。
func SanitizeContentPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = sanitizeComponent(part, 0)
	}
	result := strings.Join(parts, "/")
	result = strings.TrimPrefix(result, "/")
	return result
}

// DefaultTemplate は filenameTemplate が未指定・空文字のときに使う既定の
// ファイル名テンプレート。見た目は text/template 移行前の固定形式
// （YYYYMMDD/HHMMSS_タイトル_サービスID.m2ts）と同じだが、他の template と
// 同じ経路（NewData 経由）で展開されるため、時刻は必ず JST で解決される。
//
// 既定を専用関数ではなく template 文字列にするのは、専用関数だと
// `.In(jst)` を通さずサーバーのタイムゾーンに依存する書き方に戻る誘惑が
// 常にあるため。
const DefaultTemplate = "{{.Year}}{{.Month}}{{.Day}}/{{.Hour}}{{.Min}}{{.Sec}}_{{.Title}}_{{.ServiceID}}"

// Data はファイル名テンプレートに渡す値。
//
// Title/Channel/ChannelType はデータ由来の自由文字列だが、NewData の時点で
// 既に sanitizeComponent を通した「1 パス成分に収まる」文字列が入っている
// （番組名には "/" が普通に入る。「A/B」等）。**階層を作れるのはテンプレートに
// 書かれた "/"（および {{.StartAt.Format "2006/01"}} のようにユーザーが明示的に
// 書いた書式）だけ**であり、Data のフィールド自体が区切りに昇格することはない。
type Data struct {
	// StartAt は番組開始時刻（JST）。{{.StartAt.Format "2006-01"}} のように
	// 任意の書式が書ける。
	StartAt                                     time.Time
	Year, ShortYear, Month, Day, Hour, Min, Sec string
	// DOW は曜日（日〜土）。
	DOW string
	// Title は番組名。パス成分としてサニタイズ済み（NewData 参照）。
	Title string
	// Channel は物理チャンネル。パス成分としてサニタイズ済み。
	Channel string
	// ServiceID はサービス ID の10進表記。
	ServiceID string
	// ChannelType はチャンネル種別（GR/BS/CS/SKY 等）。パス成分としてサニタイズ済み。
	ChannelType string
}

// NewData は予約行のスナップショットの値から Data を組み立てる。
//
// title/channel/channelType はここで sanitizeComponent を通す
// （データ由来の "/" が階層区切りに昇格するのを構造的に防ぐ担保）。ただし
// 空文字は空文字のまま返す（channel/channelType は移行前の行で NULL・空文字
// になりうるが、それをそのまま "_" にすり替えるとテンプレートの見た目が
// 意図せず崩れるため）。時刻は必ず JST で解決する（サーバーのタイムゾーン
// 設定に依存させない）。
func NewData(title string, startAt time.Time, channel string, serviceID int, channelType string) Data {
	j := startAt.In(jst)
	return Data{
		StartAt:     j,
		Year:        j.Format("2006"),
		ShortYear:   j.Format("06"),
		Month:       j.Format("01"),
		Day:         j.Format("02"),
		Hour:        j.Format("15"),
		Min:         j.Format("04"),
		Sec:         j.Format("05"),
		DOW:         dowKanji[int(j.Weekday())],
		Title:       sanitizeOrEmpty(title, 80),
		Channel:     sanitizeOrEmpty(channel, 0),
		ServiceID:   strconv.Itoa(serviceID),
		ChannelType: sanitizeOrEmpty(channelType, 0),
	}
}

// Build はファイル名テンプレート tmpl を text/template として解釈し、d の値で
// 展開する。拡張子はテンプレートに含めない規約なので、展開結果に常に
// ".m2ts" を付し、最後に SanitizeContentPath を通す（Data の各フィールドは
// NewData の時点でパス成分化済みだが、テンプレート自体に静的に ".." や
// 絶対パスを書かれた場合の防御は最終的にここで担保する）。
//
// テンプレートの構文エラー（Parse）・実行時エラー（Execute。未知フィールドの
// 参照 {{.Foo}} 等、Parse だけでは検出できない）は推測せずそのまま返す。
// 呼び出し側が判断する（api はルール作成/更新時に 400、reconciler は
// ログに出して同期対象から外す）。
func Build(tmpl string, d Data) (string, error) {
	t, err := template.New("filename").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parsing filename template: %w", err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("executing filename template: %w", err)
	}
	return SanitizeContentPath(buf.String() + ".m2ts"), nil
}

// Validate はファイル名テンプレート文字列が有効かどうかを検証する。
//
// Parse だけでなく、サンプルの Data に対して実際に Build（Parse + Execute）
// まで行う。未知フィールド（{{.Foo}}）は Parse では素通りし、構造体フィールドの
// 評価を実際に行う Execute で初めてエラーになるため、両方を通さないと
// タイポを検出できない。ルール作成/更新時の 400 判定
// （internal/api/rules.go の validateRuleInput）に使う。
func Validate(tmpl string) error {
	_, err := Build(tmpl, sampleData())
	return err
}

// sampleData は Validate 用のサンプルデータ。実データと同じ形の Data であれば
// 値そのものはテンプレートの妥当性検証に無関係なので、固定値でよい。
func sampleData() Data {
	return NewData("サンプル番組", time.Now(), "27", 1024, "GR")
}
