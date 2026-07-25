package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

const (
	epgQueue = "epg"

	// defaultEpgRetentionGrace は放送済み番組を刈り取るまでの猶予。
	defaultEpgRetentionGrace = 24 * time.Hour

	// epgBatchSize は 1 回の pgx.Batch に詰める行数。全量で数万〜十万行になるため
	// 分割してメモリと 1 バッチあたりの所要を抑える。
	epgBatchSize = 1000
)

// validChannelTypes は epg_services.channel_type の CHECK 制約に対応する。
// mirakc が未知の型を返した場合、そのサービスだけ捨てて同期は続行する。
var validChannelTypes = map[string]bool{"GR": true, "BS": true, "CS": true, "SKY": true}

// EpgSyncArgs は EPG 全量同期ジョブの引数。
type EpgSyncArgs struct {
	Site string `json:"site"`
}

// Kind は River ジョブの種別名を返す。
func (EpgSyncArgs) Kind() string { return "epg_sync" }

// InsertOpts は River ジョブの挿入オプションを返す。
// 同一サイトの全量同期が重ならないよう ByArgs で一意化する。
func (EpgSyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: epgQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
		},
	}
}

// EpgSyncWorker は mirakc の services / programs を Postgres に全量投影する River ワーカー。
//
// プロジェクションは使い捨てキャッシュで、真実は常に mirakc 側にある。
// そのため差分同期はせず、毎回の全量ポーリング + スイープでレベルトリガーに収束させる。
type EpgSyncWorker struct {
	river.WorkerDefaults[EpgSyncArgs]
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool

	// RetentionGrace は end_at がこの時間より前の番組を刈り取る猶予。0 なら既定値。
	RetentionGrace time.Duration
}

// Work は EPG の全量同期を 1 パス実行する。
// services / programs を upsert し、今回観測しなかった行と放送済み番組を削除する。
func (w *EpgSyncWorker) Work(ctx context.Context, job *river.Job[EpgSyncArgs]) error {
	site := job.Args.Site
	log := slog.With("site", site)

	q := sqlcgen.New(w.Pool)

	// スイープ基準は upsert より前の DB 時刻。これより古い observed_at が
	// 「今回観測されなかった行」になる。
	mark, err := q.EpgSweepMark(ctx)
	if err != nil {
		return fmt.Errorf("getting sweep mark: %w", err)
	}

	services, err := w.MirakcClient.ListServices(ctx)
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}
	syncedServices, err := w.syncServices(ctx, q, site, services)
	if err != nil {
		return fmt.Errorf("syncing services: %w", err)
	}

	programs, err := w.MirakcClient.ListPrograms(ctx)
	if err != nil {
		return fmt.Errorf("listing programs: %w", err)
	}
	syncedPrograms, err := w.syncPrograms(ctx, q, site, programs)
	if err != nil {
		return fmt.Errorf("syncing programs: %w", err)
	}

	// 空レスポンスでスイープを走らせるとプロジェクション全体が消える。
	// mirakc は再起動直後に EPG を読み込み終えておらず空を返しうるため、
	// その 1 パスで番組表を消し飛ばさないようスイープを見送る。
	// 削除しなくても次のパスが収束させる（レベルトリガー）。
	// 大量削除の前で立ち止まる規律は reconciler のサーキットブレーカーと同じ（issue #11）。
	var staleServices, stalePrograms int64
	if syncedServices == 0 {
		log.Warn("epg sync: mirakc returned no services, skipping service sweep")
	} else if staleServices, err = q.DeleteStaleEpgServices(ctx, sqlcgen.DeleteStaleEpgServicesParams{
		Site:       site,
		ObservedAt: mark,
	}); err != nil {
		return fmt.Errorf("deleting stale services: %w", err)
	}

	if syncedPrograms == 0 {
		log.Warn("epg sync: mirakc returned no projectable programs, skipping program sweep")
	} else if stalePrograms, err = q.DeleteStaleEpgPrograms(ctx, sqlcgen.DeleteStaleEpgProgramsParams{
		Site:       site,
		ObservedAt: mark,
	}); err != nil {
		return fmt.Errorf("deleting stale programs: %w", err)
	}

	// ローリングウィンドウ: mirakc が過去番組を保持し続けても、こちらは刈り取る。
	grace := w.RetentionGrace
	if grace <= 0 {
		grace = defaultEpgRetentionGrace
	}
	pruned, err := q.PruneEpgPrograms(ctx, sqlcgen.PruneEpgProgramsParams{
		Site:  site,
		EndAt: mark.Add(-grace),
	})
	if err != nil {
		return fmt.Errorf("pruning aired programs: %w", err)
	}

	log.Info("epg sync complete",
		"services_fetched", len(services),
		"services_projected", syncedServices,
		"programs_fetched", len(programs),
		"programs_projected", syncedPrograms,
		"stale_services", staleServices,
		"stale_programs", stalePrograms,
		"pruned", pruned,
	)
	return nil
}

// syncServices はサービスを upsert し、投影した件数を返す。
func (w *EpgSyncWorker) syncServices(ctx context.Context, q *sqlcgen.Queries, site string, services []mirakc.Service) (int, error) {
	var projected int
	for chunk := range chunks(services, epgBatchSize) {
		params := make([]sqlcgen.UpsertEpgServiceParams, 0, len(chunk))
		for _, s := range chunk {
			if !validChannelTypes[s.Channel.Type] {
				slog.Warn("epg sync: skipping service with unknown channel type",
					"site", site, "service_id", s.ServiceID, "channel_type", s.Channel.Type)
				continue
			}
			params = append(params, sqlcgen.UpsertEpgServiceParams{
				Site:               site,
				NetworkID:          int32(s.NetworkID),
				ServiceID:          int32(s.ServiceID),
				Type:               int32(s.Type),
				LogoID:             int32(s.LogoID),
				RemoteControlKeyID: int32(s.RemoteControlKeyID),
				Name:               s.Name,
				ChannelType:        s.Channel.Type,
				Channel:            s.Channel.Channel,
				HasLogoData:        s.HasLogoData,
			})
		}
		if len(params) == 0 {
			continue
		}
		if err := execBatch(q.UpsertEpgService(ctx, params)); err != nil {
			return projected, err
		}
		projected += len(params)
	}
	return projected, nil
}

// projectable は番組を EPG プロジェクションに載せるかを判定する（issue #17 の決定）。
//
// 地上波は 1 物理チャンネルが複数サービス（ＮＨＫ総合１/２ 等）に分かれており、
// マルチ編成でないときサブサービス側は「同じ eventId で name が null の影の行」として
// 返ってくる。1 番組が 2〜3 行に重複するので、name を持つ行だけを投影する。
//
// 影の行を落としても録れなくなる番組はない。mirakc の録画は
// `filter-program --sid --eid` でサービス単位に絞るため、影の行を予約しても
// 得られるものは同じ shared グループの親と同じか空である。逆にマルチ編成の実番組
// （サブサービスで独立編成される高校野球等）は name を持つのでここを通る。
//
// EPGStation は relatedItems を見る isMainProgram() と name チェックの二段で
// 同じことをしているが、実データ 7139 件で「name があって shared の main でない番組」が
// 0 件であることを確認したので、name の有無だけで同一の結果になる。
func projectable(p mirakc.Program) bool {
	// startAt がない番組は時間軸に置けない
	if p.StartAt == nil {
		return false
	}
	// 名前のない影の行・リレー枠は画面に描くものがない
	return p.Name != nil && *p.Name != ""
}

// syncPrograms は番組を upsert し、投影した件数を返す。
//
// params はチャンクごとに組み立てる。全件分をまとめて作ると、パース済みの
// []mirakc.Program と再マーシャルした jsonb ペイロードを同時に抱えることになり、
// 全サービス 8 日分（数万〜十万件 × 数 KB）ではピークメモリが跳ねる。
func (w *EpgSyncWorker) syncPrograms(ctx context.Context, q *sqlcgen.Queries, site string, programs []mirakc.Program) (int, error) {
	var projected int
	for chunk := range chunks(programs, epgBatchSize) {
		params := make([]sqlcgen.UpsertEpgProgramParams, 0, len(chunk))
		for _, p := range chunk {
			if !projectable(p) {
				continue
			}
			startAt := p.StartAt.Time()
			var durationMs int64
			if p.Duration != nil {
				durationMs = *p.Duration
			}

			params = append(params, sqlcgen.UpsertEpgProgramParams{
				Site:        site,
				ProgramID:   p.ID,
				NetworkID:   int32(p.NetworkID),
				ServiceID:   int32(p.ServiceID),
				EventID:     int32(p.EventID),
				StartAt:     startAt,
				DurationMs:  durationMs,
				EndAt:       startAt.Add(time.Duration(durationMs) * time.Millisecond),
				IsFree:      p.IsFree,
				Name:        derefStr(p.Name),
				Description: derefStr(p.Description),
				GenreLv1:    genreLv1(p.Genres),
				Extended:    marshalOrNil(p.Extended),
				Genres:      marshalOrNil(p.Genres),
				Video:       marshalOrNil(p.Video),
				Audios:      marshalOrNil(p.Audios),
			})
		}
		if len(params) == 0 {
			continue
		}
		if err := execBatch(q.UpsertEpgProgram(ctx, params)); err != nil {
			return projected, err
		}
		projected += len(params)
	}
	return projected, nil
}

// genreLv1 はジャンル絞り込みのクエリ軸となる lv1 を重複なしで取り出す。
func genreLv1(genres []mirakc.Genre) []int16 {
	if len(genres) == 0 {
		return []int16{}
	}
	seen := make(map[int16]struct{}, len(genres))
	out := make([]int16, 0, len(genres))
	for _, g := range genres {
		lv1 := int16(g.LV1)
		if _, dup := seen[lv1]; dup {
			continue
		}
		seen[lv1] = struct{}{}
		out = append(out, lv1)
	}
	return out
}

// batchResults は sqlc が :batchexec 用に生成する型が満たすインターフェース。
type batchResults interface {
	Exec(f func(int, error))
	Close() error
}

// execBatch はバッチを実行し、最初のエラーを返す。Close は常に呼ぶ。
func execBatch(br batchResults) error {
	var firstErr error
	br.Exec(func(i int, err error) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("batch item %d: %w", i, err)
		}
	})
	if closeErr := br.Close(); closeErr != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing batch: %w", closeErr)
	}
	return firstErr
}

// chunks は s を size 件ずつに分割して yield する。
func chunks[T any](s []T, size int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for start := 0; start < len(s); start += size {
			end := min(start+size, len(s))
			if !yield(s[start:end]) {
				return
			}
		}
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// marshalOrNil は v を jsonb 用にエンコードする。
// nil と空のオブジェクト・配列は「詳細なし」なので SQL NULL にする。
// 型ごとに分岐せずマーシャル結果で判定するのは、jsonb 列を増やしたときに
// 分岐の追加を忘れて空値が NULL にならない事故を防ぐため。
func marshalOrNil(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	switch string(data) {
	case "null", "{}", "[]":
		return nil
	}
	return data
}
