package reservation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// SourceRule はルール予約から作られた録画の provenance。
const SourceRule = "rule"

// SourceManual はユーザーが録画を明示した録画の provenance。
const SourceManual = "manual"

// SourceUnattributed は予約もユーザーの録画意図も特定できない録画の provenance。
const SourceUnattributed = "unattributed"

// DeriveRecordingSource は recordings.source を決める（issue #26）。
//
// reservations.source は「ユーザーが手動で予約したか」（不可逆な歴史的事実）と
// 「いまルールが base を供給しているか」（毎パス変わる導出状態）という 2 つの
// 独立した事実を 1 列に載せていたため、手動予約にルールが一度でもマッチすると
// 二度と 'manual' に戻らない不可逆な歪みがあった（reservations.source 列は削除済み）。
//
// 録画時点の program_intents に action='record' の行があるかどうかだけを見る。
// intent は放送終了まで生きているので判定時点では参照でき、この行の有無が
// 「ユーザーが録れと言ったか」の唯一の真実である。program_overrides
// （priority 等の上書き）は M2-4 で intent と分離されているため、「ルール由来の
// 予約に上書きを足しただけ」では intent 行が存在せず、正しく予約行の有無から
// 'rule' になる（docs/recording.md §4.4「manual 行にルールがマッチしても昇格は
// 要らない」）。
//
// hasReservation は予約行が引けたかどうか。intent が無いときに 'rule' と
// 'unattributed' を分けるために必要になる。予約行が無く intent も無い記録
// （tag は付いているが予約と意図が既に GC された、mirakc 側で直接起こされた等）を
// 'rule' と記録するのは誤りで、`source = 'rule'` かつ `rule_id IS NULL` という
// 矛盾した組になってしまう。かといってユーザーの意図が残っていないものを
// 'manual' と断定する材料も無いので、'unattributed' にする。
//
// internal/watcher の recordings 行を作る 2 経路（createRecording /
// handleRecordingFailed）から呼ばれる。同じ式を 2 箇所に書き下すと片方だけ
// 直してもう片方を直し忘れるため、ここに抽出してある。
func DeriveRecordingSource(ctx context.Context, q *sqlcgen.Queries, site string, programID int64, hasReservation bool) (string, error) {
	intent, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: site, ProgramID: programID})
	switch {
	case err == nil && intent.Action == IntentRecord:
		// ユーザーが「録れ」と言った。ルールもマッチしていても変わらない。
		return SourceManual, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("looking up program intent for program %d: %w", programID, err)
	}
	if !hasReservation {
		return SourceUnattributed, nil
	}
	return SourceRule, nil
}
