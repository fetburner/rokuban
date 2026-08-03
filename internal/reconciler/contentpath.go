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
// contentpath.Build で展開する。展開の際に渡す contentpath.Data は
// program_snapshots（#27 で reservations から抽出された番組の事実の
// スナップショット）の title/channel/channelType から contentpath.NewData で
// 組む — この時点で各フィールドがパス成分としてサニタイズされるため、EPG
// データ（番組名等）に "/" や ".." が混ざっても意図しない階層やパス
// トラバーサルを構造的に作れない。最終的な拡張子付与とパス全体のサニタイズは
// contentpath.Build の責務。
//
// snap のチャンネル識別列は issue #101（00026）で NOT NULL 化されたので、
// ここで NULL を仮定したフォールバックは持たない（resolveContentPath の
// コメント参照）。
//
// テンプレートは実行時にもエラーになりうる（未知フィールドの参照等。ありえ
// ないはずだが）。推測せずそのままエラーを返す。呼び出し側（createSchedule）は
// これを受けてログに出し、同期対象から外す。
//
// ContentPath（フルパスの直接指定、db.ReservationOptions.ContentPath）は
// このテンプレート機構とは別物で、呼び出し側（reconciler.createSchedule）が
// 両方指定時に ContentPath を優先する。
func buildContentPath(snap sqlcgen.ProgramSnapshot, template string) (string, error) {
	serviceID := int(snap.ServiceID)
	if template == "" {
		return contentpath.GenerateContentPath(snap.Title, snap.StartAt, serviceID), nil
	}

	data := contentpath.NewData(snap.Title, snap.StartAt, snap.Channel, serviceID, snap.ChannelType)

	path, err := contentpath.Build(template, data)
	if err != nil {
		return "", fmt.Errorf("expanding filename template: %w", err)
	}
	return path, nil
}
