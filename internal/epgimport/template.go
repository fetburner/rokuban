// Package epgimport は `rokuban import epgstation` の変換ロジックをまとめる
// （issue #72 / M3-10）。EPGStation のルール・ライブラリを rokuban の
// 永続資産（rules / recordings / media_assets）へ機械変換する恒久コマンドの
// 本体で、cmd/rokuban/import.go はこのパッケージを薄く呼ぶだけにする。
//
// RecordedHistory（履歴）の取り込みはこのパッケージに含まない
// （cmd/rokuban/import.go の newImportEPGStationCmd の doc コメント参照）。
package epgimport

import "regexp"

// epgVariablePattern は EPGStation の %変数% 記法のトークンを拾う。
var epgVariablePattern = regexp.MustCompile(`%[A-Za-z]+%`)

// epgToTemplateVar は EPGStation recordedFormat の %変数% → rokuban
// text/template 記法の変換表（docs/recording/contentpath.md
// 「EPGStation からの変換」節そのもの）。
var epgToTemplateVar = map[string]string{
	"%YEAR%":      "{{.Year}}",
	"%SHORTYEAR%": "{{.ShortYear}}",
	"%MONTH%":     "{{.Month}}",
	"%DAY%":       "{{.Day}}",
	"%HOUR%":      "{{.Hour}}",
	"%MIN%":       "{{.Min}}",
	"%SEC%":       "{{.Sec}}",
	"%DOW%":       "{{.DOW}}",
	"%TITLE%":     "{{.Title}}",
	"%CH%":        "{{.Channel}}",
	"%SID%":       "{{.ServiceID}}",
	"%TYPE%":      "{{.ChannelType}}",
}

// ConvertRecordedFormat は EPGStation の recordedFormat（%変数% 記法）を
// rokuban の text/template 記法へ機械変換する。
//
// 変換表に無い %変数%（%CHNAME% / %CHID% / %ID% や単純なタイプミス）が
// 1 つでもあれば変換自体を行わず、その一覧を unsupported として返す
// （tmpl は空文字）。%変数% 記法では未対応の変数名は黙って空文字に置換され、
// ユーザーは数週間後にファイル名が崩れて初めて気づく
// （docs/recording/contentpath.md「経緯と失敗事例」）。呼び出し側
// （buildRuleFields）はこの空変換を「filename_template を書かず
// DefaultTemplate にフォールバックし、警告を出す」に倒す。
func ConvertRecordedFormat(epgFormat string) (tmpl string, unsupported []string) {
	if epgFormat == "" {
		return "", nil
	}
	seen := make(map[string]bool)
	for _, tok := range epgVariablePattern.FindAllString(epgFormat, -1) {
		if _, ok := epgToTemplateVar[tok]; !ok && !seen[tok] {
			seen[tok] = true
			unsupported = append(unsupported, tok)
		}
	}
	if len(unsupported) > 0 {
		return "", unsupported
	}
	out := epgVariablePattern.ReplaceAllStringFunc(epgFormat, func(tok string) string {
		return epgToTemplateVar[tok]
	})
	return out, nil
}
