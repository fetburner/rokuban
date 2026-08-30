package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/ptr"
)

const (
	epgQueue = "epg"

	// epgBatchSize は 1 回の pgx.Batch に詰める行数。全量で数万〜十万行になるため
	// 分割してメモリと 1 バッチあたりの所要を抑える。
	epgBatchSize = 1000

	// epgNotifyTopic は SSE クライアントへ配る番組表更新のトピック名。
	epgNotifyTopic = "epg"

	// epgSyncTimeout は全量同期 1 パスの上限。
	epgSyncTimeout = 10 * time.Minute
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
//
// 同一サイトの全量同期が重ならないよう ByArgs で一意化するが、ByState は
// 「まだ終わっていない状態」だけに絞る。River の既定（UniqueOptsByStateDefault）は
// completed を含むため、既定のままだと一度成功した時点で以降の定期投入がすべて
// 重複として捨てられ、10 分間隔の定期ジョブが実質ワンショットになる。
//
// Queue は a.Site で修飾する（physicalQueueName、issue #185 M4-13。必ず
// physicalQueueName を経由する --- qualifyQueueName のコメント参照）。
// tuner_sync も同じ epg キューを共有しているので、TunerSyncArgs.InsertOpts も
// 同じ規則で修飾する（片方だけ修飾すると MaxWorkers: 1 による同時実行の抑制が
// site 単位に分かれて崩れる）。
//
// ByQueue: uniqueByQueue の理由は pendingJobStates 直後の doc コメント参照。
func (a EpgSyncArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: physicalQueueName(epgQueue, a.Site),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: uniqueByQueue,
			ByState: pendingJobStates,
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

	// RetentionGrace は end_at がこの時間より前の番組を刈り取る猶予
	// （config.epg.retention_grace。config.defaults() が既定値 24 時間を
	// 埋めるので、ここでは常に config が渡した値をそのまま使う）。
	RetentionGrace time.Duration

	// Site はこのワーカープロセス自身の site（`--sites` で束縛された site）。Work は
	// これと job.Args.Site を verifySite で照合してから mirakc に触る
	// （issue #139）。空なら db.DefaultSite に解決する（verifySite 参照）。
	Site string
}

// Timeout は River の既定（1 分）より長い上限を与える。
//
// 全量同期の所要は番組数に比例する。GR のみ 7139 件で 1.7 秒なので既定でも足りるが、
// BS/CS を含めて十万件規模になると既定を超えうる。一方 ingest と違って無制限に
// したくはない（mirakc が応答しないまま掴み続けるのを避ける）ので、上限は置く。
func (w *EpgSyncWorker) Timeout(*river.Job[EpgSyncArgs]) time.Duration {
	return epgSyncTimeout
}

// Work は EPG の全量同期を 1 パス実行する。
// services / programs を upsert し、今回観測しなかった行と放送済み番組を削除する。
func (w *EpgSyncWorker) Work(ctx context.Context, job *river.Job[EpgSyncArgs]) error {
	site := job.Args.Site
	log := slog.With("site", site)

	started := time.Now()
	defer func() { metrics.EpgSyncDuration.Observe(time.Since(started).Seconds()) }()

	// mirakc インスタンスはサイトスコープ。他サイトのジョブをこのプロセスの
	// mirakc に投げると、別インスタンスの EPG をこのサイトの投影として書きうる
	// （issue #139）。ListServices/ListPrograms より前に照合する。
	if err := verifySite(w.Site, site, epgQueue); err != nil {
		return err
	}

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
	serviceChannels := serviceChannelIndex(services)
	syncedPrograms, observedChannels, err := w.syncPrograms(ctx, q, site, programs, serviceChannels)
	if err != nil {
		return fmt.Errorf("syncing programs: %w", err)
	}

	// 空レスポンスでスイープを走らせるとプロジェクションが消える。
	// mirakc は再起動直後に EPG を読み込み終えておらず空を返しうるため、
	// その 1 パスで番組表を消し飛ばさないようスイープを見送る。
	// 削除しなくても次のパスが収束させる（レベルトリガー）。
	// 大量削除の前で立ち止まる規律は reconciler のサーキットブレーカーと同じ（issue #11）。
	var staleServices int64
	if syncedServices == 0 {
		log.Warn("epg sync: mirakc returned no services, skipping service sweep")
	} else if staleServices, err = q.DeleteStaleEpgServices(ctx, sqlcgen.DeleteStaleEpgServicesParams{
		Site:       site,
		ObservedAt: mark,
	}); err != nil {
		return fmt.Errorf("deleting stale services: %w", err)
	}

	// 番組のスイープはチャンネル単位。mirakc の EPG 収集は物理チャンネルごとに
	// チューニングして collect-eits を回すため、1 チャンネルの収集失敗
	// （録画とのチューナー競合・timeout）で番組が返らないことがある。
	// サイト単位でスイープすると、そのチャンネルの番組表だけが消える。
	stalePrograms, err := w.sweepPrograms(ctx, q, site, mark, services, serviceChannels, observedChannels)
	if err != nil {
		return fmt.Errorf("deleting stale programs: %w", err)
	}
	if len(observedChannels) == 0 {
		log.Warn("epg sync: mirakc returned no projectable programs, skipping program sweep")
	} else if skipped := countChannels(serviceChannels) - len(observedChannels); skipped > 0 {
		log.Warn("epg sync: some channels returned no programs, their sweep was skipped",
			"channels_without_programs", skipped)
	}

	// ローリングウィンドウ: mirakc が過去番組を保持し続けても、こちらは刈り取る。
	pruned, err := q.PruneEpgPrograms(ctx, sqlcgen.PruneEpgProgramsParams{
		Site:  site,
		EndAt: mark.Add(-w.RetentionGrace),
	})
	if err != nil {
		return fmt.Errorf("pruning aired programs: %w", err)
	}

	// 番組表が更新されたことをクライアントに知らせる。epg_programs は 1 パスで
	// 数千行を upsert するためトリガーでは通知が細かすぎるので、
	// パス完了時に 1 回だけ明示的に送る（internal/db/queries/notify.sql のコメント参照）。
	if err := q.NotifyTopic(ctx, epgNotifyTopic); err != nil {
		// ヒントの配送失敗は同期の失敗ではない。次のパスか
		// クライアントの staleTime 経過後の再取得で収束する。
		log.Warn("epg sync: notifying clients failed", "err", err)
	}

	// EPG 同期完了は ruler_pass 起動契機のヒントの 1 つ（docs/recording.md §3.1
	// 「EPG 同期の完了」）。EPG が変わったら評価を早める。ここでの投入はルール
	// 書き込みのようなトランザクション整合の要求（dual-write 回避）はなく、
	// あくまで定期パスを前倒しするヒントなので、同一トランザクションである
	// 必要はない。river.ClientFromContextSafely でジョブ実行中の Client を
	// 取り出す（EpgSyncWorker 自身に river.Client を持たせずに済む）。Client が
	// 取れない場合（単体テストで Work を直接呼ぶ等）は投入せず、次の定期パスに委ねる。
	if riverClient, clientErr := river.ClientFromContextSafely[pgx5.Tx](ctx); clientErr == nil {
		if _, insertErr := riverClient.Insert(ctx, RulerPassArgs{Site: site}, nil); insertErr != nil {
			log.Warn("epg sync: inserting ruler_pass hint failed", "err", insertErr)
		}
	}

	metrics.EpgSyncLastSuccess.SetToCurrentTime()
	metrics.EpgProgramsProjected.Set(float64(syncedPrograms))
	metrics.EpgChannelsWithoutPrograms.Set(float64(countChannels(serviceChannels) - len(observedChannels)))

	log.Info("epg sync complete",
		"services_fetched", len(services),
		"services_projected", syncedServices,
		"channels", countChannels(serviceChannels),
		"channels_with_programs", len(observedChannels),
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

// serviceKey は (networkId, serviceId) の組。mirakc の programId や Service.id の
// 符号化に依存せずサービスを一意に指すための鍵。
type serviceKey struct {
	networkID int32
	serviceID int32
}

// channelKey は mirakc の物理チャンネル（type + channel）。
// EPG 収集はこの単位で行われるため、スイープのスコープもこの単位にする。
type channelKey struct {
	channelType string
	channel     string
}

func keyOfService(s mirakc.Service) serviceKey {
	return serviceKey{networkID: int32(s.NetworkID), serviceID: int32(s.ServiceID)}
}

// serviceChannelIndex はサービスから所属チャンネルを引く索引を作る。
func serviceChannelIndex(services []mirakc.Service) map[serviceKey]channelKey {
	idx := make(map[serviceKey]channelKey, len(services))
	for _, s := range services {
		idx[keyOfService(s)] = channelKey{channelType: s.Channel.Type, channel: s.Channel.Channel}
	}
	return idx
}

// countChannels は索引に現れる物理チャンネル数を返す。
func countChannels(idx map[serviceKey]channelKey) int {
	seen := make(map[channelKey]struct{}, len(idx))
	for _, ch := range idx {
		seen[ch] = struct{}{}
	}
	return len(seen)
}

// sweepPrograms は「今回番組を返したチャンネル」に属するサービスの stale 行だけを削除する。
//
// 対象チャンネルのサービスは番組が 0 件のものも含める。マルチ編成が終わって
// サブサービスの番組が無くなったとき、その古い行を消せるようにするため
// （サービス単位でスイープすると、この行がローリングウィンドウまで残ってしまう）。
//
// クエリは network_id ごとに分けて呼ぶ。1 つの TS は 1 つの original_network_id を
// 持つのでチャンネルより粗くならず、可変長の (network_id, service_id) 組を
// SQL に渡す必要がなくなる。
func (w *EpgSyncWorker) sweepPrograms(
	ctx context.Context,
	q *sqlcgen.Queries,
	site string,
	mark time.Time,
	services []mirakc.Service,
	serviceChannels map[serviceKey]channelKey,
	observedChannels map[channelKey]struct{},
) (int64, error) {
	if len(observedChannels) == 0 {
		return 0, nil
	}

	byNetwork := make(map[int32][]int32)
	for _, s := range services {
		key := keyOfService(s)
		if _, ok := observedChannels[serviceChannels[key]]; !ok {
			continue
		}
		byNetwork[key.networkID] = append(byNetwork[key.networkID], key.serviceID)
	}

	networks := make([]int32, 0, len(byNetwork))
	for nid := range byNetwork {
		networks = append(networks, nid)
	}
	slices.Sort(networks)

	var deleted int64
	for _, nid := range networks {
		n, err := q.DeleteStaleEpgProgramsForServices(ctx, sqlcgen.DeleteStaleEpgProgramsForServicesParams{
			Site:       site,
			ObservedAt: mark,
			NetworkID:  nid,
			ServiceIds: byNetwork[nid],
		})
		if err != nil {
			return deleted, fmt.Errorf("network %d: %w", nid, err)
		}
		deleted += n
	}
	return deleted, nil
}

// syncPrograms は番組を upsert し、投影した件数と「番組を投影できたチャンネル」を返す。
//
// params はチャンクごとに組み立てる。全件分をまとめて作ると、パース済みの
// []mirakc.Program と再マーシャルした jsonb ペイロードを同時に抱えることになり、
// 全サービス 8 日分（数万〜十万件 × 数 KB）ではピークメモリが跳ねる。
func (w *EpgSyncWorker) syncPrograms(
	ctx context.Context,
	q *sqlcgen.Queries,
	site string,
	programs []mirakc.Program,
	serviceChannels map[serviceKey]channelKey,
) (int, map[channelKey]struct{}, error) {
	var projected int
	observed := make(map[channelKey]struct{})
	for chunk := range chunks(programs, epgBatchSize) {
		params := make([]sqlcgen.UpsertEpgProgramParams, 0, len(chunk))
		chunkChannels := make(map[channelKey]struct{})
		for _, p := range chunk {
			if !projectable(p) {
				continue
			}
			// 所属チャンネルが分かる番組だけを「そのチャンネルを観測した」根拠にする。
			// 分からない番組（サービス一覧に無いサービス）は投影はするが、
			// スイープ対象を導けないので観測には数えない。
			if ch, known := serviceChannels[serviceKey{networkID: int32(p.NetworkID), serviceID: int32(p.ServiceID)}]; known {
				chunkChannels[ch] = struct{}{}
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
				Name:        ptr.Deref(p.Name),
				Description: ptr.Deref(p.Description),
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
			return projected, observed, err
		}
		projected += len(params)
		// upsert が成功したチャンネルだけを観測済みにする。失敗したチャンネルを
		// 観測済みにすると、そのチャンネルの古い行を消してしまう。
		for ch := range chunkChannels {
			observed[ch] = struct{}{}
		}
	}
	return projected, observed, nil
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
