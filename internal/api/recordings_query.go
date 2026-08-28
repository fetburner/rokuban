package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/rulequery"
)

// serviceRef は Service.id を DB の 2 列に分解した組。
type serviceRef struct {
	NetworkID int32
	ServiceID int32
}

// recordingsFilter は GET /api/recordings の絞り込み + キーセットページング軸
// （issue #136）。ゼロ値のフィールドは「絞り込みなし」を表す。
type recordingsFilter struct {
	Trash bool

	Q       string
	QTarget ListRecordingsParamsQTarget // "" は既定 (titleDescription) と同じ扱い

	Genres       []int16
	ChannelTypes []string
	// Sites はサイト絞り込み（軸内 OR）。空なら全サイト。
	Sites []string
	// Services はチャンネル絞り込み（軸内 OR）。`Service.id` を
	// (networkId, serviceId) に分解した組で持つ。**site は含まない** ---
	// site は別軸で、軸間は AND（openapi.yaml の `service` の description）。
	Services []serviceRef

	Status ListRecordingsParamsStatus
	Source ListRecordingsParamsSource
	RuleID *int64

	From *time.Time
	To   *time.Time

	// SortDesc は program_start_at の並び順。true = DESC（既定）, false = ASC。
	// 生成された ListRecordingsParamsOrder の定数（Asc / Desc）は "order" を
	// パースする側（ListRecordings ハンドラ）でだけ扱い、このフィルタ自体は
	// bool に落として持つ（フィールド名 Desc が同名パッケージ定数と紛れるのを
	// 避けるため）。
	SortDesc bool

	Before   *time.Time
	BeforeID *int64

	Limit int
}

// defaultRecordingsLimit / maxRecordingsLimit は GET /api/recordings の
// limit の既定値・上限（openapi.yaml のパラメータ定義と一致させる）。
const (
	defaultRecordingsLimit = 50
	maxRecordingsLimit     = 200
)

// genreLv1Min / genreLv1Max は genre_lv1 のドメイン（rule_genres.genre_lv1 の
// CHECK と同じ、00006_rules.sql / 00034_recordings_search.sql の genre_lv1_of
// 参照）。範囲外の genre クエリパラメータは黙って 0 件に落とさず 400 にする
// （PR #187 レビュー O4）。
const (
	genreLv1Min = 0
	genreLv1Max = 15
)

// recordingsFilterFromParams は ListRecordingsParams（openapi_gen.go の生成型）を
// recordingsFilter に変換する。不正な入力（before/beforeId が片方だけ、limit が
// 範囲外、enum に一致しない qTarget/channelType/status/source/order、
// ドメイン外の genre）はエラーメッセージを返す（空文字なら妥当）。
//
// enum は無視して黙って既定値や 0 件に落とさない（issue #136 の罠「黙って 0 件
// にしない」・400 の本文を捨てない規約、CLAUDE.md）。生成型が持つ Valid() を
// 使う（oapi-codegen が enum ごとに生成する）。ListRecordings ハンドラがこの
// 戻り値をそのまま ListRecordings400JSONResponse.Error に載せる。
func recordingsFilterFromParams(p ListRecordingsParams) (recordingsFilter, string) {
	f := recordingsFilter{
		Trash:    p.Trash != nil && *p.Trash,
		SortDesc: true, // 既定 desc。p.Order を検証した後に確定する
		Limit:    defaultRecordingsLimit,
		RuleID:   p.RuleId,
		From:     p.From,
		To:       p.To,
		Before:   p.Before,
		BeforeID: p.BeforeId,
	}
	if p.Order != nil {
		if !p.Order.Valid() {
			return recordingsFilter{}, fmt.Sprintf("invalid order %q (want asc or desc)", *p.Order)
		}
		f.SortDesc = *p.Order != Asc
	}
	if p.Q != nil {
		f.Q = *p.Q
	}
	if p.QTarget != nil {
		if !p.QTarget.Valid() {
			return recordingsFilter{}, fmt.Sprintf("invalid qTarget %q (want title or titleDescription)", *p.QTarget)
		}
		f.QTarget = *p.QTarget
	}
	if p.Genre != nil {
		for _, g := range *p.Genre {
			if g < genreLv1Min || g > genreLv1Max {
				return recordingsFilter{}, fmt.Sprintf("genre must be between %d and %d, got %d", genreLv1Min, genreLv1Max, g)
			}
		}
		f.Genres = int16SliceFromInts(*p.Genre)
	}
	if p.ChannelType != nil {
		for _, ct := range *p.ChannelType {
			if !ct.Valid() {
				return recordingsFilter{}, fmt.Sprintf("invalid channelType %q (want GR, BS, CS or SKY)", ct)
			}
		}
		f.ChannelTypes = channelTypeStrings(*p.ChannelType)
	}
	if p.Site != nil {
		f.Sites = *p.Site
	}
	if p.Service != nil {
		networkIDs, serviceIDs, msg := splitServiceIDs(*p.Service)
		if msg != "" {
			return recordingsFilter{}, msg
		}
		for i := range networkIDs {
			f.Services = append(f.Services, serviceRef{
				NetworkID: networkIDs[i], ServiceID: serviceIDs[i],
			})
		}
	}
	if p.Status != nil {
		if !p.Status.Valid() {
			return recordingsFilter{}, fmt.Sprintf("invalid status %q", *p.Status)
		}
		f.Status = *p.Status
	}
	if p.Source != nil {
		if !p.Source.Valid() {
			return recordingsFilter{}, fmt.Sprintf("invalid source %q (want rule or manual)", *p.Source)
		}
		f.Source = *p.Source
	}
	if p.Limit != nil {
		f.Limit = *p.Limit
	}

	if (f.Before == nil) != (f.BeforeID == nil) {
		return recordingsFilter{}, "before and beforeId must be given together"
	}
	if f.Limit < 1 || f.Limit > maxRecordingsLimit {
		return recordingsFilter{}, fmt.Sprintf("limit must be between 1 and %d", maxRecordingsLimit)
	}
	return f, ""
}

// int16SliceFromInts は openapi の genre（[]int）を genre_lv1 の smallint[] 引数に
// 変換する。呼び出し側（recordingsFilterFromParams）が範囲を検証済みであること
// が前提。
func int16SliceFromInts(vs []int) []int16 {
	out := make([]int16, len(vs))
	for i, v := range vs {
		out[i] = int16(v)
	}
	return out
}

// channelTypeStrings は ListRecordingsParamsChannelType の列を素の文字列列に変換する。
func channelTypeStrings(vs []ListRecordingsParamsChannelType) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

// recordingsSelectColumns / recordingsAvailableEncodedAssetsSelect /
// recordingsFromJoins は `GET /api/recordings`（buildRecordingsQuery）と
// `GET /api/recordings/{id}`（queryRecordingByID）が共有する SELECT リストと
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
const (
	// recordingsSelectColumns は両クエリ共通の SELECT リスト（末尾カンマ無し）。
	recordingsSelectColumns = `
    r.id, r.site, r.rule_id, r.source, r.service_name, r.channel_type, r.channel,
    r.network_id, r.service_id, r.event_id, r.title, r.description,
    r.program_start_at, r.program_duration_ms, r.status,
    r.started_at, r.ended_at, r.quality_events, r.deleted_at, r.created_at,
    a.size_bytes                        AS original_size_bytes,
    COALESCE(d.packets, 0)::bigint      AS drop_packets,
    COALESCE(d.drops, 0)::bigint        AS drop_drops,
    COALESCE(d.errors, 0)::bigint       AS drop_errors,
    COALESCE(d.scrambled, 0)::bigint    AS drop_scrambled,
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
    -- has_original_asset は上の LEFT JOIN a とは述語が違う（a は
    -- state <> 'deleted' で絞るが、こちらは state を問わない）。**この違いが
    -- 「まだ取り込めていない」と「取り込んだ後に削除した」を分ける唯一の材料**
    -- なので、a.id IS NOT NULL で代用してはならない（issue #211）。
    EXISTS (
        SELECT 1 FROM media_assets o
        WHERE o.recording_id = r.id AND o.kind = 'original'
    ) AS has_original_asset,
    -- has_ingestable_record の述語は **watcher が ingest ジョブを投入する条件と
    -- 同じもの**を見る（internal/watcher/watcher.go の
    -- record.Recording.Status == "finished"）。record_sync.status は mirakc の
    -- recordingStatus そのまま（CHECK 無し。docs/schema/record-sync.md）。
    --
    -- **status で絞らずに行の存在だけを見てはならない。** record_sync 行は
    -- failed / canceled の record にも作られ、Rokuban はこの行を消さない
    -- （本番に DELETE FROM record_sync の経路は無い）ので、絞らないと
    -- 「ingest ジョブが永久に来ない録画」が永久に pending を名乗る
    -- （= UI が来ない未来を断定する。issue #211 と同じ形の誤り）。
    EXISTS (
        SELECT 1 FROM record_sync rs
        WHERE rs.recording_id = r.id AND rs.status = 'finished'
    ) AS has_ingestable_record,
    ip.written_bytes  AS ingest_written_bytes,
    ip.expected_bytes AS ingest_expected_bytes,
    ip.observed_at    AS ingest_observed_at`

	// recordingsAvailableEncodedAssetsSelect はブラウザ再生用の観測列（active な
	// encoded のみ）。先頭にカンマを持つので recordingsSelectColumns の直後に
	// そのまま連結できる。
	//
	// buildRecordingsQuery は trash=true のときこれを連結しない（一覧側の
	// 意図的な省略。docs/api/rest.md「録画一覧」）。queryRecordingByID は常に
	// 連結し、代わりに Go 側でごみ箱の行だけ結果を捨てる（そちらのコメント
	// 参照）--- 「ごみ箱では出さない」という同じ結論に、SQL 側で省くか
	// Go 側で捨てるかという別の手段で辿り着いている。手段が違う理由は
	// 一覧側は trash という絞り込み軸を静的に知っているため SQL 自体を
	// 分岐できるが、単体 GET は行を読むまで trash かどうかが分からないため。
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
                jsonb_build_object('profile', e.profile, 'sizeBytes', e.size_bytes)
                ORDER BY e.profile
            ),
            '[]'::jsonb
        )
        FROM media_assets e
        WHERE e.recording_id = r.id
          AND e.kind = 'encoded'
          AND e.state = 'active'
          AND e.profile IS NOT NULL
    ) AS available_encoded_assets`

	// recordingsFromJoins は両クエリ共通の FROM + JOIN 節。
	recordingsFromJoins = `
FROM recordings r
LEFT JOIN media_assets a
    ON a.recording_id = r.id AND a.kind = 'original' AND a.state <> 'deleted'
LEFT JOIN recording_encode_policy p ON p.recording_id = r.id
LEFT JOIN recording_ingest_progress ip ON ip.recording_id = r.id
LEFT JOIN LATERAL (
    SELECT sum(packets) AS packets, sum(drops) AS drops,
           sum(errors) AS errors, sum(scrambled) AS scrambled
    FROM drop_stats
    WHERE media_asset_id = a.id
) d ON true`
)

// buildRecordingsQuery は GET /api/recordings の絞り込み + キーセットページングを
// 動的 WHERE として組む（internal/rulequery.Compile の arg クロージャ方式に倣う。
// sqlc の静的クエリにしない理由は queryRecordings のコメント参照）。
//
// 射影は ListRecordings / ListTrashRecordings（internal/db/queries/recordings.sql・
// recordings_trash.sql）と同じ列を明示的に並べる（r.* ではなく列名を書くのは、
// この SELECT リストが queryRecordings の Scan 呼び出しの順序をそのまま決める
// ため）。available_encoded_assets は trash のときだけ省く ---
// ListTrashRecordings がそれを意図的に射影しない（ごみ箱では配信 3 クエリが
// deleted_at IS NOT NULL を理由に必ず 404 になるため）のと同じ区別。
//
// **既定は全サイト**（api は不変条件 1 により site に束縛されない）。`?site=`
// はその上の絞り込みで、「束縛」ではない --- 束縛はプロセスがどの mirakc に
// 触れるかの話で、絞り込みは読み出しの述語にすぎない。site 軸を service の
// 識別子に混ぜないのは、混ぜると「あるサイトの録画を全部」がチャンネルの
// 列挙でしか表せず、service だけが他の軸と違う意味論（組の選言）になるため。
func buildRecordingsQuery(f recordingsFilter) (string, []any, error) {
	if (f.Before == nil) != (f.BeforeID == nil) {
		return "", nil, fmt.Errorf("before and beforeId must be given together")
	}

	args := make([]any, 0, 16)
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

	// 基底述語は現行 ListRecordings / ListTrashRecordings と揺れさせない
	// （trash=false: r.deleted_at IS NULL、superseded_at は現行も絞っていないので
	// 新たに絞らない）。
	if f.Trash {
		and("r.deleted_at IS NOT NULL")
		and("r.purged_at IS NULL")
	} else {
		and("r.deleted_at IS NULL")
	}

	// q は条件が実際にあるときだけ節を足す（"$n IS NULL OR ..." 形にしない）。
	// これにより Postgres が最初に立てるプランは常に具体的になり、trgm 式 GIN
	// （recordings_title_trgm / recordings_description_trgm、00034）が
	// 汎用プランに落ちて使われなくなることを避ける（issue #136 の「罠」）。
	//
	// これだけでは不十分（PR #187 レビュー O1）: pgx の既定
	// QueryExecModeCacheStatement は SQL テキストを prepared statement として
	// キャッシュし、Postgres 自身が「同じ statement を 6 回目以降 custom plan
	// でなく generic plan で評価する」（PREPARE/EXECUTE の既定挙動）。同じ
	// フィルタ組み合わせ（= 同じ SQL テキスト）に異なる値の `q` が 6 回以上
	// 来ると、trgm 式 GIN が効かない generic plan に落ちうる
	// （queryRecordings が QueryExecModeExec を強制する理由）。
	if f.Q != "" {
		switch f.QTarget {
		case Title:
			and(rulequery.KeywordClause("r.title", f.Q, false, arg))
		default: // "" または TitleDescription
			and("(" + rulequery.KeywordClause("r.title", f.Q, false, arg) +
				" OR " + rulequery.KeywordClause("r.description", f.Q, false, arg) + ")")
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
		and("r.status = " + arg(string(f.Status)))
		// 通常一覧の status=failed だけは、本物の record に置き換わった擬似
		// failed 行によるホーム警告の偽陽性を防ぐ。無条件一覧・trash 一覧は
		// 履歴を維持するため絞らない（docs/frontend/home.md）。
		if !f.Trash && f.Status == ListRecordingsParamsStatusFailed {
			and("r.superseded_at IS NULL")
		}
	}
	if f.Source != "" {
		and("r.source = " + arg(string(f.Source)))
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

	availableAssetsSelect := ""
	if !f.Trash {
		// ListRecordings（internal/db/queries/recordings.sql）と同じ形。
		// ブラウザ再生用の観測（active な encoded のみ）。trash のときだけ省く
		// 理由は recordingsAvailableEncodedAssetsSelect のコメント参照。
		availableAssetsSelect = recordingsAvailableEncodedAssetsSelect
	}

	limitPlaceholder := arg(f.Limit)

	sql := `
SELECT` + recordingsSelectColumns + availableAssetsSelect + recordingsFromJoins + `
WHERE ` + where.String() + `
ORDER BY r.program_start_at ` + orderDir + `, r.id ` + orderDir + `
LIMIT ` + limitPlaceholder

	return sql, args, nil
}

// queryRecordings は buildRecordingsQuery が組んだ SQL を実行し、
// recordingFromListFields で Recording に写す。
//
// sqlc の静的クエリ（Queries.ListRecordings 等）にしないのは、絞り込み軸ごとに
// `($n IS NULL OR ...)` 形で条件を足すと汎用プランになり、trgm 式 GIN
// （recordings_title_trgm 等、00034）を使わないことがあるため（issue #136 の
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

// queryRecordingByID は GET /api/recordings/{id} の単体取得。
// 一覧（queryRecordings）と同じ射影（recordingsSelectColumns /
// recordingsAvailableEncodedAssetsSelect / recordingsFromJoins、この 2 つの
// クエリの共有元）を使うが、絞り込み軸が id 固定のためキーセットカーソルも
// 動的 WHERE ビルダも要らない --- trgm 式 GIN が問題になる可変な組み合わせが
// 存在しない（queryRecordings のコメント参照）ので、単純な静的クエリで十分。
//
// **trash（`deleted_at IS NOT NULL`）の行も返す。** 一覧の `trash=true` が
// メタデータを 200 で返すのと揃える（メディア配信の 404 契約とは別の判断。
// openapi.yaml の getRecording description 参照）。**purged_at が立った
// tombstone（issue #135）だけは除く** --- ファイルが既に無く、通常一覧・
// ごみ箱一覧のどちらにも現れない行なので、単体 GET だけ見える形にしない。
//
// 見つからなければ (Recording{}, false, nil) を返す。
func queryRecordingByID(ctx context.Context, pool *pgxpool.Pool, id int64, knownProfiles map[string]struct{}) (Recording, bool, error) {
	const sql = `
SELECT` + recordingsSelectColumns + recordingsAvailableEncodedAssetsSelect + recordingsFromJoins + `
WHERE r.id = $1 AND r.purged_at IS NULL`

	var fields recordingListFields
	err := pool.QueryRow(ctx, sql, id).Scan(
		&fields.ID, &fields.Site, &fields.RuleID, &fields.Source, &fields.ServiceName, &fields.ChannelType, &fields.Channel,
		&fields.NetworkID, &fields.ServiceID, &fields.EventID, &fields.Title, &fields.Description,
		&fields.ProgramStartAt, &fields.ProgramDurationMs, &fields.Status,
		&fields.StartedAt, &fields.EndedAt, &fields.QualityEvents, &fields.DeletedAt, &fields.CreatedAt,
		&fields.OriginalSizeBytes,
		&fields.DropPackets, &fields.DropDrops, &fields.DropErrors, &fields.DropScrambled,
		&fields.EncodeProfiles,
		&fields.EncodeAttempts,
		&fields.HasOriginalAsset, &fields.HasIngestableRecord,
		&fields.IngestWrittenBytes, &fields.IngestExpectedBytes, &fields.IngestObservedAt,
		&fields.AvailableEncodedAssets,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Recording{}, false, nil
		}
		return Recording{}, false, fmt.Errorf("querying recording %d: %w", id, err)
	}

	// ごみ箱の行では一覧の trash=true と同じく encodedAssets を出さない
	// （プレイヤーを出さないので揃える必要が無い。openapi.yaml の
	// getRecording description、docs/frontend/recordings.md）。
	if fields.DeletedAt != nil {
		fields.AvailableEncodedAssets = nil
	}

	rec, err := recordingFromListFields(fields, true, knownProfiles)
	if err != nil {
		return Recording{}, false, err
	}
	return rec, true, nil
}

func queryRecordings(ctx context.Context, pool *pgxpool.Pool, f recordingsFilter, knownProfiles map[string]struct{}) ([]Recording, error) {
	sql, args, err := buildRecordingsQuery(f)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, sql, append([]any{pgx.QueryExecModeExec}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("querying recordings: %w", err)
	}
	defer rows.Close()

	result := make([]Recording, 0, f.Limit)
	for rows.Next() {
		var fields recordingListFields
		var scanArgs = []any{
			&fields.ID, &fields.Site, &fields.RuleID, &fields.Source, &fields.ServiceName, &fields.ChannelType, &fields.Channel,
			&fields.NetworkID, &fields.ServiceID, &fields.EventID, &fields.Title, &fields.Description,
			&fields.ProgramStartAt, &fields.ProgramDurationMs, &fields.Status,
			&fields.StartedAt, &fields.EndedAt, &fields.QualityEvents, &fields.DeletedAt, &fields.CreatedAt,
			&fields.OriginalSizeBytes,
			&fields.DropPackets, &fields.DropDrops, &fields.DropErrors, &fields.DropScrambled,
			&fields.EncodeProfiles,
			&fields.EncodeAttempts,
			&fields.HasOriginalAsset, &fields.HasIngestableRecord,
			&fields.IngestWrittenBytes, &fields.IngestExpectedBytes, &fields.IngestObservedAt,
		}
		if !f.Trash {
			scanArgs = append(scanArgs, &fields.AvailableEncodedAssets)
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("scanning recording row: %w", err)
		}
		rec, err := recordingFromListFields(fields, f.Trash, knownProfiles)
		if err != nil {
			return nil, err
		}
		result = append(result, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating recordings: %w", err)
	}
	return result, nil
}
