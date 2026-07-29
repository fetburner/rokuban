package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	// time/tzdata: Asia/Tokyo を OS のタイムゾーンデータベースに頼らず解決するための
	// blank import。配布イメージ（debian:bookworm-slim）に tzdata パッケージが
	// 入っているとは限らないため、バイナリに埋め込んでおく。
	_ "time/tzdata"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/epgstation"
	"github.com/fetburner/rokuban/internal/shadowdiff"
)

// newShadowDiffCmd は shadow-diff サブコマンドを作る（M2-14: 軽量シャドー差分）。
//
// 同じ mirakc に Rokuban と EPGStation をぶら下げて並走させているときに、両者の
// 予約集合を突き合わせて差分を人間可読なレポートとして出す。M2 の出口基準
// 「予約差分ゼロ or 全件説明可能」を測る道具そのもの（issue #6 / #24）。
func newShadowDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shadow-diff",
		Short: "Rokuban と EPGStation の予約差分をレポートする",
		Long: `同じ mirakc を共有する Rokuban と EPGStation の予約集合を programId で
突き合わせ、差分を標準出力にレポートする。

説明できない差分（RokubanOnly / EPGStationOnly）が 1 件でもあれば
終了コード 1 を返す（CI や runbook から && で連ねられるようにするため）。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			epgstationURL, err := cmd.Flags().GetString("epgstation-url")
			if err != nil {
				return err
			}

			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}

			ctx := cmd.Context()

			pool, err := db.NewPool(ctx, cfg.DB)
			if err != nil {
				return err
			}
			defer pool.Close()

			report, err := runShadowDiff(ctx, sqlcgen.New(pool), epgstation.NewClient(epgstationURL, nil), db.DefaultSite)
			if err != nil {
				return err
			}

			if err := printShadowDiffReport(cmd.OutOrStdout(), report); err != nil {
				return err
			}

			if report.HasUnexplained() {
				return fmt.Errorf("説明できない差分がある（RokubanOnly: %d 件, EPGStationOnly: %d 件）",
					len(report.RokubanOnly), len(report.EPGStationOnly))
			}
			return nil
		},
	}

	cmd.Flags().String("epgstation-url", "", "EPGStation の base URL（例: http://localhost:8888）")
	if err := cmd.MarkFlagRequired("epgstation-url"); err != nil {
		// フラグ名は定数なので、開発時のタイプミス以外では起こらない。
		panic(err)
	}

	return cmd
}

// runShadowDiff は Rokuban の DB と EPGStation の API から双方の予約集合を集めて
// 突き合わせる。DB / ネットワークとの往復はここに閉じ込め、比較ロジック自体は
// internal/shadowdiff.Compare（純関数）に委ねる。
func runShadowDiff(ctx context.Context, q *sqlcgen.Queries, epgClient *epgstation.Client, site string) (shadowdiff.Report, error) {
	epgReserves, err := epgClient.ListReserves(ctx)
	if err != nil {
		return shadowdiff.Report{}, fmt.Errorf("listing EPGStation reserves: %w", err)
	}

	// ListReservationsForSyncEvaluation（state <> 'orphaned'）を使う。detached の
	// 予約も mirakc に schedule が作られる（M2-4 で修正）ため、EPGStation との
	// 突き合わせ対象にも含めないと detached 予約が偽の EPGStationOnly として
	// 報告されてしまう。
	//
	// ただしこのクエリは候補を返すだけで、effective.skip による絞り込みは
	// 含まない（internal/db/queries/reservations.sql のコメント参照）。
	// db.EvaluateSyncCandidates（reconciler.listDesired と共通）に通して
	// 各行の skip 判定を得る --- reconciler は skip された予約を除外するが、
	// shadow-diff は除外せず Skipped フラグとして残す必要がある
	// （EPGStation 側に対応する予約があるとき Expected に落とすため）。
	// 以前はこの行のループが Skipped: false を決め打ちしており、M2-6 の
	// 重複排除が base.skip=true を立てた予約を「EPGStation と一致（Both）」と
	// 誤報告する見逃しがあった（issue #54）。
	rows, err := q.ListReservationsForSyncEvaluation(ctx, site)
	if err != nil {
		return shadowdiff.Report{}, fmt.Errorf("listing rokuban reservations: %w", err)
	}

	skipped, err := q.ListSkippedProgramIntentsBySite(ctx, site)
	if err != nil {
		return shadowdiff.Report{}, fmt.Errorf("listing rokuban skip intents: %w", err)
	}

	candidates := db.EvaluateSyncCandidates(rows)
	rokuban := make([]shadowdiff.RokubanReservation, 0, len(candidates)+len(skipped))
	for _, c := range candidates {
		if c.Err != nil {
			// 壊れた jsonb を Skipped: false 扱いで静かに握りつぶすと見逃しに
			// なるので、他の予約行に倣ってエラーを返す（不変条件: jsonb の
			// Unmarshal 失敗を握りつぶさない）。
			return shadowdiff.Report{}, fmt.Errorf("listing rokuban reservations: %w", c.Err)
		}
		rokuban = append(rokuban, shadowdiff.RokubanReservation{
			ProgramID: c.Reservation.ProgramID,
			Title:     c.Reservation.Title,
			StartAt:   c.Reservation.ProgramStartAt,
			Skipped:   c.Skipped,
		})
	}
	for _, s := range skipped {
		var title string
		if s.Name != nil {
			title = *s.Name
		}
		rokuban = append(rokuban, shadowdiff.RokubanReservation{
			ProgramID: s.ProgramID,
			Title:     title,
			StartAt:   s.ProgramStartAt,
			Skipped:   true,
		})
	}

	return shadowdiff.Compare(rokuban, epgReserves), nil
}

// jstDisplayFormat は番組表の慣習に合わせた JST の表示形式。
const jstDisplayFormat = "2006-01-02 15:04:05 MST"

// reportWriter は差分レポートの逐次書き込みをまとめ、最初に発生したエラーを保持する。
// io.Writer への書き込み 1 回ごとに if err != nil を書く定型コードを避けるための
// ごく薄いラッパー（bufio.Writer などでよく使われるパターン）。tabwriter からも
// 直接使えるよう io.Writer を実装する。
type reportWriter struct {
	w   io.Writer
	err error
}

// Write は io.Writer を満たす。最初に発生したエラーだけ rw.err に記録して
// 後から検査できるようにする（呼び出しごとの結果はそのまま返す）。
func (rw *reportWriter) Write(p []byte) (int, error) {
	n, err := rw.w.Write(p)
	if err != nil && rw.err == nil {
		rw.err = err
	}
	return n, err
}

func (rw *reportWriter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(rw, format, args...)
}

func (rw *reportWriter) println(args ...any) {
	_, _ = fmt.Fprintln(rw, args...)
}

// printShadowDiffReport は Report を人間可読な形で w に出力する。
// 時刻は番組表と揃えて JST で表示する（ローカルタイムゾーンではなく固定で Asia/Tokyo）。
func printShadowDiffReport(w io.Writer, report shadowdiff.Report) error {
	jst, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return fmt.Errorf("loading Asia/Tokyo location: %w", err)
	}

	rw := &reportWriter{w: w}
	rw.println("=== shadow-diff レポート ===")
	rw.printf("Both:            %d\n", len(report.Both))
	rw.printf("RokubanOnly:     %d\n", len(report.RokubanOnly))
	rw.printf("EPGStationOnly:  %d\n", len(report.EPGStationOnly))
	rw.printf("Expected:        %d\n", len(report.Expected))
	rw.println()

	if len(report.RokubanOnly) > 0 {
		rw.println("-- RokubanOnly（説明できない差分。EPGStation 側に対応する予約がない） --")
		printItemTable(rw, jst, report.RokubanOnly, false)
		rw.println()
	}

	if len(report.EPGStationOnly) > 0 {
		rw.println("-- EPGStationOnly（説明できない差分。Rokuban 側に対応する予約がない） --")
		printItemTable(rw, jst, report.EPGStationOnly, false)
		rw.println()
	}

	if len(report.Expected) > 0 {
		rw.println("-- Expected（allowlist で説明可能な差分） --")
		printItemTable(rw, jst, report.Expected, true)
		rw.println()
	}

	if !report.HasUnexplained() {
		rw.println("説明できない差分はない。")
	}

	if rw.err != nil {
		return fmt.Errorf("writing shadow-diff report: %w", rw.err)
	}
	return nil
}

// printItemTable は Item の一覧を programId / 題名 / 開始時刻（JST）の表として rw に出す。
// withReason が true なら理由列も付ける（Expected 用）。書き込みエラーは rw.err に
// 蓄積されるので、ここでは戻り値を扱わない。
func printItemTable(rw *reportWriter, jst *time.Location, items []shadowdiff.Item, withReason bool) {
	tw := tabwriter.NewWriter(rw, 0, 4, 2, ' ', 0)
	if withReason {
		_, _ = fmt.Fprintln(tw, "programId\ttitle\tstartAt (JST)\treason")
	} else {
		_, _ = fmt.Fprintln(tw, "programId\ttitle\tstartAt (JST)")
	}
	for _, item := range items {
		startAt := item.StartAt.In(jst).Format(jstDisplayFormat)
		// ProgramID == 0 は「programId を持たない予約」（時刻指定予約）の印。
		// mirakc の programId は実運用上ゼロにならないので目印として使える。
		programID := fmt.Sprintf("%d", item.ProgramID)
		if item.ProgramID == 0 {
			programID = "-"
		}
		if withReason {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", programID, item.Title, startAt, item.Reason)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", programID, item.Title, startAt)
		}
	}
	_ = tw.Flush()
}
