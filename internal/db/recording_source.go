package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// DeriveRecordingSource は recordings.source を決める（issue #26）。
//
// reservations.source は「ユーザーが手動で予約したか」（不可逆な歴史的事実）と
// 「いまルールが base を供給しているか」（毎パス変わる導出状態）という 2 つの
// 独立した事実を 1 列に載せていたため、手動予約にルールが一度でもマッチすると
// 二度と 'manual' に戻らない不可逆な歪みがあった（同列は 00012 で削除済み）。
//
// 録画時点（または #98 の never-scheduled 行のように「録画されなかった」と
// 判定した時点）の program_intents に action='record' の行があるかどうかだけを
// 見る。intent は放送終了まで生きているので判定時点では必ず参照でき、この行の
// 有無が「ユーザーが録れと言ったか」の唯一の真実である。program_overrides
// （priority 等の上書き）は M2-4 で intent と分離されているため、「ルール由来の
// 予約に上書きを足しただけ」では intent 行が存在せず、正しく 'rule' のままになる
// （docs/recording.md §4.4「manual 行にルールがマッチしても昇格は要らない」）。
//
// hasReservation は予約行が引けたかどうか。**意図が無いときの既定値**を分ける
// ために必要になる。予約行が無い記録（tag は付いているが予約が既に削除
// されている等）を 'rule' と記録するのは誤りで、`source = 'rule'` かつ
// `rule_id IS NULL` という矛盾した組になってしまう。帰属できるルールが無いなら
// 「人間が手で起こした録画」として 'manual' に倒す（issue #26 以前の実装が
// `source := "manual"` を既定にしていたのと同じ判断）。
//
// internal/watcher（recordings 行を作る 2 つの経路: createRecording /
// handleRecordingFailed）と internal/reconciler（never-scheduled 行を作る
// recordNeverScheduled）の両方から呼ばれるため、ここに抽出してある。同じ式を
// 2 箇所に書き下すと、片方だけ直してもう片方を直し忘れる事故が起きる
// （CLAUDE.md「不変条件 5」周辺の教訓と同型）。
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
		return SourceManual, nil
	}
	return SourceRule, nil
}
