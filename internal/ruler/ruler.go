// Package ruler はルール評価から reservations.base を導出する 1 パス評価ロジック。
//
// ruler はシングルトンではなく River のジョブ（internal/worker の RulerPassWorker）
// として実行される。定期・冪等・DB のみ・重複実行不可という性質が epg_sync と同じで、
// 排他は advisory lock ではなくジョブロック + UniqueOpts（サイト単位）で担保する
// （docs/data.md §2）。このパッケージは 1 パス分の評価ロジックだけを持ち、いつ・
// どの契機で呼ぶか（定期実行の起動契機はデプロイ形態に委ねる）は呼び出し側の責務。
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
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/rulequery"
)

// Config は Ruler の設定。
type Config struct {
	// MaxDeletesPerPass は 1 サイト・1 パスあたりの削除許容数。超えたら削除を
	// 一切実行せず、サーキットブレーカーとして停止する。**数えるのは「ルールが
	// base を供給していたのにマッチしなくなった」削除だけ**で、ユーザーの明示操作
	// からしか説明できない削除（intent skip／intent クリア／最後の investment
	// だった overrides の削除）は数にも入らず、ラッチ中でも実行される
	// （分類の条件と根拠は internal/db/queries/ruler.sql の
	// DeleteReleasedReservationsBySiteAndProgramIDs、判断は docs/recording.md
	// §3.2「大量削除サーキットブレーカー」）。ルールの**編集**で勝者が変わる経路は
	// rule_id が残るのでブレーカー対象のままで、ルールの**削除**はそもそもここを
	// 経由しない（API ハンドラが同一トランザクションで reservations を直接 DELETE
	// する。同 §3.2「止められる場所は ruler だけ」の表）。
	//
	// **発動は internal/breaker によりラッチとして永続化される**（issue #24 M2-5）。
	// 件数が閾値以下に戻っても自動では解除されず、
	// `POST /api/sites/{site}/breakers/ruler_deletes/resume`
	// による手動再開まで導出削除を止め続ける（docs/recording.md §3.2「発動はラッチ」）。
	// reconciler 側には同種の閾値はもう無い（撤去済み。同 §3.2「止められる場所は
	// ruler だけ」）— ここが唯一導出削除を止められる場所になる。
	MaxDeletesPerPass int

	// RetentionGrace は番組終了後、reservations / program_intents を GC するまでの
	// 猶予（issue #24 M2-3）。epg.retention_grace（EPG プロジェクションのロー
	// リングウィンドウ）と同じ値を流用する運用を想定しており、既定値も揃えてある。
	// GC はこのブレーカー（MaxDeletesPerPass）の対象にならない
	// （runGC のコメント参照）。
	RetentionGrace time.Duration

	// RetractGrace は放送開始直前にルールから外れた予約を、このパスでは削除しない
	// 猶予（denpa の `RULE_RETRACT_GRACE` に倣う。docs/recording/ruler.md §3.1
	// 「直前 unmatch の猶予」）。**MaxDeletesPerPass / RetentionGrace と違い、この
	// パッケージ自身は既定値を持たない** --- 0（ゼロ値）はそのまま「無効」を意味し、
	// New はこれを 0 より大きい値に読み替えない。本番の既定 1h は呼び出し側
	// （internal/config.RulerConfig.RetractGrace）が埋める。ここで package 既定値
	// （例: 1h）を持たせると、この値を一切気にしない既存の全テスト・呼び出し元
	// （ruler.New(sites, pool, nil) や cfg.RetractGrace を設定しない Config{}）が
	// 黙って猶予ありの挙動に変わり、削除を確認するテストが不安定にサイレント破壊
	// される。0 を「無効」の後方互換な既定にすることで、猶予は明示的に opt-in する
	// 機能になる。
	RetractGrace time.Duration
}

func defaultConfig() Config {
	return Config{
		MaxDeletesPerPass: 50,
		RetentionGrace:    24 * time.Hour,
		// RetractGrace の既定は 0（無効）。RetractGrace のフィールドコメント参照。
	}
}

// Ruler はルール評価 → reservations.base 生成の 1 パス評価を行う。
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
// 現状の設定（config.mirakcs）は単一サイト構成が多いのでその 1 つで動くが、複数サイト
// 構成に備えて引数はスライスにしてある。呼び出し元の
// internal/worker.RulerPassWorker はジョブ引数のサイト 1 つだけを渡す
// （ジョブの排他がサイト単位のため）。
func New(sites []string, pool *pgxpool.Pool, cfg *Config) *Ruler {
	c := defaultConfig()
	if cfg != nil {
		if cfg.MaxDeletesPerPass > 0 {
			c.MaxDeletesPerPass = cfg.MaxDeletesPerPass
		}
		if cfg.RetentionGrace > 0 {
			c.RetentionGrace = cfg.RetentionGrace
		}
		// RetractGrace は 0 も意味のある値（無効化）なので、他の 2 つと違い
		// ">0 のときだけ上書き" にしない。cfg が渡された時点でその値をそのまま使う。
		c.RetractGrace = cfg.RetractGrace
	}
	return &Ruler{sites: sites, pool: pool, cfg: c}
}

// RunPass は全サイトに対して全量評価パスを 1 回実行し、続けて番組終了後の GC
// （runGC）を行う。
//
// ruler はシングルトンではなく River のジョブ（internal/worker の RulerPassWorker）
// として呼ばれる。起動契機は定期実行（真実）・ルール編集・EPG 同期完了（いずれも
// ヒント）の 3 つがあるが、すべて RunPass を呼ぶ 1 本の経路に合流する
// （docs/recording.md §3.1）。
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

	// パスの先頭でブレーカーの発動状態を DB の真実に合わせ直す（ゲージはプロセス
	// 再起動で失われるため。breaker.ObserveState のコメント参照）。true なら
	// このパスでは導出削除を一切実行しない（下のスイッチの tripped 分岐）。
	// 作成・更新・base の再計算・GC は止めない — 止めたいのは削除だけ。
	tripped, err := breaker.ObserveState(ctx, q, site, breaker.RulerDeletes)
	if err != nil {
		return fmt.Errorf("observing %s circuit breaker: %w", breaker.RulerDeletes, err)
	}

	rules, err := q.ListEnabledRules(ctx)
	if err != nil {
		return fmt.Errorf("listing enabled rules: %w", err)
	}

	// winner: programId ごとの勝者ルール（最初にマッチした = priority DESC, id ASC
	// の全順序で最初のルール）。ListEnabledRules は既に priority DESC, id ASC で
	// 来るので、最初に書き込んだルールが常に勝者になる
	// （docs/recording.md §3.1「複数ルール解決」）。負けたルールは reservations
	// のどの列にも供給しないので保持しない --- 必要になれば enabled ルールを
	// rulequery.MatchProgramIDsForRule で回せば同じ集合が作り直せる。
	ruleByID := make(map[int64]sqlcgen.Rule, len(rules))
	winner := make(map[int64]int64)
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
		}
	}

	// skip 意図だけをここで読む。「record 意図または overrides の行がある」は
	// program_investments view（#162）に一本化したので、record 側は
	// ListProgramInvestmentProgramIDsBySite から引く（下記）。
	intents, err := q.ListProgramIntentActionsBySite(ctx, site)
	if err != nil {
		return fmt.Errorf("listing program intents: %w", err)
	}
	skipIntent := make(map[int64]struct{})
	for _, in := range intents {
		if in.Action == db.IntentSkip {
			skipIntent[in.ProgramID] = struct{}{}
		}
	}

	// 「この番組にユーザーの投資があるか」（record 意図 ∪ overrides の行）は
	// program_investments view（#162）から引く。ruler は overrides の中身も
	// record 意図の中身も一切読まないので programId だけを取る。
	investmentProgramIDs, err := q.ListProgramInvestmentProgramIDsBySite(ctx, site)
	if err != nil {
		return fmt.Errorf("listing program investments: %w", err)
	}

	// desired = (ルールにマッチした番組 − intent.skip) ∪ program_investments
	// （docs/recording.md §4.2「ruler から見た load-bearing な行」。
	// program_intents / program_overrides は絶対に書かない — 読むだけ）。
	//
	// investment（record 意図 ∪ overrides）は skip を引いた後の winner に
	// 無条件で足す。順序を入れ替えても record 側の結果は変わらない ---
	// `program_intents` は (site, program_id) に 1 行しか持てないため
	// action='record' と action='skip' は同じ番組で排他であり、winner から
	// skip を引く操作は record 側の投資に触れない。overrides 側は skip と
	// 独立に存在できるが、investment に無条件で足すことで「skip 意図があっても
	// overrides は desired に残す」（§4.3「record 意図または上書きがある →
	// 削除せず detached で保持」）を満たす。skip 側は intent.action='skip' が
	// effective.skip として引き続き効くので（db.EffectiveOptions）、reconciler は
	// この行を同期しない。行の存在が答えるのは「この番組にユーザーの投資が
	// あるか」で、録画するかどうかとは別の問い。
	desired := make(map[int64]struct{}, len(winner)+len(investmentProgramIDs))
	for programID := range winner {
		if _, skipped := skipIntent[programID]; !skipped {
			desired[programID] = struct{}{}
		}
	}
	for _, programID := range investmentProgramIDs {
		desired[programID] = struct{}{}
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

	// 重複排除（M2-6）: 勝者ルールが dedupe_enabled な番組について、同じルールで
	// 既に録れている番組を探す。判定は base の計算より前に済ませる必要がある
	// （結果が base.skip に載る）。候補は勝者ルールがある番組だけ --- ルールが
	// base を供給していない予約（手動予約・detached）の base は凍結されるので、
	// 重複排除の判定対象でもない。
	var dedupeCandidates []dedupeCandidate
	for _, programID := range desiredIDs {
		ruleID, ok := winner[programID]
		if !ok {
			continue
		}
		if !ruleByID[ruleID].DedupeEnabled {
			continue
		}
		dedupeCandidates = append(dedupeCandidates, dedupeCandidate{ProgramID: programID, RuleID: ruleID})
	}
	dedupeMatches, err := evaluateDedupe(ctx, r.pool, site, dedupeCandidates)
	if err != nil {
		return fmt.Errorf("evaluating dedupe: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := sqlcgen.New(tx)

	// program_snapshots への追従更新（#27）。「射影にある間は更新、消えたら凍結」を
	// 担う唯一の書き手がここ。snapshot は番組の事実を保持する表であり、skip 意図の
	// ような不可逆なユーザー操作を表す列ではないため、skip 意図だけが行を支える場合も
	// 射影にある限り追従させる（GC の終了判定で意図を CASCADE させないため）。
	// なぜ existingSet も対象に含めるか: 猶予やラッチで残る非 desired な行も「射影に
	// まだ居る」限り追従させるため（issue #556、docs/schema/reservations.md §3.7）。凍結を保つのは
	// UpsertProgramSnapshotsFromProjection 内の epg_programs との JOIN 自体で、
	// 射影に無い programId をここに含めても何もされない。reservations.program_fkey
	// が program_snapshots を参照するため、予約行の upsert より先に実行する
	// 必要がある。
	snapshotSyncIDs := slices.Clone(desiredIDs)
	for id := range skipIntent {
		if _, ok := desired[id]; !ok {
			snapshotSyncIDs = append(snapshotSyncIDs, id)
		}
	}
	for id := range existingSet {
		if _, ok := desired[id]; !ok {
			snapshotSyncIDs = append(snapshotSyncIDs, id)
		}
	}
	if len(snapshotSyncIDs) > 0 {
		if _, err := tq.UpsertProgramSnapshotsFromProjection(ctx, sqlcgen.UpsertProgramSnapshotsFromProjectionParams{
			Site:       site,
			ProgramIds: snapshotSyncIDs,
		}); err != nil {
			return fmt.Errorf("upserting program snapshots from projection: %w", err)
		}
	}

	// 新規に予約行を作れるのは program_snapshots に行がある programId だけ
	// （FK）。上の Upsert で射影にあるものは今作られたばかり、無いものは
	// 既存の意図・上書き・予約から過去に作られたものだけが該当する。
	var materializable map[int64]struct{}
	if len(desiredIDs) > 0 {
		ids, err := tq.ListProgramSnapshotProgramIDsBySiteAndProgramIDs(ctx, sqlcgen.ListProgramSnapshotProgramIDsBySiteAndProgramIDsParams{
			Site:       site,
			ProgramIds: desiredIDs,
		})
		if err != nil {
			return fmt.Errorf("checking program snapshot existence: %w", err)
		}
		materializable = make(map[int64]struct{}, len(ids))
		for _, id := range ids {
			materializable[id] = struct{}{}
		}
	}

	rows := make([]rulerInputRow, 0, len(desiredIDs))
	for _, programID := range desiredIDs {
		if _, ok := materializable[programID]; !ok {
			// 射影にも program_snapshots にもスナップショットの材料が無い:
			// program_intents.action=record は単独では番組情報を持たないため、
			// ここは待つしかない（通常は CreateReservation が予約行と intent を
			// 同時に作るため起きないはずの経路。次のパスで射影が復活すれば拾える）。
			slog.Warn("ruler: cannot materialize reservation without a program snapshot",
				"site", site, "program_id", programID)
			continue
		}

		var ruleID *int64
		var base json.RawMessage
		var dedupMatchRecordingID *int64
		var dedupSimilarity *float32
		if rid, ok := winner[programID]; ok {
			ridCopy := rid
			ruleID = &ridCopy
			// マッチが無ければ（dedupe_enabled=false の場合も含め）両方 nil の
			// まま = NULL に戻す。前のパスでマッチしていて今回のパスで似た録画が
			// 無くなったなら、古い根拠を残してはいけない（導出値は毎パス作り直す。
			// CLAUDE.md 不変条件 9）。
			match, matched := dedupeMatches[programID]
			if matched {
				recordingID := match.RecordingID
				similarity := match.Similarity
				dedupMatchRecordingID = &recordingID
				dedupSimilarity = &similarity
			}
			base, err = computeBase(ruleByID[rid], matched)
			if err != nil {
				return fmt.Errorf("computing base for rule %d: %w", rid, err)
			}
		}

		rows = append(rows, rulerInputRow{
			ProgramID:             programID,
			RuleID:                ruleID,
			Base:                  base,
			DedupMatchRecordingID: dedupMatchRecordingID,
			DedupSimilarity:       dedupSimilarity,
		})
	}

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

	// ユーザー（運用者）が投資を手放す書き込みをしない限り起きない削除を先に、
	// ブレーカーとは無関係に実行する（docs/recording.md §3.2「大量削除サーキット
	// ブレーカー」）。toDelete 全体を渡し、どれがそれに当たるかは DELETE 文の
	// WHERE が適用の瞬間に判定して RETURNING で返す — 分類をトランザクション外の
	// 古い読み取りに置かないため（#29 型の窓を作らない）。条件と、その条件で
	// 守備範囲が狭まらない根拠（および狭まる境界）は
	// internal/db/queries/ruler.sql の同クエリのコメントが権威。
	var released []int64
	if len(toDelete) > 0 {
		released, err = tq.DeleteReleasedReservationsBySiteAndProgramIDs(ctx, sqlcgen.DeleteReleasedReservationsBySiteAndProgramIDsParams{
			Site:       site,
			ProgramIds: toDelete,
		})
		if err != nil {
			return fmt.Errorf("deleting user-released reservations: %w", err)
		}
	}
	// ブレーカーが数えるのは残り（= ルールが base を供給していたのにマッチしなく
	// なった行）だけ。EPG の欠損・フリッカーが作れるのはこちらの集合に限られる。
	derivedDeletes := subtract(toDelete, released)

	// 猶予（ruler.retract_grace, issue #428）: 開始直前にルールから外れた予約は
	// このパスでは引っ込めない。「猶予中」を列には焼かず、削除候補から都度除く導出
	// 規則にする（CLAUDE.md 不変条件 9）。
	//
	// **released（明示操作: intent skip / intent クリア / 最後の investment だった
	// overrides の削除）を引いた後の derivedDeletes に適用する** --- toDelete
	// 全体に適用すると、rule_id が前パスから非 NULL のままユーザーが
	// intent{skip} を立てた行まで猶予が守ってしまい、「これは録らない」という
	// 明示操作が直前の猶予に呑まれて DeleteReleasedReservationsBySiteAndProgramIDs
	// が一生見ない行になる（released はこの少し上で先に実行済み。猶予はそこには
	// 一切関与しない）。derivedDeletes まで絞ってから猶予を掛けることで、
	// 猶予が保護する対象はブレーカー対象の集合そのものになる
	// （docs/recording/breaker.md「大量削除サーキットブレーカー」の猶予との関係）。
	//
	// このクエリは tx 内（tq）で読む。derivedDeletes の programId はどれも
	// 今パスの desired に無い＝upsertReservationsFromPass の入力行（rows）にも
	// DeleteReleasedReservationsBySiteAndProgramIDs の対象にも含まれないので、
	// ここまでの tx 内の書き込みで触られていない --- reservations.rule_id は
	// 前パスの値のまま読める（罠: 今パスの評価結果で NULL に落ちた後に見ても遅い）。
	// tx 内で読むことは「読み取りと適用の間の窓を消す」ためではない（`r.pool.Begin`
	// は既定の READ COMMITTED で、文ごとに新しいスナップショットを取るため、この
	// SELECT とこの後の DELETE の間に他コミットが割り込む窓は同じ tx 内でも残る
	// --- SERIALIZABLE / REPEATABLE READ でなければ消えない）。
	//
	// 安全性が成り立つのは窓が無いからではなく、**猶予が削除集合からの減算にしか
	// 効かない**からである。判定が古すぎて過大（stale-too-large）でも、次のパスが
	// 全量再評価するので 1 パス遅れるだけで収束する（レベルトリガー、自己修復）。
	// 判定が古すぎて過小（stale-too-small = 本当は保護すべき行を見落とす）はほぼ
	// 起き得ない:
	//   - ルールが再度 enabled になった → 次のパスで desired が作り直す
	//   - 投資（record 意図 ∪ overrides）が新たに付いた → DELETE 文自身の
	//     `NOT EXISTS program_investments` が適用の瞬間に再評価する（#29 型の窓を
	//     作らない設計は DeleteReservationsBySiteAndProgramIDs 側が既に担っている）
	//   - start_at が猶予の窓に入ってきた → 猶予の述語は epg_programs.start_at
	//     （射影の最新値）を直接見る。残る窓は internal/db/queries/ruler.sql の
	//     ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs のコメントが権威。
	//
	// r.cfg.RetractGrace <= 0 は「無効」（RetractGrace のフィールドコメント参照）。
	//
	// graceProtectedCount は下のログのためだけに持ち越す。猶予で残った行は
	// ブレーカーのラッチと同じ見え方（delete_candidates はあるのに
	// deleted/released が 0）になるため、ログで区別できるようにする。
	var graceProtectedCount int
	if r.cfg.RetractGrace > 0 && len(derivedDeletes) > 0 {
		graceProtected, err := r.retractGraceProtectedSubset(ctx, tq, site, derivedDeletes)
		if err != nil {
			return fmt.Errorf("checking retract grace: %w", err)
		}
		graceProtectedCount = len(graceProtected)
		derivedDeletes = subtract(derivedDeletes, graceProtected)
	}

	var deleted int64
	// newlyTripped: このパスで初めて発動した（tripped が false だった）ことを示す
	// フラグ。metrics.RulerCircuitBreakerTrips.Inc() は tx.Commit の成否が
	// まだ分からないこの時点では呼ばない — ここで呼ぶと後段の Commit が失敗した
	// 場合にカウンタだけ進んでしまう（DB には発動が記録されていないのに
	// メトリクスだけ発動したことになる）。フラグに持ち越し、Commit 成功後にのみ
	// Inc する。breaker.Trip 内のゲージ設定は次パスの ObserveState が DB の
	// 真実に合わせ直すので、ここでは触らなくてよい。
	var newlyTripped bool
	switch {
	case len(derivedDeletes) > r.cfg.MaxDeletesPerPass:
		// 閾値超過。tripped が既に true でも Trip を呼び直す — 既に発動中なら
		// tripped_at は据え置かれたまま pending/detail だけが最新の値に更新される
		// （TripCircuitBreaker の ON CONFLICT。「いつから止まっているか」を保つ一方、
		// 手動確認の材料は最新に保つ）。どちらの場合もこの分岐では削除しない。
		sample, sampleErr := r.buildDeleteSample(ctx, tq, site, derivedDeletes)
		if sampleErr != nil {
			return fmt.Errorf("building circuit breaker sample: %w", sampleErr)
		}
		// Trip がエラーを返した場合もこの分岐からは削除を実行しない
		// （記録できないまま削除を続けるのが最悪の組み合わせ。breaker.Trip のコメント）。
		// tx 内で呼ぶことで、発動の記録と「このパスでは削除しない」を一体に保つ。
		if tripErr := breaker.Trip(ctx, tq, site, breaker.RulerDeletes, r.cfg.MaxDeletesPerPass, sample); tripErr != nil {
			return fmt.Errorf("tripping circuit breaker: %w", tripErr)
		}
		newlyTripped = !tripped
	case tripped:
		// ラッチ中: 今回の候補数は閾値以下に戻っているが、自動では解除しない
		// （breaker パッケージのコメント「自動で解けるようにすると『一瞬止まって
		// 自動復帰した』がアラートに残らない」）。再開は人間が
		// POST /api/sites/{site}/breakers/ruler_deletes/resume を叩くまで待つ。
		if len(derivedDeletes) > 0 {
			slog.Warn("ruler: circuit breaker latched — withholding derived deletes until manually resumed",
				"site", site,
				"breaker", breaker.RulerDeletes,
				"pending_deletes", len(derivedDeletes),
			)
		}
	case len(derivedDeletes) > 0:
		deleted, err = tq.DeleteReservationsBySiteAndProgramIDs(ctx, sqlcgen.DeleteReservationsBySiteAndProgramIDsParams{
			Site:       site,
			ProgramIds: derivedDeletes,
		})
		if err != nil {
			return fmt.Errorf("deleting stale reservations: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}

	// Commit が成功して初めて「このパスで実際に発動した」と言えるので、ここで
	// カウンタを進める（switch 内で直接 Inc すると Commit 失敗時にもカウンタが
	// 進んでしまう）。
	if newlyTripped {
		metrics.RulerCircuitBreakerTrips.Inc()
	}

	metrics.RulerReservations.WithLabelValues("created").Add(float64(created))
	metrics.RulerReservations.WithLabelValues("updated").Add(float64(updated))
	metrics.RulerReservations.WithLabelValues("deleted").Add(float64(deleted))
	// released はブレーカーを通っていない削除なので deleted と分けて数える。
	// 混ぜると「閾値を下回る導出削除が素通りしていないか」を deleted の増え方で
	// 見る運用（docs/operations.md §2）が、明示操作の分で汚れる。
	metrics.RulerReservations.WithLabelValues("released").Add(float64(len(released)))
	// grace_protected はカウンタにしない: 他の 5 値は「行が 1 回寄与するエッジ」
	// だが、猶予で残った行は毎パス（既定 10 分）再計上される「水準」なので、
	// increase() で見ると値がパス頻度に比例してしまい、録れた予約の数を意味しない
	// （1 件の猶予が 10 分間隔 x 1h 窓で ~6 に膨らむ）。ログの grace_protected
	// フィールドだけで、ブレーカーのラッチと見分ける目的は十分に果たせる。

	slog.Info("ruler: pass complete",
		"site", site,
		"rules", len(rules),
		"desired", len(desired),
		"created", created,
		"updated", updated,
		"deleted", deleted,
		"released", len(released),
		"grace_protected", graceProtectedCount,
		"delete_candidates", len(deleteCandidates),
	)
	return nil
}

// runGC は番組終了 + RetentionGrace 経過の reservations / program_intents を
// 削除する（issue #24 M2-3、docs/schema.md §3「行の物理削除（GC）は『番組の
// 終了時刻を過ぎた後』のみ」）。state（active/detached。orphaned は issue #98
// で recordings 側の観測になったため、この GC はそもそも関知しない）を問わず、
// site にも従属しない全体操作なので RunPass のサイトループの外から 1 回だけ
// 呼ばれる。recordings.reservation_id は当時 ON DELETE SET NULL だった
// （issue #158 で列自体を削除済み）ので、削除しても録画履歴
// （recordings/media_assets）は失われない。
//
// **サーキットブレーカー（MaxDeletesPerPass）の対象にしない。** ブレーカーが
// 守るのは「ルール x EPG」の評価結果から導出される削除だけで、EPG の一時的な
// 欠損・フリッカーに引きずられて予約を大量に消してしまう事故を防ぐためのもの
// （docs/recording.md §3.2「大量削除サーキットブレーカー」）。GC の削除対象は
// 時刻の比較だけで決定的に定まり、EPG の状態に左右されない。むしろ
// reconciler/ruler が長時間停止していた後に溜まった期限切れ行を再開後に
// 一括で消すのは正常な挙動であり、ここをブレーカーで止めると本来消えるべき
// 行が積み上がり続けるだけで実害がない削除を止めてしまう。
//
// ブレーカーがラッチ化された（M2-5）後もこれは変わらない: runGC は
// breaker.ObserveState / IsTripped を一度も呼ばない。runPassForSite の
// tripped 分岐が止めるのは自分が実行する DeleteReservationsBySiteAndProgramIDs
// だけで、GC はサイトのループの外・別の関数として独立に動き続ける。
func (r *Ruler) runGC(ctx context.Context) error {
	cutoff := time.Now().Add(-r.cfg.RetentionGrace)

	// #27 で GC は DeleteEndedProgramSnapshots 1 本に集約された。
	// program_snapshots への FK が ON DELETE CASCADE なので、reservations /
	// program_intents / program_overrides の 3 表がまとめて落ちる（移行前は
	// 3 本の DELETE がそれぞれ別のスナップショット列を見ており、ドリフトして
	// いたので表ごとに違う時刻で GC していた。docs/schema.md §3「行の物理削除
	// （GC）は『番組の終了時刻を過ぎた後』のみ」）。recordings.reservation_id は
	// 当時 ON DELETE SET NULL だった（issue #158 で列自体を削除済み）ので、
	// 録画履歴（recordings/media_assets）はこの削除の影響を受けない。
	q := sqlcgen.New(r.pool)
	deletedSnapshots, err := q.DeleteEndedProgramSnapshots(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("deleting ended program snapshots: %w", err)
	}

	metrics.RulerReservations.WithLabelValues("gc").Add(float64(deletedSnapshots))

	if deletedSnapshots > 0 {
		slog.Info("ruler: GC complete",
			"cutoff", cutoff,
			"deleted_program_snapshots", deletedSnapshots,
		)
	}
	return nil
}

// buildDeleteSample は発動時に breaker.Trip へ渡す breaker.Sample を組む。
// Total は toDelete の全件数、Programs は手動確認用の抜粋（先頭 breaker.MaxSampleSize
// 件のタイトルスナップショット）。件数が多いときこそ発動するので、抜粋の対象を
// 絞ってから DB に問い合わせる（無駄に大きな IN リストを作らない）。
func (r *Ruler) buildDeleteSample(ctx context.Context, q *sqlcgen.Queries, site string, toDelete []int64) (breaker.Sample, error) {
	sampleIDs := toDelete
	if len(sampleIDs) > breaker.MaxSampleSize {
		sampleIDs = sampleIDs[:breaker.MaxSampleSize]
	}

	titles, err := q.ListReservationTitlesBySiteAndProgramIDs(ctx, sqlcgen.ListReservationTitlesBySiteAndProgramIDsParams{
		Site:       site,
		ProgramIds: sampleIDs,
	})
	if err != nil {
		return breaker.Sample{}, fmt.Errorf("listing reservation titles for circuit breaker sample: %w", err)
	}

	programs := make([]breaker.SampleProgram, 0, len(titles))
	for _, t := range titles {
		programs = append(programs, breaker.SampleProgram{ProgramID: t.ProgramID, Title: t.Title})
	}
	return breaker.Sample{Total: len(toDelete), Programs: programs}, nil
}

// subtract は ids から removed に含まれる要素を除いたスライスを返す（順序は ids のまま）。
// 「削除候補のうち、ブレーカー外で既に消えた分を引く」ためだけに使う。
//
// removed が ids の部分集合であることは前提にしない（cap を len(ids) にしてある）。
// 現状の呼び出しでは DELETE ... RETURNING の結果なので必ず部分集合だが、
// 前提が落ちたときに負の cap で panic するより多めに確保して壊れないほうがよい。
func subtract(ids, removed []int64) []int64 {
	if len(removed) == 0 {
		return ids
	}
	removedSet := make(map[int64]struct{}, len(removed))
	for _, id := range removed {
		removedSet[id] = struct{}{}
	}
	rest := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := removedSet[id]; ok {
			continue
		}
		rest = append(rest, id)
	}
	return rest
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

// retractGraceProtectedSubset は candidates（derivedDeletes --- released を
// 引いた後の、ブレーカー対象の削除候補）のうち、猶予（ruler.retract_grace,
// issue #428）でこのパスでは削除を見送るべき programId を返す。呼び出し側
// （runPassForSite）が r.cfg.RetractGrace > 0 のときだけ、released を引いた
// 後に呼ぶ --- toDelete 全体に適用すると、ユーザーの明示操作（intent skip 等）
// まで猶予が呑み込んでしまう（runPassForSite のコメント参照）。
//
// 対象は「(1) 前パスでルールが base を供給していた（reservations.rule_id が
// NOT NULL）、(2) その番組の epg_programs.start_at（射影の最新値。猶予の正しさ
// を program_snapshots の同期範囲や実行順序に結合させたくないので直接見る）が
// [now, now+grace) の範囲、(3) そのルールが今も enabled」の 3 条件をすべて
// 満たす行。SQL 側
// （ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs、
// internal/db/queries/ruler.sql）が権威。
func (r *Ruler) retractGraceProtectedSubset(ctx context.Context, q *sqlcgen.Queries, site string, candidates []int64) ([]int64, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	now := time.Now()
	return q.ListRetractGraceProtectedProgramIDsBySiteAndProgramIDs(ctx, sqlcgen.ListRetractGraceProtectedProgramIDsBySiteAndProgramIDsParams{
		Site:       site,
		ProgramIds: candidates,
		Now:        now,
		GraceUntil: now.Add(r.cfg.RetractGrace),
	})
}

// computeBase は勝者ルールから reservations.base（jsonb）を組む。
//
// フィールドは docs/schema.md §8「予約オプション」のうち、勝者ルールから決まる部分
// （priority / encodeProfiles / keepOriginal / filenameTemplate）に限る。
// **contentPath（展開済みのフルパス）は含めない** — reconciler は再作成時に
// observed（mirakc に登録済みの schedule）の contentPath を引き継いで固定する
// （`reservations.base` には書き戻さない。docs/recording/reconciler.md
// §「予約オプションの差分反映」）。ruler が base に contentPath を持たせて毎パス
// 書き直すと、この固定を素通りして EPG 更新のたびに mirakc の schedule が
// 作り直される churn を招く。
//
// filenameTemplate はこれと事情が違う: テンプレート文字列そのものはルール編集
// でしか変わらず、EPG 更新（番組名・時刻のスナップショット変化）では変化しない。
// そのため base に載せても contentPath と同じ churn は起きない —
// 展開（テンプレート文字列 → 実パス）は reconciler の役目のまま変わらない。
// 空文字なら載せない（base を最小に保ち、既定の固定形式利用時に
// IS DISTINCT FROM の差分書き込みが空振りしないため）。
//
// **skip を base に載せる唯一の経路が重複排除（dedupeSkip）である。** ユーザーの
// 「録るな」は program_intents.action が担うフィールドで、base 側で表現すると
// 優先順位の合成に jsonb マージの細工が要る。逆に重複排除は「ルール x 履歴」から
// 毎パス導出される値なので base に載るのが正しく、ユーザーの action='record' が
// これに勝つ合成は db.EffectiveOptions の 1 箇所で解かれる
// （docs/recording.md §4.2「dedup skip（重複排除）」）。
func computeBase(rule sqlcgen.Rule, dedupeSkip bool) (json.RawMessage, error) {
	priority := int(rule.Priority)
	keepOriginal := rule.KeepOriginal
	profiles := slices.Clone(rule.EncodeProfiles)

	opts := db.ReservationOptions{
		Priority:       &priority,
		KeepOriginal:   &keepOriginal,
		EncodeProfiles: &profiles,
	}
	if rule.FilenameTemplate != "" {
		filenameTemplate := rule.FilenameTemplate
		opts.FilenameTemplate = &filenameTemplate
	}
	// マッチしなかったときは skip を**載せない**（false を載せない）。base を最小に
	// 保ち、重複排除を使っていないルールの base が M2-6 前と一致するようにする
	// （差分書き込みが空振りしない）。
	if dedupeSkip {
		skip := true
		opts.Skip = &skip
	}
	out, err := json.Marshal(opts)
	if err != nil {
		return nil, fmt.Errorf("marshalling base options: %w", err)
	}
	return out, nil
}
