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
	"log/slog"
	"time"

	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
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

	// DeleteReconcile は削除 reconcile（M3-8）の物理 unlink を守る。ごみ箱 /
	// until_encoded / 孤児回収の 3 ソースをまとめた 1 パス分の削除件数が対象
	// （docs/storage.md §7「一括削除サーキットブレーカーはループ全体に 1 つ」）。
	DeleteReconcile = "delete_reconcile"
)

// siteless は行の site 列を持たない（IsSiteless が true を返す）ブレーカー
// 識別子の集合。今のところ DeleteReconcile だけ ---
// DeleteReconcileWorker の doc コメント参照: cleanup キューはどの束縛 site の
// worker が拾うか不定なので、行の site を実サイト名にすると 1 つの site
// 非依存の懸念（一括削除数）が複数の site 列に分散してしまう
// （不変条件 12「表は行の寿命で割る」に反する）。
//
// internal/api の 2 つの resume ハンドラ（ResumeCircuitBreaker /
// ResumeSitelessCircuitBreaker）と internal/worker/delete_reconcile.go の
// 書き込みはこの分類を共有する（判定を 2 箇所に書かない。issue #450）。
var siteless = map[string]bool{DeleteReconcile: true}

// IsSiteless は name が site を持たないブレーカーかを返す。true のとき、
// 行の site 列は実サイト名ではなく空文字列を持つ。
//
// 空文字列を exported な文字列定数にしないのは、all_test.go の AST スキャナが
// ブレーカー名ではない `""` を All に無い文字列定数として誤検出するため。
// 呼び出し側はリテラルの `""` を書く。
func IsSiteless(name string) bool {
	return siteless[name]
}

// All はここで定義している全ブレーカー識別子の一覧。internal/api はここから
// resume の妥当性検証に使う既知集合を導出する。
//
// この一覧は手書きなので、定数を足して All への追加を忘れる余地がある。
// 足し忘れは all_test.go の TestAll_MatchesDeclaredConstants が検出する。
// openapi.yaml の enum との一致は internal/api/breakers_test.go の
// TestBreakerAllNamesAreValidCircuitBreakerNameEnumMembers が見る。
var All = []string{RulerDeletes, ReconcileTotalLoss, DeleteReconcile}

// Sample は発動時に「何が消されようとしていたか」を説明するペイロード。
// 手動確認の材料であり、ブレーカー自身のロジックは中身を使わない。
type Sample = store.BreakerSample

// SampleProgram は確認用抜粋の 1 件。
type SampleProgram = store.BreakerSampleProgram

// MaxSampleSize は detail に載せる抜粋の上限。
const MaxSampleSize = 20

// TruncateSample は detail に保存するサンプルを上限件数に整える。
func TruncateSample(sample Sample) Sample {
	if len(sample.Programs) > MaxSampleSize {
		sample.Programs = sample.Programs[:MaxSampleSize]
	}
	return sample
}

// ObserveState はブレーカーの現在の発動状態を返し、メトリクスのゲージを合わせる。
//
// 各パスの先頭で呼ぶ。発動中なら削除を実行してはならない（作成・更新は続けてよい
// --- 止めたいのは削除だけで、レベルトリガーで収束させたい他の差分は止めない）。
//
// ゲージはプロセスの再起動で失われるので、各パスの先頭で呼んで DB の真実に
// 合わせ直す（レベルトリガー: メトリクスも導出値として毎回作り直す）。
func ObserveState(ctx context.Context, b store.Breakers, site, name string) (bool, error) {
	tripped, err := b.IsTripped(ctx, site, name)
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

// Trip はブレーカーを発動させる（既に発動中なら件数と抜粋を更新する）。
//
// 呼び出し側は Trip の後、その削除を実行してはならない。戻り値のエラーは
// 「発動の記録に失敗した」であり、その場合も削除は実行しない（記録できないまま
// 削除を続けるのが最悪の組み合わせ）。
func Trip(ctx context.Context, b store.Breakers, site, name string, threshold int, sample Sample) error {
	trippedAt, err := b.Trip(ctx, site, name, threshold, TruncateSample(sample))
	if err != nil {
		return err
	}
	announceTrip(site, name, threshold, sample, trippedAt)
	return nil
}

// announceTrip は発動をゲージとログに出す。Trip と TripInTx が共有する。
func announceTrip(site, name string, threshold int, sample Sample, trippedAt time.Time) {
	metrics.CircuitBreakerTripped.WithLabelValues(site, name).Set(1)
	slog.Error("circuit breaker tripped — deletes withheld until manually resumed",
		"site", site,
		"breaker", name,
		"pending_deletes", sample.Total,
		"threshold", threshold,
		"tripped_at", trippedAt,
	)
}
