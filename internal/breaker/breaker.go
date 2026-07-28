// Package breaker は大量削除サーキットブレーカーの永続状態を扱う。
//
// 守る対象は「ルール x EPG」の評価結果から導出される削除だけである
// （docs/recording.md §3.2「大量削除サーキットブレーカー」）。EPG の一時欠損
// （mirakc 再起動・再スキャン・SI 取得不良）で素朴な ruler は予約を大量に
// 「不要」と判定してしまう。EPGStation#692（予約と録画が勝手に消える）は
// この障害クラスの実例。
//
// # 発動は行の存在で表す
//
// 「停止していない」を表す行は無い（意味を持たない行を作らない）。再開は
// 行の DELETE。M1-4 の骨格はパス内で完結していて次のパスに何も残さなかったが、
// 「手動確認後に再開」という設計には**人間が確認するまで止まり続けるラッチ**が
// 必要で、それはプロセスをまたぐ永続状態である。レベルトリガー設計の中で
// 数少ない導出できない状態 --- 誰かが確認したという事実は再取得できない。
//
// # ラッチは件数が閾値以下に戻っても自動で解けない
//
// EPG が回復して削除候補がゼロになれば削除するものが無いので実害はないが、
// 自動で解けるようにすると「一瞬止まって自動復帰した」がアラートに残らず、
// EPG が繰り返し欠損する状況を見逃す。解除は必ず人間の操作を通す。
package breaker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
)

// ブレーカーの識別子。値の権威は DB の CHECK ではなくここ
// （ブレーカーの追加をマイグレーションなしでできるようにするため）。
const (
	// RulerDeletes は ruler が「ルール x EPG」の評価から導出した予約削除を守る。
	// **導出削除を止められる唯一の場所**であり、ここを通った削除は
	// 「DB がそう決めた」という確定事項として reconciler が mirakc へ伝搬する。
	RulerDeletes = "ruler_deletes"

	// ReconcileTotalLoss は reconciler の「desired が空なのに observed がある」
	// という全損シグネチャを守る。件数の閾値ではない（理由は Guard のコメント）。
	ReconcileTotalLoss = "reconcile_total_loss"
)

// Sample は発動時に「何が消されようとしていたか」を説明するためのペイロード。
// 手動確認の材料であり、ブレーカー自身のロジックは中身を使わない。
type Sample struct {
	// Total は止めた削除の総数。
	Total int `json:"total"`
	// Programs は先頭いくつかの抜粋（全部は入れない。件数が多いときこそ発動する）。
	Programs []SampleProgram `json:"programs,omitempty"`
}

// SampleProgram は抜粋 1 件。
type SampleProgram struct {
	ProgramID int64  `json:"programId"`
	Title     string `json:"title,omitempty"`
}

// MaxSampleSize は detail に載せる抜粋の上限。
const MaxSampleSize = 20

// Querier は breaker が必要とする DB 操作。sqlcgen.Queries が満たす。
// トランザクション内でも外でも使えるようにインタフェースで受ける。
type Querier interface {
	TripCircuitBreaker(ctx context.Context, arg sqlcgen.TripCircuitBreakerParams) (sqlcgen.CircuitBreaker, error)
	GetCircuitBreaker(ctx context.Context, arg sqlcgen.GetCircuitBreakerParams) (sqlcgen.CircuitBreaker, error)
}

// IsTripped は指定のブレーカーが発動中かを返す。
//
// 各パスの先頭で呼ぶ。発動中なら削除を実行してはならない（作成・更新は続けてよい
// --- 止めたいのは削除だけで、レベルトリガーで収束させたい他の差分は止めない）。
func IsTripped(ctx context.Context, q Querier, site, name string) (bool, error) {
	_, err := q.GetCircuitBreaker(ctx, sqlcgen.GetCircuitBreakerParams{Site: site, Name: name})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("getting circuit breaker %q: %w", name, err)
	}
	return true, nil
}

// Trip はブレーカーを発動させる（既に発動中なら件数と抜粋を更新する）。
//
// 呼び出し側は Trip の後、その削除を実行してはならない。戻り値のエラーは
// 「発動の記録に失敗した」であり、その場合も削除は実行しない（記録できないまま
// 削除を続けるのが最悪の組み合わせ）。
func Trip(ctx context.Context, q Querier, site, name string, threshold int, sample Sample) error {
	if len(sample.Programs) > MaxSampleSize {
		sample.Programs = sample.Programs[:MaxSampleSize]
	}
	detail, err := json.Marshal(sample)
	if err != nil {
		return fmt.Errorf("marshalling circuit breaker sample: %w", err)
	}

	row, err := q.TripCircuitBreaker(ctx, sqlcgen.TripCircuitBreakerParams{
		Site:      site,
		Name:      name,
		Pending:   int32(sample.Total),
		Threshold: int32(threshold),
		Detail:    detail,
	})
	if err != nil {
		return fmt.Errorf("tripping circuit breaker %q: %w", name, err)
	}

	metrics.CircuitBreakerTripped.WithLabelValues(site, name).Set(1)
	slog.Error("circuit breaker tripped — deletes withheld until manually resumed",
		"site", site,
		"breaker", name,
		"pending_deletes", sample.Total,
		"threshold", threshold,
		"tripped_at", row.TrippedAt,
	)
	return nil
}

// ObserveState はブレーカーの現在状態をメトリクスに反映する。
//
// ゲージはプロセスの再起動で失われるので、各パスの先頭で呼んで DB の真実に
// 合わせ直す（レベルトリガー: メトリクスも導出値として毎回作り直す）。
func ObserveState(ctx context.Context, q Querier, site, name string) (bool, error) {
	tripped, err := IsTripped(ctx, q, site, name)
	if err != nil {
		return false, err
	}
	v := 0.0
	if tripped {
		v = 1
	}
	metrics.CircuitBreakerTripped.WithLabelValues(site, name).Set(v)
	return tripped, nil
}
