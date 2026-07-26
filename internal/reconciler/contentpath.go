package reconciler

import (
	"fmt"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// buildContentPath は録画ファイルの content_path を組み立てる。
//
// template が空文字なら従来の固定形式（contentpath.GenerateContentPath。
// 後方互換で挙動を変えない）を返す。非空なら text/template として
// contentpath.Build で展開する。展開の際に渡す contentpath.Data は予約行
// スナップショットの title/channel/channelType から contentpath.NewData で
// 組む — この時点で各フィールドがパス成分としてサニタイズされるため、EPG
// データ（番組名等）に "/" や ".." が混ざっても意図しない階層やパス
// トラバーサルを構造的に作れない。最終的な拡張子付与とパス全体のサニタイズは
// contentpath.Build の責務。
//
// テンプレートは実行時にもエラーになりうる（未知フィールドの参照等。ありえ
// ないはずだが）。推測せずそのままエラーを返す。呼び出し側（createSchedule）は
// これを受けてログに出し、同期対象から外す — 壊れた options で mirakc に
// 既定値の schedule を作ってしまわないという既存の方針（res.ServiceID == nil
// の扱いと同じ）に揃えている。
//
// ContentPath（フルパスの直接指定、db.ReservationOptions.ContentPath）は
// このテンプレート機構とは別物で、呼び出し側（reconciler.createSchedule）が
// 両方指定時に ContentPath を優先する。
func buildContentPath(res sqlcgen.Reservation, template string) (string, error) {
	serviceID := 0
	if res.ServiceID != nil {
		serviceID = int(*res.ServiceID)
	}
	if template == "" {
		return contentpath.GenerateContentPath(res.Title, res.ProgramStartAt, serviceID), nil
	}

	channel := ""
	if res.Channel != nil {
		channel = *res.Channel
	}
	channelType := ""
	if res.ChannelType != nil {
		channelType = *res.ChannelType
	}
	data := contentpath.NewData(res.Title, res.ProgramStartAt, channel, serviceID, channelType)

	path, err := contentpath.Build(template, data)
	if err != nil {
		return "", fmt.Errorf("expanding filename template: %w", err)
	}
	return path, nil
}
