// Package ruler はルール評価から reservations.base を導出するシングルトンループ。
//
// 1 パスで全ルール x 全射影番組を Postgres の集合演算で評価し（評価は全量）、
// 実際に値が変わった行だけ書く（書き込みは差分。docs/recording.md §3.1）。
// mirakc には一切触れない（不変条件 1）。真実は reservations と program_intents、
// および EPG プロジェクション（epg_programs / epg_services）のみ。
package ruler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/rulequery"
)

// Config は Ruler の設定。
type Config struct {
	// PassInterval は全量評価パスの周期。タイマーによる定期パスが真実で、
	// epg_sync 完了やルール編集はヒントに過ぎない（今回は未実装。将来 RunPass を
	// 前倒しで呼ぶ経路を足せるよう、ここは公開メソッドとして分けてある）。
	PassInterval time.Duration
	// MaxDeletesPerPass は 1 サイト・1 パスあたりの削除許容数。超えたら削除を
	// 一切実行せず、サーキットブレーカーとして停止する（reconciler.Config の
	// MaxDeletesPerPass と同じ考え方）。ここで守るのは「ルール x EPG」から導出される
	// 削除だけで、ユーザーの明示操作（DeleteReservation の intent{skip}）は
	// このブレーカーの対象にならない（そちら経由の削除は desired 集合の側で
	// 最初から除外されるため、ここには現れない）。
	MaxDeletesPerPass int

	// RetentionGrace は番組終了後、reservations / program_intents を GC するまでの
	// 猶予（issue #24 M2-3）。epg.retention_grace（EPG プロジェクションのロー
	// リングウィンドウ）と同じ値を流用する運用を想定しており、既定値も揃えてある。
	// GC はこのブレーカー（MaxDeletesPerPass）の対象にならない
	// （runGC のコメント参照）。
	RetentionGrace time.Duration
}

func defaultConfig() Config {
	return Config{
		PassInterval:      10 * time.Minute,
		MaxDeletesPerPass: 50,
		RetentionGrace:    24 * time.Hour,
	}
}

// Ruler はルール評価 → reservations.base 生成の全量評価ループ。
type Ruler struct {
	sites []string
	pool  *pgxpool.Pool
	cfg   Config
}

// New は Ruler を生成する。cfg が nil の場合はデフォルト設定を使う。
//
// sites はルールを評価する対象サイトの一覧。ルールはサイトに従属しないグローバルな
// 資産で（docs/recording.md §3.1「サイトの扱い」）、rule_sites が空なら全サイト、
// 非空ならそのサイトのみが対象になる（rulequery.Conditions.Sites 経由）。
// M1/M2 の設定は単一サイトなので db.DefaultSite 1 つで動くが、複数サイト構成に
// 備えて引数はスライスにしてある。
func New(sites []string, pool *pgxpool.Pool, cfg *Config) *Ruler {
	c := defaultConfig()
	if cfg != nil {
		if cfg.PassInterval > 0 {
			c.PassInterval = cfg.PassInterval
		}
		if cfg.MaxDeletesPerPass > 0 {
			c.MaxDeletesPerPass = cfg.MaxDeletesPerPass
		}
		if cfg.RetentionGrace > 0 {
			c.RetentionGrace = cfg.RetentionGrace
		}
	}
	return &Ruler{sites: sites, pool: pool, cfg: c}
}

// Run はルール評価ループを開始し、ctx がキャンセルされるまでブロックする。
func (r *Ruler) Run(ctx context.Context) error {
	if err := r.RunPass(ctx); err != nil {
		slog.Error("ruler: initial pass failed", "err", err)
	}

	ticker := time.NewTicker(r.cfg.PassInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := r.RunPass(ctx); err != nil {
				slog.Error("ruler: pass failed", "err", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// RunPass は全サイトに対して全量評価パスを 1 回実行し、続けて番組終了後の GC
// （runGC）を行う。
//
// タイマー（Run）から呼ばれるのが既定の起動契機だが、将来 epg_sync 完了や
// ルール編集をヒントに前倒しで呼ぶ経路を足せるよう、公開メソッドとして
// 独立させてある。
//
// 1 サイトの失敗が他サイトの評価を止めないよう、サイトごとに独立して実行し、
// 最初のエラーだけを返す（呼び出し側でログ済みのエラーは握り潰さない）。
// GC は site に従属しない全体操作なので、サイトのループの外で 1 回だけ行う。
func (r *Ruler) RunPass(ctx context.Context) error {
	start := time.Now()

	var firstErr error
	for _, site := range r.sites {
		if err := r.runPassForSite(ctx, site); err != nil {
			slog.Error("ruler: pass failed for site", "site", site, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("site %s: %w", site, err)
			}
			continue
		}
	}

	if err := r.runGC(ctx); err != nil {
		slog.Error("ruler: GC failed", "err", err)
		if firstErr == nil {
			firstErr = fmt.Errorf("gc: %w", err)
		}
	}

	metrics.RulerPassDuration.Observe(time.Since(start).Seconds())
	if firstErr == nil {
		metrics.RulerLastPass.SetToCurrentTime()
	}
	return firstErr
}

// runPassForSite は 1 サイト分の全量評価 + 差分書き込みを行う。
func (r *Ruler) runPassForSite(ctx context.Context, site string) error {
	q := sqlcgen.New(r.pool)

	rules, err := q.ListEnabledRules(ctx)
	if err != nil {
		return fmt.Errorf("listing enabled rules: %w", err)
	}

	// winner: programId ごとの勝者ルール（最初にマッチした = priority DESC, id ASC
	// の全順序で最初のルール）。allMatches: マッチした全ルール（reservation_rule_matches
	// 用。勝敗と無関係）。ListEnabledRules は既に priority DESC, id ASC で来るので、
	// 最初に書き込んだルールが常に勝者になる（docs/recording.md §3.1「複数ルール解決」）。
	ruleByID := make(map[int64]sqlcgen.Rule, len(rules))
	winner := make(map[int64]int64)
	allMatches := make(map[int64][]int64)
	for _, rule := range rules {
		ruleByID[rule.ID] = rule
		matched, err := rulequery.MatchProgramIDsForRule(ctx, r.pool, site, rule.ID)
		if err != nil {
			return fmt.Errorf("matching rule %d: %w", rule.ID, err)
		}
		for _, programID := range matched {
			if _, exists := winner[programID]; !exists {
				winner[programID] = rule.ID
			}
			allMatches[programID] = append(allMatches[programID], rule.ID)
		}
	}

	intents, err := q.ListProgramIntentActionsBySite(ctx, site)
	if err != nil {
		return fmt.Errorf("listing program intents: %w", err)
	}
	recordIntent := make(map[int64]struct{})
	skipIntent := make(map[int64]struct{})
	for _, in := range intents {
		switch in.Action {
		case db.IntentRecord:
			recordIntent[in.ProgramID] = struct{}{}
		case db.IntentSkip:
			skipIntent[in.ProgramID] = struct{}{}
		}
	}

	// desired = (ルールにマッチした番組 ∪ intent.record) − intent.skip
	// （docs/recording.md 案 A。program_intents は絶対に書かない — 読むだけ）。
	desired := make(map[int64]struct{}, len(winner)+len(recordIntent))
	for programID := range winner {
		desired[programID] = struct{}{}
	}
	for programID := range recordIntent {
		desired[programID] = struct{}{}
	}
	for programID := range skipIntent {
		delete(desired, programID)
	}

	existingProgramIDs, err := q.ListReservationProgramIDsBySite(ctx, site)
	if err != nil {
		return fmt.Errorf("listing existing reservation program ids: %w", err)
	}
	existingSet := make(map[int64]struct{}, len(existingProgramIDs))
	for _, id := range existingProgramIDs {
		existingSet[id] = struct{}{}
	}

	// 削除候補 = 既存予約のうち desired から外れたもの。ただし EPG プロジェクションから
	// 番組自体が消えている場合は「ルールがマッチしなくなった」と確信を持って判定できない
	// （評価する材料がないだけ）ので削除せず凍結する（docs/schema.md「射影にある間は
	// 更新、消えたら凍結」を削除判定にも適用する）。
	var deleteCandidates []int64
	for id := range existingSet {
		if _, ok := desired[id]; !ok {
			deleteCandidates = append(deleteCandidates, id)
		}
	}
	toDelete, err := r.stillProjectedSubset(ctx, q, site, deleteCandidates)
	if err != nil {
		return fmt.Errorf("checking still-projected candidates: %w", err)
	}

	desiredIDs := make([]int64, 0, len(desired))
	for id := range desired {
		desiredIDs = append(desiredIDs, id)
	}

	var snapshots []sqlcgen.ListProgramSnapshotsBySiteAndProgramIDsRow
	if len(desiredIDs) > 0 {
		snapshots, err = q.ListProgramSnapshotsBySiteAndProgramIDs(ctx, sqlcgen.ListProgramSnapshotsBySiteAndProgramIDsParams{
			Site:       site,
			ProgramIds: desiredIDs,
		})
		if err != nil {
			return fmt.Errorf("listing program snapshots: %w", err)
		}
	}
	snapshotByProgram := make(map[int64]sqlcgen.ListProgramSnapshotsBySiteAndProgramIDsRow, len(snapshots))
	for _, s := range snapshots {
		snapshotByProgram[s.ProgramID] = s
	}

	rows := make([]rulerInputRow, 0, len(desiredIDs))
	for _, programID := range desiredIDs {
		snap, hasProjection := snapshotByProgram[programID]
		if !hasProjection {
			if _, exists := existingSet[programID]; !exists {
				// 射影にもなく既存の予約行もない: スナップショットの材料が
				// どこにもないので作成できない。program_intents.action=record は
				// 単独では番組情報を持たないため、ここは待つしかない
				// （通常は CreateReservation が予約行と intent を同時に作るため
				// 起きないはずの経路。次のパスで射影が復活すれば拾える）。
				slog.Warn("ruler: cannot materialize reservation without projection or existing row",
					"site", site, "program_id", programID)
				continue
			}
		}

		var ruleID *int64
		var base json.RawMessage
		if rid, ok := winner[programID]; ok {
			ridCopy := rid
			ruleID = &ridCopy
			base, err = computeBase(ruleByID[rid])
			if err != nil {
				return fmt.Errorf("computing base for rule %d: %w", rid, err)
			}
		}

		row := rulerInputRow{
			ProgramID:     programID,
			RuleID:        ruleID,
			Base:          base,
			HasProjection: hasProjection,
		}
		if hasProjection {
			startAt := snap.StartAt
			durationMs := snap.DurationMs
			networkID := snap.NetworkID
			serviceID := snap.ServiceID
			channelType := snap.ChannelType
			channel := snap.Channel
			row.Title = snap.Title
			row.StartAt = &startAt
			row.DurationMs = &durationMs
			row.NetworkID = &networkID
			row.ServiceID = &serviceID
			row.ChannelType = &channelType
			row.Channel = &channel
		}
		rows = append(rows, row)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	results, err := upsertReservationsFromPass(ctx, tx, site, rows)
	if err != nil {
		return fmt.Errorf("upserting reservations: %w", err)
	}
	var created, updated int
	for _, res := range results {
		if res.Created {
			created++
		} else {
			updated++
		}
	}

	if err := rewriteRuleMatches(ctx, tx, site, allMatches, skipIntent); err != nil {
		return fmt.Errorf("rewriting reservation_rule_matches: %w", err)
	}

	tq := sqlcgen.New(tx)
	var deleted int64
	if len(toDelete) > r.cfg.MaxDeletesPerPass {
		metrics.RulerCircuitBreakerTrips.Inc()
		slog.Error("ruler: circuit breaker tripped — too many derived deletes in one pass",
			"site", site,
			"pending_deletes", len(toDelete),
			"threshold", r.cfg.MaxDeletesPerPass,
		)
	} else if len(toDelete) > 0 {
		deleted, err = tq.DeleteReservationsBySiteAndProgramIDs(ctx, sqlcgen.DeleteReservationsBySiteAndProgramIDsParams{
			Site:       site,
			ProgramIds: toDelete,
		})
		if err != nil {
			return fmt.Errorf("deleting stale reservations: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}

	metrics.RulerReservations.WithLabelValues("created").Add(float64(created))
	metrics.RulerReservations.WithLabelValues("updated").Add(float64(updated))
	metrics.RulerReservations.WithLabelValues("deleted").Add(float64(deleted))

	slog.Info("ruler: pass complete",
		"site", site,
		"rules", len(rules),
		"desired", len(desired),
		"created", created,
		"updated", updated,
		"deleted", deleted,
		"delete_candidates", len(deleteCandidates),
	)
	return nil
}

// runGC は番組終了 + RetentionGrace 経過の reservations / program_intents を
// 削除する（issue #24 M2-3、docs/schema.md §3「行の物理削除（GC）は『番組の
// 終了時刻を過ぎた後』のみ」）。state（active/detached/orphaned）を問わず、
// site にも従属しない全体操作なので RunPass のサイトループの外から 1 回だけ
// 呼ばれる。recordings.reservation_id は ON DELETE SET NULL なので、削除しても
// 録画履歴（recordings/media_assets）は失われない。
//
// **サーキットブレーカー（MaxDeletesPerPass）の対象にしない。** ブレーカーが
// 守るのは「ルール x EPG」の評価結果から導出される削除だけで、EPG の一時的な
// 欠損・フリッカーに引きずられて予約を大量に消してしまう事故を防ぐためのもの
// （docs/recording.md §3.2「大量削除サーキットブレーカー」）。GC の削除対象は
// 時刻の比較だけで決定的に定まり、EPG の状態に左右されない。むしろ
// reconciler/ruler が長時間停止していた後に溜まった期限切れ行を再開後に
// 一括で消すのは正常な挙動であり、ここをブレーカーで止めると本来消えるべき
// 行が積み上がり続けるだけで実害がない削除を止めてしまう。
func (r *Ruler) runGC(ctx context.Context) error {
	cutoff := time.Now().Add(-r.cfg.RetentionGrace)

	q := sqlcgen.New(r.pool)
	deletedReservations, err := q.DeleteEndedReservations(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("deleting ended reservations: %w", err)
	}
	deletedIntents, err := q.DeleteEndedProgramIntents(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("deleting ended program intents: %w", err)
	}

	metrics.RulerReservations.WithLabelValues("gc").Add(float64(deletedReservations))

	if deletedReservations > 0 || deletedIntents > 0 {
		slog.Info("ruler: GC complete",
			"cutoff", cutoff,
			"deleted_reservations", deletedReservations,
			"deleted_program_intents", deletedIntents,
		)
	}
	return nil
}

// stillProjectedSubset は candidates のうち、EPG プロジェクションに現在も番組がある
// ものだけを返す。空なら問い合わせずに空を返す。
func (r *Ruler) stillProjectedSubset(ctx context.Context, q *sqlcgen.Queries, site string, candidates []int64) ([]int64, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	return q.ListEpgProgramIDsBySiteAndProgramIDs(ctx, sqlcgen.ListEpgProgramIDsBySiteAndProgramIDsParams{
		Site:       site,
		ProgramIds: candidates,
	})
}

// rewriteRuleMatches は reservation_rule_matches を今回のマッチ結果で書き換える。
// この表に SSE 用の行トリガーはないため、reservations と違い差分書き込みは要求されない
// （毎パス全部書き直してよい。docs/recording.md §3.1「複数ルール解決」）。
//
// 対象は「ルールにマッチし、かつ intent.skip で desired から除外されていない」番組。
// skip された番組は予約行そのものを持たないため、マッチのトレースを紐づける先がない。
func rewriteRuleMatches(ctx context.Context, tx pgx.Tx, site string, allMatches map[int64][]int64, skipIntent map[int64]struct{}) error {
	programIDs := make([]int64, 0, len(allMatches))
	for programID := range allMatches {
		if _, skipped := skipIntent[programID]; skipped {
			continue
		}
		programIDs = append(programIDs, programID)
	}
	if len(programIDs) == 0 {
		return nil
	}

	q := sqlcgen.New(tx)
	reservationRows, err := q.ListReservationIDsBySiteAndProgramIDs(ctx, sqlcgen.ListReservationIDsBySiteAndProgramIDsParams{
		Site:       site,
		ProgramIds: programIDs,
	})
	if err != nil {
		return fmt.Errorf("listing reservation ids: %w", err)
	}

	reservationIDByProgram := make(map[int64]int64, len(reservationRows))
	reservationIDs := make([]int64, 0, len(reservationRows))
	for _, row := range reservationRows {
		reservationIDByProgram[row.ProgramID] = row.ID
		reservationIDs = append(reservationIDs, row.ID)
	}
	if len(reservationIDs) == 0 {
		return nil
	}

	if err := q.DeleteReservationRuleMatchesByReservationIDs(ctx, reservationIDs); err != nil {
		return fmt.Errorf("deleting old reservation_rule_matches: %w", err)
	}

	var matchReservationIDs, matchRuleIDs []int64
	for programID, ruleIDs := range allMatches {
		reservationID, ok := reservationIDByProgram[programID]
		if !ok {
			// 対応する予約行がない（skip 済み or この直前の削除で消えた等）。
			// マッチの記録先がないので静かにスキップする。
			continue
		}
		for _, ruleID := range ruleIDs {
			matchReservationIDs = append(matchReservationIDs, reservationID)
			matchRuleIDs = append(matchRuleIDs, ruleID)
		}
	}

	return insertReservationRuleMatches(ctx, tx, matchReservationIDs, matchRuleIDs)
}

// computeBase は勝者ルールから reservations.base（jsonb）を組む。
//
// フィールドは docs/schema.md §8「予約オプション」のうち、勝者ルールから決まる部分
// （priority / encodeProfiles / keepOriginal / filenameTemplate）に限る。
// **contentPath（展開済みのフルパス）は含めない** — reconciler が初回生成した値を
// base に固定する契約になっており（issue #19）、ruler が毎パス書き直すと EPG
// 更新のたびに mirakc の schedule が作り直される churn を招く。
//
// filenameTemplate はこれと事情が違う: テンプレート文字列そのものはルール編集
// でしか変わらず、EPG 更新（番組名・時刻のスナップショット変化）では変化しない。
// そのため base に載せても contentPath と同じ churn は起きない —
// 展開（テンプレート文字列 → 実パス）は reconciler の役目のまま変わらない。
// 空文字なら載せない（base を最小に保ち、既定の固定形式利用時に
// IS DISTINCT FROM の差分書き込みが空振りしないため）。
//
// skip も含めない（program_intents.action が担うフィールドで、
// base 側で表現すると優先順位の合成に jsonb マージの細工が要る）。
func computeBase(rule sqlcgen.Rule) (json.RawMessage, error) {
	priority := int(rule.Priority)
	keepOriginal := rule.KeepOriginal
	profiles := append([]string(nil), rule.EncodeProfiles...)

	opts := db.ReservationOptions{
		Priority:       &priority,
		KeepOriginal:   &keepOriginal,
		EncodeProfiles: &profiles,
	}
	if rule.FilenameTemplate != "" {
		filenameTemplate := rule.FilenameTemplate
		opts.FilenameTemplate = &filenameTemplate
	}
	out, err := json.Marshal(opts)
	if err != nil {
		return nil, fmt.Errorf("marshalling base options: %w", err)
	}
	return out, nil
}
