package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/rulequery"
)

// recordingsFilter は GET /api/recordings の絞り込み + キーセットページング軸
// （issue #136）。ゼロ値のフィールドは「絞り込みなし」を表す。
type recordingsFilter struct {
	Trash bool

	Q       string
	QTarget ListRecordingsParamsQTarget // "" は既定 (titleDescription) と同じ扱い

	Genres       []int16
	ChannelTypes []string
	ServiceIDs   []int32

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

// recordingsFilterFromParams は ListRecordingsParams（openapi_gen.go の生成型）を
// recordingsFilter に変換する。不正な入力（before/beforeId が片方だけ、limit が
// 範囲外）はエラーメッセージを返す（空文字なら妥当）。400 の本文を捨てない規約
// （CLAUDE.md）に従い、ListRecordings ハンドラがこれをそのまま
// ListRecordings400JSONResponse.Error に載せる。
func recordingsFilterFromParams(p ListRecordingsParams) (recordingsFilter, string) {
	f := recordingsFilter{
		Trash:    p.Trash != nil && *p.Trash,
		SortDesc: p.Order == nil || *p.Order != Asc,
		Limit:    defaultRecordingsLimit,
		RuleID:   p.RuleId,
		From:     p.From,
		To:       p.To,
		Before:   p.Before,
		BeforeID: p.BeforeId,
	}
	if p.Q != nil {
		f.Q = *p.Q
	}
	if p.QTarget != nil {
		f.QTarget = *p.QTarget
	}
	if p.Genre != nil {
		f.Genres = int16SliceFromInts(*p.Genre)
	}
	if p.ChannelType != nil {
		f.ChannelTypes = channelTypeStrings(*p.ChannelType)
	}
	f.ServiceIDs = int32Slice(p.ServiceId)
	if p.Status != nil {
		f.Status = *p.Status
	}
	if p.Source != nil {
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
// 変換する。
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

// buildRecordingsQuery は GET /api/recordings の絞り込み + キーセットページングを
// 動的 WHERE として組む（internal/rulequery.Compile の arg クロージャ方式に倣う。
// sqlc の静的クエリにしない理由は queryRecordings のコメント参照）。
//
// 射影は ListRecordings / ListTrashRecordings（internal/db/queries/recordings.sql・
// recordings_trash.sql）と同じ列を明示的に並べる（r.* ではなく列名を書くのは、
// この SELECT リストが queryRecordings の Scan 呼び出しの順序をそのまま決める
// ため）。available_encoded_profiles は trash のときだけ省く ---
// ListTrashRecordings がそれを意図的に射影しない（ごみ箱では配信 3 クエリが
// deleted_at IS NOT NULL を理由に必ず 404 になるため）のと同じ区別。
func buildRecordingsQuery(site string, f recordingsFilter) (string, []any, error) {
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
	// （trash=false: r.site = $1 AND r.deleted_at IS NULL、superseded_at は
	// 現行も絞っていないので新たに絞らない）。
	and("r.site = " + arg(site))
	if f.Trash {
		and("r.deleted_at IS NOT NULL")
		and("r.purged_at IS NULL")
	} else {
		and("r.deleted_at IS NULL")
	}

	// q は条件が実際にあるときだけ節を足す（"$n IS NULL OR ..." 形にしない）。
	// これにより常に具体的なプランになり、trgm 式 GIN
	// （recordings_title_trgm / recordings_description_trgm、00034）が
	// 汎用プランに落ちて使われなくなることを避ける（issue #136 の「罠」）。
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
	if len(f.ServiceIDs) > 0 {
		and("r.service_id = ANY(" + arg(f.ServiceIDs) + ")")
	}
	if f.Status != "" {
		and("r.status = " + arg(string(f.Status)))
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

	availableProfilesSelect := ""
	if !f.Trash {
		// ListRecordings（internal/db/queries/recordings.sql）と同じ形。
		// ブラウザ再生用の観測（active な encoded のみ）。
		availableProfilesSelect = `,
    (
        SELECT coalesce(array_agg(e.profile ORDER BY e.profile), '{}')::text[]
        FROM media_assets e
        WHERE e.recording_id = r.id
          AND e.kind = 'encoded'
          AND e.state = 'active'
          AND e.profile IS NOT NULL
    ) AS available_encoded_profiles`
	}

	limitPlaceholder := arg(f.Limit)

	sql := `
SELECT
    r.id, r.rule_id, r.source, r.service_name, r.channel_type, r.channel,
    r.network_id, r.service_id, r.event_id, r.title, r.description,
    r.program_start_at, r.program_duration_ms, r.status,
    r.started_at, r.ended_at, r.quality_events, r.deleted_at, r.created_at,
    a.size_bytes                        AS original_size_bytes,
    COALESCE(d.packets, 0)::bigint      AS drop_packets,
    COALESCE(d.drops, 0)::bigint        AS drop_drops,
    COALESCE(d.errors, 0)::bigint       AS drop_errors,
    COALESCE(d.scrambled, 0)::bigint    AS drop_scrambled,
    COALESCE(p.encode_profiles, '{}')::text[] AS encode_profiles` + availableProfilesSelect + `
FROM recordings r
LEFT JOIN media_assets a
    ON a.recording_id = r.id AND a.kind = 'original' AND a.state <> 'deleted'
LEFT JOIN recording_encode_policy p ON p.recording_id = r.id
LEFT JOIN LATERAL (
    SELECT sum(packets) AS packets, sum(drops) AS drops,
           sum(errors) AS errors, sum(scrambled) AS scrambled
    FROM drop_stats
    WHERE media_asset_id = a.id
) d ON true
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
func queryRecordings(ctx context.Context, pool *pgxpool.Pool, site string, f recordingsFilter) ([]Recording, error) {
	sql, args, err := buildRecordingsQuery(site, f)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("querying recordings: %w", err)
	}
	defer rows.Close()

	result := make([]Recording, 0, f.Limit)
	for rows.Next() {
		var fields recordingListFields
		var scanArgs = []any{
			&fields.ID, &fields.RuleID, &fields.Source, &fields.ServiceName, &fields.ChannelType, &fields.Channel,
			&fields.NetworkID, &fields.ServiceID, &fields.EventID, &fields.Title, &fields.Description,
			&fields.ProgramStartAt, &fields.ProgramDurationMs, &fields.Status,
			&fields.StartedAt, &fields.EndedAt, &fields.QualityEvents, &fields.DeletedAt, &fields.CreatedAt,
			&fields.OriginalSizeBytes,
			&fields.DropPackets, &fields.DropDrops, &fields.DropErrors, &fields.DropScrambled,
			&fields.EncodeProfiles,
		}
		if !f.Trash {
			scanArgs = append(scanArgs, &fields.AvailableEncodedProfiles)
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("scanning recording row: %w", err)
		}
		rec, err := recordingFromListFields(fields, f.Trash)
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
