package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/store"
)

// recordingsSelectColumns / recordingsAvailableEncodedAssetsSelect /
// recordingsFromJoins は `GET /api/recordings`（buildRecordingsQuery）と
// `GET /api/recordings/{id}`（RecordingQuery.Get）が共有する SELECT リストと
// FROM/JOIN 節。openapi.yaml は単体 GET を「一覧要素と同形」とコミットしている
// ため、この 2 つのクエリの射影は常に一致していなければならない。両方が
// 手書きの文字列で別々に SQL を組んでいた最初の実装では、一方に列を足しても
// もう一方は静かに古い形のまま残り続けても `Scan` 呼び出し自体は（列数が
// 揃っている限り）コンパイルも実行も通ってしまう --- `encode_profiles`
// （issue #159）や `available_encoded_profiles`（issue #133。現在は
// `available_encoded_assets` に改名。issue #236）のように過去に
// 列が増えた表なので、次に一覧側だけ列を足したときに同じ drift が再発しうる
// （issue #232 のレビュー指摘）。共有の定数に切り出すことで、列を足す変更は
// 両方のクエリに自動的に効く。
// recordingStoreParam は一覧・単体取得クエリが置き場名を束縛する位置（buildRecordingsQuery と Get の第 1 引数）。
const recordingStoreParam = "$1"

var (
	// recordingsSelectColumns は両クエリ共通の SELECT リスト（末尾カンマ無し）。
	recordingsSelectColumns = `
    r.id, r.site, r.rule_id, r.source, r.service_name, r.channel_type, r.channel,
    r.network_id, r.service_id, r.event_id, r.title, r.description,
    r.program_start_at, r.program_duration_ms, r.status,
    r.started_at, r.ended_at, r.quality_events, r.deleted_at, r.created_at,
    a.size_bytes                        AS original_size_bytes,
    -- 副問い合わせの a は外側の LEFT JOIN a を隠す。外側は削除途中のコピーも含むので、
    -- active だけを見る共有述語 activePlaybackOriginalSQL（別名 a 前提）には使えない。
    CASE WHEN ` + recordingPlaybackVisibleSQL + ` THEN COALESCE(
        (SELECT COALESCE(a.ingest_generation, 0)::bigint FROM media_assets a
         WHERE a.recording_id = r.id AND ` + activePlaybackOriginalSQL(recordingStoreParam) + `),
        ip.temp_generation
    ) END AS hls_generation,
    -- 計測済みでも drop_stats 行が無ければ 0 を返す。未計測を 0 と混同しない判定は
    -- has_measured_drop_summary で行い、集計値の COALESCE を存在判定に使わない。
    COALESCE(d.packets, 0)::bigint      AS drop_packets,
    COALESCE(d.drops, 0)::bigint        AS drop_drops,
    COALESCE(d.errors, 0)::bigint       AS drop_errors,
    COALESCE(d.scrambled, 0)::bigint    AS drop_scrambled,
    COALESCE(p.keep_original, 'always')::text AS keep_original,
    COALESCE(p.encode_profiles, '{}')::text[] AS encode_profiles,
    -- 完了していないエンコードプロファイルの試行状態（issue #316）。
    -- state そのものを SQL で CASE に潰さず recording_encode_attempts の生の
    -- 行を jsonb_agg で出すのは、ingest の has_original_asset 等と同じ理由
    -- --- 導出（encodeJobStatusesFromFields）を DB なしで単体テストできる
    -- 形に保つため。行が無いプロファイル（queued）は Go 側で
    -- encode_profiles − encoded_profiles − ここの行、として導出する。
    (
        SELECT coalesce(
            jsonb_agg(
                jsonb_build_object('profile', ea.profile, 'state', ea.state)
                ORDER BY ea.profile
            ),
            '[]'::jsonb
        )
        FROM recording_encode_attempts ea
        WHERE ea.recording_id = r.id
    ) AS encode_attempts,
    -- ingest（原本の取り込み）の状態を導出するための素の事実（issue #212）。
    -- state そのものを SQL で CASE に潰さず 3 つの事実として出すのは、
    -- 導出（ingestProgressFromFields）を DB なしで単体テストできる形に
    -- 保つため。
    --
    -- has_original_asset は上の LEFT JOIN a とは述語が違う（a は削除途中のコピーも
    -- 含むが、こちらはコピー状態を問わない）。**この違いが
    -- 「まだ取り込めていない」と「取り込んだ後に削除した」を分ける唯一の材料**
    -- なので、a.id IS NOT NULL で代用してはならない（issue #211）。
    EXISTS (
        SELECT 1 FROM media_assets o
        WHERE o.recording_id = r.id AND o.kind = 'original'
    ) AS has_original_asset,
    -- dropSummary は原本の現在の保存サイズを実際に計測した結果だけを返す。
    -- state を問わず照合するので、計測後に tombstone になった原本の要約は残る。
    -- in-place 更新後に古いサイズの計測記録が残っても、それは一致せず未計測扱い。
    EXISTS (
        SELECT 1
        FROM current_ts_scanned_originals ts_scan
        JOIN media_assets scanned_original
          ON scanned_original.id = ts_scan.media_asset_id
        WHERE scanned_original.recording_id = r.id
    ) AS has_measured_drop_summary,
    -- has_ingestable_record の述語は **watcher が ingest ジョブを投入する条件と
    -- 同じもの**を見る（internal/watcher/watcher.go の
    -- record.Recording.Status == "recording" または "finished"）。record_sync.status は mirakc の
    -- recordingStatus そのまま（CHECK 無し。docs/schema/record-sync.md）。
    --
    -- **status で絞らずに行の存在だけを見てはならない。** record_sync 行は
    -- failed / canceled の record にも作られ、Rokuban はこの行を消さない
    -- （本番に DELETE FROM record_sync の経路は無い）ので、絞らないと
    -- 「ingest ジョブが永久に来ない録画」が永久に pending を名乗る
    -- （= UI が来ない未来を断定する。issue #211 と同じ形の誤り）。
    EXISTS (
        SELECT 1 FROM record_sync rs
        WHERE rs.recording_id = r.id AND rs.status IN ('recording', 'finished')
    ) AS has_ingestable_record,
    -- has_abnormally_ended_record は「record が canceled / failed で終わった」。
    -- **述語は worker の終端条件と一致させる**（internal/worker/ingest.go の
    -- RecordFollowReader の status hook が errIngestRecordEndedAbnormally を返す status の集合）。
    --
    -- この列が要るのは、取り消した録画の recording_ingest_progress 行が
    -- 残りうるからである。worker は cancel / fail を観測したとき進捗行を消して
    -- からジョブを終端するが、その DELETE が失敗してもログだけで続行する
    -- （成功したジョブの後始末を失敗で巻き戻さない既存の判断に揃えている）。
    -- 行が残ると ingest_written_bytes が非 NULL になり、ingestProgressFromFields
    -- は has_ingestable_record より進捗行を先に見るので、二度と取り込まれない
    -- 録画が恒久的に「取り込み中（停滞）」を名乗る。
    --
    -- **has_ingestable_record の否定にしてはならない。** 未知の status では
    -- worker は追従を続ける（RecordFollowReader が未知の status を再試行する）ので、
    -- その間の進捗行は生きた観測である。
    EXISTS (
        SELECT 1 FROM record_sync rs
        WHERE rs.recording_id = r.id AND rs.status IN ('canceled', 'failed')
    ) AS has_abnormally_ended_record,
    ip.written_bytes  AS ingest_written_bytes,
    ip.expected_bytes AS ingest_expected_bytes,
    ip.observed_at    AS ingest_observed_at,
    COALESCE(p.cm_detect, false)::boolean AS cm_detect,
    EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id) AS cm_detected,
    (
        SELECT COALESCE(
            jsonb_agg(jsonb_build_object('startMs', lower(cr.cm_range), 'endMs', upper(cr.cm_range)) ORDER BY lower(cr.cm_range)),
            '[]'::jsonb
        )
        FROM recording_cm_detections d
        CROSS JOIN LATERAL unnest(d.cm_ranges) AS cr(cm_range)
        WHERE d.recording_id = r.id
    ) AS cm_ranges,
    ca.state AS cm_attempt_state,
    ca.stage AS cm_attempt_stage,
    ca.error AS cm_attempt_error,
    -- チャプターの所有とユーザー層（encodedAssets[].cutStale の判定材料）。
    -- **導出（chapters.Derive）は Go 側のまま**で、ここは素の事実だけを射影する
    -- （state そのものを SQL で CASE に潰さない既存の規律と同じ。SQL に
    -- タイムラインの導出を複製すると、チャプターの規則が 2 か所に分かれる）。
    EXISTS (
        SELECT 1 FROM recording_chapter_ownership o WHERE o.recording_id = r.id
    ) AS chapters_owned,
    COALESCE(
        (
            SELECT jsonb_agg(
                jsonb_build_object(
                    'startMs', lower(s.span), 'endMs', upper(s.span),
                    'label', s.label, 'cut', s.cut
                )
                ORDER BY lower(s.span)
            )
            FROM recording_chapter_spans s
            WHERE s.recording_id = r.id
        ),
        '[]'::jsonb
    ) AS chapter_spans,
    -- 実効シリーズ（recordings.series の材料）。**recording_series ビューを唯一の
    -- 定義にする** --- ここで COALESCE(label_rule_hits の値, r.series_key) を
    -- 書き下すと、棚（ListRecordingShelves）と一覧で実効シリーズの規則が 2 箇所に
    -- 分かれる。相関サブクエリなのは、ビューを JOIN すると行が増えうる形
    -- （label_rule_hits は PK なので実際には増えないが、読み手がそれを確かめる
    -- 必要がある）を避けるため。一覧は LIMIT 50 なので 1 行 1 回の PK 引き。
	    (SELECT s.value FROM recording_series s WHERE s.recording_id = r.id) AS series,
	    r.series_key AS series_key,
	    pp.position_ms AS resume_position_ms,
	    pp.updated_at AS resume_position_updated_at,
	    (
	        SELECT max(w.watched_at)
	        FROM recording_watched w
	        JOIN recordings event_recording ON event_recording.id = w.recording_id
	        WHERE event_recording.network_id = r.network_id
	          AND event_recording.service_id = r.service_id
	          AND event_recording.program_start_at = r.program_start_at
	    ) AS watched_at`

	// recordingsAvailableEncodedAssetsSelect はブラウザ再生用の観測列（active な
	// encoded のみ）。先頭にカンマを持つので recordingsSelectColumns の直後に
	// そのまま連結できる。
	//
	// buildRecordingsQuery / RecordingQuery.Get のどちらも常にこれを連結する。
	// 「ごみ箱では出さない」（プレイヤーを出さないので値を揃えても使われない。
	// 3d56f92 の理由。性能実測は無い）は Go 側 1 か所（recordingFromListFields が
	// r.DeletedAt != nil だけを見て AvailableEncodedAssets を落とす）だけで決める。
	//
	// keepRanges は media_asset_cuts.keep_ranges を配列にしたもの（null = cut で
	// ない版）。**`cut` の真偽を別に持たない** --- 凍結した区間の有無がそのまま
	// 「カット版か」であり、2 つの主張を並べると片方だけ古くなる（不変条件 9）。
	// Go 側は api.cutStale の判定にも同じ値を使う（現在のタイムラインから
	// chapters.Derive で導出した keep と突き合わせる）。
	//
	// ユーザーが外した版（removed_encoded_assets）は、削除 reconcile が unlink する
	// までの最大 15 分も active のまま残るが、ここでは出さない。出すと外した直後の
	// 再取得でも版が残って見え、再生中の版から再生元が選び直されない。削除を
	// 確定する述語と同じ view を引くので、最後の版として消されずに残った版は再び出る。
	//
	// jsonb_agg で profile と size_bytes を同じ行に載せる（issue #236 M7-3。
	// プロファイル名の配列 + サイズの並行配列という 2 本の index 揺れやすい
	// 配列にしなかった理由 --- 片方だけ ORDER BY を書き忘れると添字が
	// ずれるが、jsonb_agg は 1 要素が 1 行の全情報を持つのでその種の drift が
	// 構造的に起きない）。size_bytes は media_assets の NOT NULL 列なので
	// active な行がある限り必ず入る。
	recordingsAvailableEncodedAssetsSelect = `,
    (
        SELECT coalesce(
            jsonb_agg(
                jsonb_build_object(
                    'profile', e.profile,
                    'sizeBytes', e.size_bytes,
                    'keepRanges', (
                        SELECT jsonb_agg(
                            jsonb_build_object('startMs', lower(k), 'endMs', upper(k))
                            ORDER BY lower(k)
                        )
                        FROM media_asset_cuts c
                        CROSS JOIN LATERAL unnest(c.keep_ranges) AS k
                        WHERE c.media_asset_id = e.id
                    )
                )
                ORDER BY e.profile
            ),
            '[]'::jsonb
        )
        FROM media_assets e
        WHERE e.recording_id = r.id
          AND e.kind = 'encoded'
          AND EXISTS (
              SELECT 1 FROM active_media_asset_copies(e.id, ` + recordingStoreParam + `::text)
          )
          AND e.profile IS NOT NULL
          AND NOT EXISTS (SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = e.id)
    ) AS available_encoded_assets`

	// recordingsFromJoins は両クエリ共通の FROM + JOIN 節。
	recordingsFromJoins = `
FROM recordings r
LEFT JOIN media_assets a
    ON a.recording_id = r.id AND a.kind = 'original' AND EXISTS (
        SELECT 1 FROM media_asset_copies copy
        WHERE copy.media_asset_id = a.id
          AND ($1::text IS NULL OR copy.store = $1)
          AND copy.state <> 'deleted'
    )
LEFT JOIN recording_encode_policy p ON p.recording_id = r.id
-- 方針変更より前の試行は reconcile が削除するまで残ることがある。
-- 現在の世代の試行だけを view 越しに射影する。
LEFT JOIN current_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN recording_ingest_progress ip ON ip.recording_id = r.id
LEFT JOIN recording_playback_positions pp ON pp.recording_id = r.id
-- 論理資産の kind = 'original' の行を JOIN しても二重計上しないのは、
-- media_assets_recording_id_kind_profile_key（UNIQUE NULLS NOT DISTINCT
-- (recording_id, kind, profile)）が recording_id ごとに original を高々 1 行に
-- 制約しているため。
LEFT JOIN LATERAL (
    SELECT sum(ds.packets) AS packets, sum(ds.drops) AS drops,
           sum(ds.errors) AS errors, sum(ds.scrambled) AS scrambled
    FROM drop_stats ds
    JOIN media_assets da ON da.id = ds.media_asset_id
    WHERE da.recording_id = r.id AND da.kind = 'original'
) d ON true`
)

// buildRecordingsQuery は GET /api/recordings の絞り込み + キーセットページングを
// 動的 WHERE として組む（pgstore の compileRuleQuery と同じ arg クロージャ方式に倣う。
// sqlc の静的クエリにしない理由は RecordingQuery.List のコメント参照）。
//
// 列名を明示する（r.* ではなく列名を書くのは、この SELECT リストが
// RecordingQuery.List の Scan 呼び出しの順序をそのまま決めるため）。
// available_encoded_assets は trash かどうかによらず常に射影する ---
// 「ごみ箱では出さない」という規則は recordingFromListFields（Go 側 1 か所）が
// r.DeletedAt で判定する（recordingsAvailableEncodedAssetsSelect のコメント参照）。
//
// **既定は全サイト**（api は不変条件 1 により site に束縛されない）。`?site=`
// はその上の絞り込みで、「束縛」ではない --- 束縛はプロセスがどの mirakc に
// 触れるかの話で、絞り込みは読み出しの述語にすぎない。site 軸を service の
// 識別子に混ぜないのは、混ぜると「あるサイトの録画を全部」がチャンネルの
// 列挙でしか表せず、service だけが他の軸と違う意味論（組の選言）になるため。
// storeName が空の api-only 設定では、全置き場の active copy を対象にする。
func buildRecordingsQuery(f store.RecordingFilter, storeName string) (string, []any, error) {
	if (f.Before == nil) != (f.BeforeID == nil) {
		return "", nil, fmt.Errorf("before and beforeId must be given together")
	}

	args := []any{mediaStoreNameArg(storeName)}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	var where strings.Builder
	and := func(clause string) {
		if where.Len() == 0 {
			where.WriteString(clause)
			return
		}
		where.WriteString(" AND ")
		where.WriteString(clause)
	}

	// trash の有無で基底述語を切り替える。通常一覧では deleted_at だけを絞り、
	// superseded_at は履歴を保つため絞らない。
	if f.Trash {
		and("r.deleted_at IS NOT NULL")
		and("r.purged_at IS NULL")
	} else {
		and("r.deleted_at IS NULL")
	}

	recordingsFilterWhere(f, and, arg)
	if f.EncodeRecordingIDs != nil {
		and("r.id = ANY(" + arg(*f.EncodeRecordingIDs) + "::bigint[])")
	}
	// 番組ハブ（`?seriesOf=`）。述語は 1 文で
	// `E(r) = (SELECT E(o) FROM recordings o WHERE o.id = $n)`。起点の E を先に
	// 読んでからその値で絞る 2 文にしない --- 間に分類ルールの commit が挟まると、
	// 起点が自分のハブから消える（docs/data/series.md §8「資源同定: 起点は録画 id」）。
	//
	// **`IS NOT DISTINCT FROM` にしない。** 起点の実効シリーズが NULL（自動キーを
	// 導出できず、どのルールも当たらない録画）のとき、NULL 同士を等しいと見なすと
	// 「棚の無い録画を全部集めたハブ」が返る。openapi.yaml が約束しているのは
	// 0 件である。
	//
	// 射影は recording_series ビューを唯一の定義として読む（書き下すと棚・一覧・
	// ハブで実効シリーズの規則が 3 箇所に分かれる）。
	if f.SeriesOf != nil {
		and("(SELECT s.value FROM recording_series s WHERE s.recording_id = r.id) = " +
			"(SELECT s.value FROM recording_series s WHERE s.recording_id = " + arg(*f.SeriesOf) + ")")
		and("r.superseded_at IS NULL")
	}
	if f.ContinueWatching {
		and("pp.recording_id IS NOT NULL")
		and("r.deleted_at IS NULL")
		and("r.purged_at IS NULL")
		and("r.superseded_at IS NULL")
		and("r.status IN ('recording', 'finished')")
		and(`NOT EXISTS (
            SELECT 1
            FROM recordings event_recording
            JOIN recording_watched w ON w.recording_id = event_recording.id
            WHERE event_recording.network_id = r.network_id
              AND event_recording.service_id = r.service_id
              AND event_recording.program_start_at = r.program_start_at
        )`)
	}

	// キーセットは (program_start_at, id) の複合で割る（同一 program_start_at
	// の録画は同時刻開始の別チャンネルで普通に発生するため、単独では
	// ページ跨ぎで重複・欠落が出る。issue #136 の「罠」）。order=asc では
	// 不等号を反転する。
	if f.Before != nil {
		if f.SortDesc {
			and("(r.program_start_at, r.id) < (" + arg(*f.Before) + ", " + arg(*f.BeforeID) + ")")
		} else {
			and("(r.program_start_at, r.id) > (" + arg(*f.Before) + ", " + arg(*f.BeforeID) + ")")
		}
	}

	orderDir := "ASC"
	if f.SortDesc {
		orderDir = "DESC"
	}

	limitPlaceholder := arg(f.Limit)

	orderBy := "r.program_start_at " + orderDir + ", r.id " + orderDir
	if f.ContinueWatching {
		orderBy = "pp.updated_at DESC, r.id DESC"
	}

	sql := `
SELECT` + recordingsSelectColumns + recordingsAvailableEncodedAssetsSelect + recordingsFromJoins + `
WHERE ` + where.String() + `
ORDER BY ` + orderBy + `
LIMIT ` + limitPlaceholder

	return sql, args, nil
}

// recordingsFilterWhere は録画一覧（buildRecordingsQuery）と棚
// （buildRecordingShelvesQuery）が共有する絞り込みの述語を、録画 1 件
// （別名 r）に対して組む。片方だけ直る形にしないため、条件 → WHERE と引数の
// 変換はここ 1 箇所に置く。ページング・並び順・番組ハブ・エンコード状況・
// 続きから見る、は録画一覧だけの軸なので buildRecordingsQuery に残す。
func recordingsFilterWhere(f store.RecordingFilter, and func(string), arg func(any) string) {
	// q は条件が実際にあるときだけ節を足す（"$n IS NULL OR ..." 形にしない）。
	// 判断・pgx の既定 QueryExecModeCacheStatement が SQL テキストを named
	// prepared statement にするため Postgres が 6 回目以降 generic plan に
	// 切り替えうること（実測 0.7ms → 290ms）・RecordingQuery.List が
	// QueryExecModeExec を強制する理由は docs/api/rest.md §動的 WHERE ビルダ
	// （sqlc の静的クエリにしない）にある。
	if f.Q != "" {
		switch f.QTarget {
		case "title":
			and(keywordClause("r.title", f.Q, false, arg))
		default: // "" または "titleDescription"
			and("(" + keywordClause("r.title", f.Q, false, arg) +
				" OR " + keywordClause("r.description", f.Q, false, arg) + ")")
		}
	}
	if len(f.Genres) > 0 {
		and("r.genre_lv1 && " + arg(f.Genres) + "::smallint[]")
	}
	if len(f.ChannelTypes) > 0 {
		and("r.channel_type = ANY(" + arg(f.ChannelTypes) + ")")
	}
	if len(f.Sites) > 0 {
		and("r.site = ANY(" + arg(f.Sites) + ")")
	}
	if len(f.Services) > 0 {
		// 行値の IN。(network_id, service_id) の組で OR する形なので、
		// network_id と service_id を別々に ANY で絞る（＝直積になる）形とは違う。
		pairs := make([]string, len(f.Services))
		for i, service := range f.Services {
			pairs[i] = "(" + arg(service.NetworkID) + ", " + arg(service.ServiceID) + ")"
		}
		and("(r.network_id, r.service_id) IN (" + strings.Join(pairs, ", ") + ")")
	}
	if f.Status != "" {
		and("r.status = " + arg(f.Status))
		// 通常一覧の status=failed だけは、本物の record に置き換わった擬似
		// failed 行によるホーム警告の偽陽性を防ぐ。無条件一覧・trash 一覧は
		// 履歴を維持するため絞らない（docs/frontend/home.md）。
		if !f.Trash && f.Status == "failed" {
			and("r.superseded_at IS NULL")
		}
	}
	if f.Source != "" {
		and("r.source = " + arg(f.Source))
	}
	if f.RuleID != nil {
		and("r.rule_id = " + arg(*f.RuleID))
	}
	if f.From != nil {
		and("r.program_start_at >= " + arg(*f.From))
	}
	if f.To != nil {
		and("r.program_start_at < " + arg(*f.To))
	}
}

// buildRecordingShelvesQuery は棚の集計を組む。棚 1 件 = 実効シリーズの値 1 つ。
// 母集団は生きている録画（`deleted_at IS NULL AND superseded_at IS NULL`）で、
// 録画中・取り込み待ち・失敗も含む。絞り込み（recordingsFilterWhere。録画一覧と
// 共有）はこの母集団の録画 1 件ずつに、グループ化の前に当てる。
//
// 代表は program_start_at の新しい順で先頭の 1 件。value は画面のシリーズ名に使い、
// title は代表録画の生タイトルを補助表示する。キーが過剰併合を隠さないよう、両方返す。
//
// 値が NULL の棚も返す。棚一覧の UI は NULL を表示対象から外すが、API では
// 欠落と「分類されていない」を区別できるように残す。
//
// 値が NULL の棚の行は `GROUP BY value` が 1 つのグループにまとめる（SQL の
// GROUP BY は NULL を等しいものとして扱う）。
//
// 未視聴件数もこの集計で返す。放送イベントは生きている再生可能な（かつ絞り込んだ）
// 録画から束ね、視聴済み印はごみ箱・supersede 済み・別 site を含む全録画から読む。
// **絞り込みを watched_events に当ててはならない。** 当てると、別 site の録画で
// 見た回が `site` で絞ったときに未視聴へ戻る
// （TestListRecordingShelves_FilterKeepsWatchedMarksFromOutsideTheFilter）。
//
// **sqlc の静的クエリ（`sqlc.narg` で全条件を受ける形）にしない。** 計測では速さに
// 差が無かった（下記の (f_static) / (q_static)。prepared statement が汎用プランに
// 切り替わりうる 7 回目以降の中央値で比べた）。決め手は共有である。静的な形では
// 録画一覧（buildRecordingsQuery）と WHERE の組み立てを共有できず、条件を直すと
// 片方だけ直る。`service` の行値 IN も静的な形に載らない。録画一覧と同じく、条件が
// あるときだけ節を足し、QueryExecModeExec で毎回計画させる（RecordingQuery.List のコメント）。
//
// **この形はプランの形に依存する。** 旧母集団（再生できる録画だけ。73,000 行がすべて
// 再生可能）での過去の実測（別の環境、sqlc / pgx の prepared statement 経由）:
//
//   - その形: 141 ms
//   - 代表と件数を別々の CTE に割る: 231 ms（playable をもう 1 度走査する）
//   - playable（recordings × playable_assets × recording_series の CTE）を
//     MATERIALIZED にしない: 617 ms
//
// 617 ms の仕組みは、MATERIALIZED を外すと部分一意索引 recordings_unique_active_event
// が選ばれ、その行数見積もりが 1 になって下流が全部 1 行の計画になり、代表を求める
// ソートが外側の行数ぶん繰り返されること、だった。
//
// 下の live は旧 playable に当たる（recordings を走査する CTE）が、MATERIALIZED にしない。
// 現スキーマ・合成 seed（下記）では 617 ms は再現せず、live を MATERIALIZED にした形は
// 本番形の 1.04〜1.13 倍遅い（3 回）。617 ms の再現条件は未検証なので、再発したら
// EXPLAIN で計画を調べる。絞り込みの WHERE は live の中に置く（集計の前に母集団を削る）。
//
// playable_assets は参照が 1 回なので MATERIALIZED にしない。MATERIALIZED にした形は
// 本番形の 1.02〜1.07 倍遅く（3 回）、結果は一致した。
//
// 実効シリーズは recording_series ビューが唯一の定義で、ここでも JOIN で読む
// （COALESCE(lr.value_key, r.series_key) を書き下すと定義が 2 箇所になる）。
// ビュー経由は書き下しより約 8% 遅かった（旧母集団の形、合成データ 73,000 行・141 棚・
// 分類ルール 50 本で約 223 ms 対 約 206 ms）。
//
// `internal/api/shelves_bench_test.go` は `ROKUBAN_BENCH_DATABASE_URL` がなければ
// スキップし、専用 DB で各形を交互に回して計測する。
// seed は生きている録画 71,000 行を含む全 73,000 行、141 棚、分類ルール 50 本で、
// 放送イベントを複数拠点の録画で作り、視聴済み印と再生状態を混ぜる。
// 各形を 12 ラウンド交互に回し、7 回目以降の中央値を 3 回測った（Apple M3 Max・
// PostgreSQL 16.2）。同じ回の旧母集団・実効シリーズ書き下し形（(o_inline)、229.2〜236.6 ms）
// との比で読む。予算は 141 ms の環境で決めた比 200/141 ≈ 1.42 倍である。
//
//   - 絞り込みなし: 309.5〜318.6 ms（1.31〜1.39 倍）
//   - アニメ × 17 日間（47 棚）: 58.1〜59.0 ms（0.25 倍）。静的な形は 56.3〜59.4 ms
//   - キーワード（1 棚）: 17.8〜19.7 ms（0.08 倍）。静的な形は 16.7〜18.6 ms
//
// 実データでの絶対値は未測定である。本番の playable_count は旧形の recording_count と
// 全棚で一致し、絞り込みありの形は Go で別に集計した期待値と一致する（ハーネスが検査する）。
func buildRecordingShelvesQuery(f store.RecordingFilter, storeName string) (string, []any) {
	args := []any{mediaStoreNameArg(storeName)}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	var where strings.Builder
	and := func(clause string) {
		where.WriteString("\n      AND ")
		where.WriteString(clause)
	}
	recordingsFilterWhere(f, and, arg)

	return `
WITH playable_assets AS (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    -- kind ごとに EXISTS を分ける。1 つの EXISTS にまとめると semi join になり、棚クエリが
    -- 分けた形より遅い（コピー 6 万行で中央値 193 ms 対 181 ms。generic plan）。
    WHERE (ma.kind = 'original' AND EXISTS (SELECT 1 FROM active_media_asset_copies(ma.id, $1::text)))
       OR (ma.kind = 'encoded' AND EXISTS (SELECT 1 FROM active_media_asset_copies(ma.id, $1::text)))
),
watched_events AS MATERIALIZED (
    -- 印を束ねる側は live / playable / 絞り込みを当てない。ごみ箱・supersede 済みの印も読む。
    SELECT DISTINCT r.network_id, r.service_id, r.program_start_at
    FROM recordings r
    JOIN recording_watched w ON w.recording_id = r.id
),
live AS (
    SELECT r.id,
           r.title,
           r.program_start_at,
           r.network_id,
           r.service_id,
           rs.value,
           pa.recording_id AS playable_recording_id,
           we.network_id AS watched_network_id
    FROM recordings r
    LEFT JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    LEFT JOIN watched_events we
      ON we.network_id = r.network_id
     AND we.service_id = r.service_id
     AND we.program_start_at = r.program_start_at
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL` + where.String() + `
)
SELECT l.value,
       (array_agg(l.title ORDER BY l.program_start_at DESC, l.id DESC))[1]::text AS title,
       count(*) AS recording_count,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       (count(DISTINCT (l.network_id, l.service_id, l.program_start_at))
           FILTER (WHERE l.playable_recording_id IS NOT NULL AND l.watched_network_id IS NULL))::bigint AS unwatched_count,
       max(l.program_start_at)::timestamptz AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST`, args
}

// List は buildRecordingsQuery が組んだ SQL を実行し、store.RecordingQueryRow として返す。
//
// sqlc の静的クエリにしないのは、絞り込み軸ごとに
// `($n IS NULL OR ...)` 形で条件を足すと汎用プランになり、trgm 式 GIN
// （recordings_title_trgm 等）を使わないことがあるため（issue #136 の
// 「罠」）。buildRecordingsQuery は条件が実際にあるときだけ節を足すので、そもそも
// 1 つの静的クエリ文字列に収まらない。
//
// pgx.QueryExecModeExec を明示するのは、pgx の既定 QueryExecModeCacheStatement
// が SQL テキストごとに named prepared statement を作ってキャッシュし、
// Postgres 自身がその named statement を 6 回目以降 custom plan（実際の bind
// 値に基づく見積り）から generic plan（bind 値を見ない既定選択率での見積り）に
// 切り替えるため（PostgreSQL の PREPARE のプラン選択規則）。generic plan では
// bind 値が分からず trgm 式 GIN を選べないため、既定選択率のまま非選択的な
// インデックス（recordings_program_start_at_id_idx 等）を全走査するプランに
// 倒れる。加えて、この経路は絞り込みの組み合わせごとに SQL テキスト自体が
// 変わるので、そもそも named statement のキャッシュが効く場面が少ない
// （キャッシュを維持するコストに対して利益が薄い）。QueryExecModeExec は
// unnamed statement で毎回明示的に再計画させるので、この切り替えが原理的に
// 起こらない（PR #187 レビュー O1）。
//
// 80,001 行・同一接続で 12 回実行して実測した結果、6 回目で崖が来ることを
// 確認済み（named statement 数はログの `named stmts` 列。0 のままなら
// QueryExecModeExec が効いている）:
//
//	QueryExecModeExec   per-exec: [1.8ms 0.7 0.7 0.7 0.7 0.6 0.7 0.6 0.7 0.7 0.7 0.7]   named stmts=0
//	default(cache_stmt) per-exec: [5.0ms 3.8 3.8 3.3 3.2 | 323.9ms 286.8 295.6 288.1 289.7 285.9 287.6]  named stmts=1
//
// 0.7ms → 290ms（約 400 倍）の崖。この保護を「保険」として外すと、絞り込みの
// 組み合わせが少ない環境（同じ SQL テキストが 6 回を超えて再利用される）で
// この崖に落ちる。

func (s *Recordings) List(ctx context.Context, filter store.RecordingFilter) ([]store.RecordingQueryRow, error) {
	sql, args, err := buildRecordingsQuery(filter, s.storeName)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, sql, append([]any{pgx.QueryExecModeExec}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("querying recordings: %w", err)
	}
	defer rows.Close()

	result := make([]store.RecordingQueryRow, 0, filter.Limit)
	for rows.Next() {
		fields, err := scanRecordingQueryRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning recording row: %w", err)
		}
		result = append(result, fields)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recordings: %w", err)
	}
	return result, nil
}

// Get は録画の素の射影を返す。ごみ箱行も含み、purged の tombstone は返さない。
func (s *Recordings) Get(ctx context.Context, recordingID int64) (store.RecordingQueryRow, error) {
	sql := `
SELECT` + recordingsSelectColumns + recordingsAvailableEncodedAssetsSelect + recordingsFromJoins + `
WHERE r.id = $2 AND r.purged_at IS NULL`

	fields, err := scanRecordingQueryRow(s.pool.QueryRow(ctx, sql, mediaStoreNameArg(s.storeName), recordingID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.RecordingQueryRow{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return store.RecordingQueryRow{}, fmt.Errorf("querying recording %d: %w", recordingID, err)
	}
	return fields, nil
}

// Shelves は実効シリーズごとの棚集計を返す。
func (s *Recordings) Shelves(ctx context.Context, filter store.RecordingFilter) ([]store.RecordingShelf, error) {
	sql, args := buildRecordingShelvesQuery(filter, s.storeName)
	rows, err := s.pool.Query(ctx, sql, append([]any{pgx.QueryExecModeExec}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("listing recording shelves: %w", err)
	}
	defer rows.Close()

	result := make([]store.RecordingShelf, 0)
	for rows.Next() {
		var shelf store.RecordingShelf
		if err := rows.Scan(&shelf.Value, &shelf.Title, &shelf.Count, &shelf.PlayableCount,
			&shelf.UnwatchedCount, &shelf.LatestStartAt, &shelf.RepresentativeID); err != nil {
			return nil, fmt.Errorf("scanning recording shelf: %w", err)
		}
		result = append(result, shelf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recording shelves: %w", err)
	}
	return result, nil
}

type recordingRowScanner interface {
	Scan(dest ...any) error
}

func scanRecordingQueryRow(row recordingRowScanner) (store.RecordingQueryRow, error) {
	var fields store.RecordingQueryRow
	err := row.Scan(
		&fields.ID, &fields.Site, &fields.RuleID, &fields.Source, &fields.ServiceName, &fields.ChannelType, &fields.Channel,
		&fields.NetworkID, &fields.ServiceID, &fields.EventID, &fields.Title, &fields.Description,
		&fields.ProgramStartAt, &fields.ProgramDurationMs, &fields.Status,
		&fields.StartedAt, &fields.EndedAt, &fields.QualityEvents, &fields.DeletedAt, &fields.CreatedAt,
		&fields.OriginalSizeBytes, &fields.HLSGeneration,
		&fields.DropPackets, &fields.DropDrops, &fields.DropErrors, &fields.DropScrambled,
		&fields.KeepOriginal,
		&fields.EncodeProfiles,
		&fields.EncodeAttempts,
		&fields.HasOriginalAsset, &fields.HasMeasuredDropSummary,
		&fields.HasIngestableRecord, &fields.HasAbnormallyEndedRecord,
		&fields.IngestWrittenBytes, &fields.IngestExpectedBytes, &fields.IngestObservedAt,
		&fields.CMDetect, &fields.CMDetected, &fields.CMRanges, &fields.CMAttemptState,
		&fields.CMAttemptStage, &fields.CMAttemptError,
		&fields.ChaptersOwned, &fields.ChapterSpans,
		&fields.Series,
		&fields.SeriesKey,
		&fields.ResumePositionMs,
		&fields.ResumePositionUpdatedAt,
		&fields.WatchedAt,
		&fields.AvailableEncodedAssets,
	)
	return fields, err
}

var _ store.RecordingQuery = (*Recordings)(nil)
