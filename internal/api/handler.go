package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

var version = "dev"

// Server は予約 API のハンドラ実装。oapi-codegen の StrictServerInterface を満たす。
type Server struct {
	recordingPlayback recordingPlaybackConfig
	ready             func(context.Context) error
	chapterStore      store.ChapterEdits
	cmStationStore    store.CMStation
	cmDetectionStore  store.CMDetection
	playbackStore     store.PlaybackState
	recordingTrash    store.RecordingTrash
	recordingQuery    store.RecordingQuery
	tunerReader       store.TunerReader
	capacityStore     store.Capacity
	reservationStore  store.Reservations
	ruleStore         store.Rules
	labelRules        store.LabelRules
	breakerAdmin      store.BreakerAdmin
	// originalLocator は録画の原本のコピーがある置き場を引く。置き場ごとの URL と、置き場で修飾するキューの選択に使う。
	originalLocator store.OriginalLocator
	// breakerScopes は置き場の範囲の削除ブレーカーを再開するときに引く範囲の名前（FS 置き場と派生物の置き場）。
	// 置き場を指定する再開（/api/stores/{store}/breakers/{name}/resume）は別の作業で入る。
	breakerScopes      []string
	epgReader          store.EPGReader
	programSearch      store.ProgramSearch
	encodePolicy       store.EncodePolicy
	dedupe             store.RulerDedupe
	mediaReader        store.MediaReader
	storageObservation store.StorageObservation
	enqueuer           jobq.Enqueuer

	// sites はこのプロセスが応答してよい mirakc サイト名の集合（メンバーシップ判定用。
	// knownSite 参照）。api は不変条件 1（mirakc にもファイルシステムにも依存しない）
	// によりどの site にも束縛されないので、権威は「config.mirakcs レジストリに
	// 存在するか」であり、1 プロセスがレジストリの全 site を処理できる
	// （issue #184 M4-12）。
	sites map[string]struct{}

	// siteNames は sites の定義順一覧（GET /api/sites の応答、ルールが対象 site を
	// 指定しなかったときの既定の投入先。issue #184）。
	siteNames []string

	// encodeProfiles は config に定義されたエンコードプロファイル名の集合。
	// ルール保存時の encodeProfiles 存在検証に使う（issue #64）。
	// 空（未注入）なら名前検証をスキップする（テストの部分構成を許す）。
	encodeProfiles map[string]struct{}

	// cutProfiles は `cut: true` のプロファイル名の集合。encodeProfiles と同じ
	// く空（未注入）なら「cut の規則を検査しない」。cut だけの選択を拒否する
	// 判定（config.ValidateCutSelection）と、`awaiting_review` の導出に使う。
	cutProfiles map[string]struct{}

	// encodeProfileNames は定義順の名前一覧（GET /api/encode-profiles 用。issue #68）。
	encodeProfileNames []string

	// liveProfiles は定義順のライブプロファイル一覧（GET /api/live-profiles 用。issue #869）。
	liveProfiles []LiveProfileSummary

	// capabilities はこのデプロイで有効なオプション機能（GET /api/capabilities 用。
	// issue #209）。ゼロ値は「すべて無効」で、config の既定（live.enabled: false）と
	// 一致する --- テストの部分構成で注入を省いたときに、無効な機能の導線が出る側に
	// 倒れないようにするため。
	capabilities Capabilities
}

// NewServer は Server を生成する。enqueuer はジョブ投入の契約で、実行エンジンを
// api から隠す。nil ならジョブ投入が必要な操作は投入を省略する（RouterConfig.Enqueuer 参照）。
// sites が空なら store.DefaultSite の 1 要素とみなす（テストの部分構成を許す。既存の
// 「site が空なら store.DefaultSite」規約を集合に持ち上げただけで新しい規約ではない）。
// encodeProfileNames は config.encode.profiles の名前一覧（未知名の 400 判定用。
// nil/空なら検証をスキップする）。一覧 API にも同じ順序で載せる。
// liveProfiles は config.live.profiles の公開用一覧。設定順を保持する。
// caps はこのデプロイで有効なオプション機能（GET /api/capabilities）。ゼロ値
// （すべて無効）は config の既定と一致する。
// cutProfileNames は config.encode.profiles のうち `cut: true` の名前一覧。
// encodeProfileNames と同じく nil なら cut の規則を検査しない（テストの部分構成）。
// cmStationStore は局の CM ロゴ状態の永続化契約。cmDetectionStore は CM 再試行の永続化契約。
// chapterStore / playbackStore / recordingTrash / recordingQuery / tunerReader / capacityStore / reservationStore / ruleStore はそれぞれの永続化契約。
// labelRules は分類ルールの永続化契約。breakerAdmin はサーキットブレーカー一覧と手動再開の永続化契約。
// epgReader は番組表・番組詳細・シリーズ照会の読み取り契約、programSearch は番組検索、dedupe は重複排除の契約。
// encodePolicy はエンコード方針とカット版作り直し要求のトランザクション契約。
// mediaReader はエンコード待ち行列とドロップ統計の読み取り契約、storageObservation はストレージ残量の観測契約。
// ready は /readyz が呼ぶ DB の確認関数で、nil なら /readyz は 503 を返す。
// nil は該当操作を呼ばない部分テストでのみ許容する。
func NewServer(ready func(context.Context) error, chapterStore store.ChapterEdits, cmStationStore store.CMStation, cmDetectionStore store.CMDetection, playbackStore store.PlaybackState, recordingTrash store.RecordingTrash, recordingQuery store.RecordingQuery, tunerReader store.TunerReader, capacityStore store.Capacity, reservationStore store.Reservations, ruleStore store.Rules, labelRules store.LabelRules, breakerAdmin store.BreakerAdmin, epgReader store.EPGReader, programSearch store.ProgramSearch, encodePolicy store.EncodePolicy, dedupe store.RulerDedupe, mediaReader store.MediaReader, storageObservation store.StorageObservation, enqueuer jobq.Enqueuer, sites []string, encodeProfileNames []string, cutProfileNames []string, liveProfiles []LiveProfileSummary, caps Capabilities) *Server {
	siteNames := sites
	if len(siteNames) == 0 {
		siteNames = []string{store.DefaultSite}
	}
	siteSet := make(map[string]struct{}, len(siteNames))
	for _, s := range siteNames {
		siteSet[s] = struct{}{}
	}
	// nil スライス = 検証オフ（テストの部分構成）。空スライス = 定義ゼロで全名が未知。
	// 本番の server.go は ProfileNames()（常に non-nil）を渡すので検証が常に効く。
	var profiles map[string]struct{}
	var names []string
	if encodeProfileNames != nil {
		profiles = make(map[string]struct{}, len(encodeProfileNames))
		names = make([]string, 0, len(encodeProfileNames))
		for _, n := range encodeProfileNames {
			profiles[n] = struct{}{}
			names = append(names, n)
		}
	}
	var cut map[string]struct{}
	if cutProfileNames != nil {
		cut = make(map[string]struct{}, len(cutProfileNames))
		for _, n := range cutProfileNames {
			cut[n] = struct{}{}
		}
	}
	return &Server{
		ready: ready, chapterStore: chapterStore, cmStationStore: cmStationStore, cmDetectionStore: cmDetectionStore, playbackStore: playbackStore, recordingTrash: recordingTrash, recordingQuery: recordingQuery,
		tunerReader: tunerReader, capacityStore: capacityStore, reservationStore: reservationStore, ruleStore: ruleStore, labelRules: labelRules, breakerAdmin: breakerAdmin, epgReader: epgReader, programSearch: programSearch,
		encodePolicy: encodePolicy, dedupe: dedupe, mediaReader: mediaReader, storageObservation: storageObservation, enqueuer: enqueuer,
		sites: siteSet, siteNames: siteNames,
		encodeProfiles: profiles, encodeProfileNames: names,
		cutProfiles:  cut,
		liveProfiles: append([]LiveProfileSummary(nil), liveProfiles...),
		capabilities: caps,
	}
}

// profileSets はこの Server が持つプロファイル名の集合を、一覧の導出に渡す形で
// 返す（profileSets 型の doc コメント参照）。
func (h *Server) profileSets() profileSets {
	return profileSets{known: h.encodeProfiles, cut: h.cutProfiles}
}

// knownSite は site がこのプロセスの応答対象レジストリに存在するかを返す。
// パスパラメータ {site} の検証はすべてこれを通す（issue #184 M4-12）。
func (h *Server) knownSite(site string) bool {
	_, ok := h.sites[site]
	return ok
}

// Healthz はヘルスチェックエンドポイント。
func (h *Server) Healthz(_ context.Context, _ HealthzRequestObject) (HealthzResponseObject, error) {
	return Healthz200JSONResponse{Status: "ok"}, nil
}

// readyzTimeout は /readyz の DB 確認関数に渡す期限。
//
// **probe の `timeoutSeconds` に依存せず、ハンドラ側で 503 として答えるための上限。**
// probe 側の設定（長い `timeoutSeconds`）や probe 以外の呼び手に対しても、
// 「DB がハングしても有限時間で readiness の失敗を返す」を実装側で保証する。
// ただし ready が ctx の期限で返ることが前提である（RouterConfig.Ready 参照）。
// マニフェスト側は `timeoutSeconds` をこの値より長くしてある
// （`deploy/k8s/base/api.yaml`。同値以下だとこの経路が一度も通らない）。
// 判定は TestReadyz_HangingDBTimesOut。
const readyzTimeout = 2 * time.Second

// Readyz は readiness probe（ロードバランサ向け）。DB の確認が成功すれば 200、
// 失敗すれば 503 を返す。
//
// **確認関数が未設定（nil）なら 503**（fail-closed）。nil を 200 にすると、DB を一度も
// 見ていない Pod が Service の後ろに入る。実バイナリでは cmd/rokuban が Ready に
// pool.Ping を必ず渡すので nil にならず、nil はルータの組み立てを誤ったとき
// （テストの部分構成を含む）だけである。
//
// **`/healthz`（liveness）には依存チェックを入れない**方針は変えない。liveness で
// DB を見ると DB の瞬断で全 Pod が同時に再起動する（docs/operations.md §5
// 「healthz」）。readiness は「今トラフィックを受けられるか」なので逆に DB を見る。
//
// 見るのは DB の応答だけで、mirakc への到達性は見ない（不変条件 1）。
//
// ready が ctx の期限で返るなら、readyzTimeout を過ぎると 503 になる
// （判定は TestReadyz_HangingDBTimesOut）。
func (h *Server) Readyz(ctx context.Context, _ ReadyzRequestObject) (ReadyzResponseObject, error) {
	if h.ready == nil {
		return Readyz503JSONResponse{Status: "database check unavailable"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, readyzTimeout)
	defer cancel()
	if err := h.ready(ctx); err != nil {
		// 詳細（接続先などを含みうる DB driver のエラー文）は応答に載せず、
		// ログにだけ出す。probe の応答は誰でも引けるため。
		slog.WarnContext(ctx, "readyz: database check failed", "error", err)
		return Readyz503JSONResponse{Status: "database unavailable"}, nil
	}
	return Readyz200JSONResponse{Status: "ok"}, nil
}

// GetVersion はサーバーバージョンを返す。
func (h *Server) GetVersion(_ context.Context, _ GetVersionRequestObject) (GetVersionResponseObject, error) {
	return GetVersion200JSONResponse{Version: version}, nil
}

// ListReservations は予約一覧を返す。導出行の読み取り専用ビュー（issue #29）。
// 全サイトの予約を返す（issue #184 M4-12）。各要素の Site で区別する。
func (h *Server) ListReservations(ctx context.Context, _ ListReservationsRequestObject) (ListReservationsResponseObject, error) {
	rows, err := h.reservationStore.ListReservations(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]Reservation, 0, len(rows))
	for _, row := range rows {
		res, err := reservationFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, res)
	}
	return ListReservations200JSONResponse(result), nil
}

// reservationState は Reservation.state を (rule_id, base, neverRecorded) から
// 導出する。
//
// state 列そのものは Phase 1（#27/#28/#30）で reservations から落とされた ---
// active/detached は (rule_id, base) から導出できる値として列を持たないことに
// した。orphaned は一度 orphaned_at（reconciler だけが書く不可逆な観測）を
// 経たが、issue #98 で orphaned_at 自体も廃止され、「この放送イベントに
// never_scheduled_events の行が存在し、かつ recordings の行が存在しない」という
// 判定（neverRecorded。呼び出し元がクエリの never_recorded 列から受け取る）に
// 置き換わった。API レスポンスは 1 バイトも変えないため、ここで毎回計算して
// 返す（CLAUDE.md 不変条件 9「導出値と不可逆な事実を同じ列に載せない」・
// 「導出は読むたびに評価する」）。
//
// ここで毎パス評価し直すことで、ルールの削除（FK の ON DELETE SET NULL が
// rule_id を先に NULL にする経路）でも編集と同じく detached になる
// （#30 症状 1 の修正: 遷移を SQL の CASE で保存する形では拾えなかった）。
func reservationState(ruleID *int64, base json.RawMessage, neverRecorded bool) ReservationState {
	switch {
	case neverRecorded:
		return Orphaned
	case ruleID == nil && len(base) > 0:
		return Detached
	default:
		return Active
	}
}

// reservationFromRow は予約行・番組スナップショット・ユーザーの上書き
// （program_overrides）・ユーザー意図（program_intents.action）から API 表現を
// 組む。snapshot / overrides / intentAction は予約行ではなく別表にあるので、
// まとめて受け取る（#27 で番組の事実のスナップショットが program_snapshots に
// 抽出された）。
//
// source は reservations の列ではなく、intentAction の有無から都度導出する
// （issue #26）。reservations.source 列は「ユーザーが手動で予約したか」（不可逆な
// 事実）と「いまルールが base を供給しているか」（rule_id の有無で変わる導出状態）
// を 1 列に混ぜていたため、手動予約にルールが一度でもマッチすると 'rule' に
// 書き換わり二度と戻らないバグがあった。rule_id や base の有無ではなく
// program_intents だけを判定材料にするのはそのため --- そうしないと「手動予約 +
// ルールがマッチ中」が rule と表示され、同じバグに逆戻りする
// （docs/recording.md §4.4「manual 行にルールがマッチしても昇格は要らない」）。
//
// skip も列ではなく reservation.EffectiveOptions の結果（base + overrides + action）である。
// 壊れた jsonb でエラーを返すのは意図的: skip=false を返して黙って進むと
// 「mirakc に同期されないのに理由が UI から読めない」という一番説明しにくい状態に
// なる（docs/schema.md §3「jsonb の Unmarshal 失敗を握りつぶさない」）。
// dedupMatchRecordingId / dedupSimilarity はその skip の根拠（M2-6）で、
// ruler が毎パス作り直す導出列をそのまま出す。
//
// neverRecorded は呼び出し元のクエリ（GetReservationFullBySiteAndProgramID /
// ListReservationsFull）が EXISTS で計算した「この予約に status='failed' の
// recordings 行があるか」（issue #98。reservationState のコメント参照）。
func reservationFromRow(row store.ReservationDetails) (Reservation, error) {
	r, snap := row.Reservation, row.Snapshot
	source := ReservationSourceRule
	if row.IntentAction != nil && *row.IntentAction == reservation.IntentRecord {
		source = ReservationSourceManual
	}
	opts, err := reservation.EffectiveOptions(r.Base, row.Overrides, row.IntentAction)
	if err != nil {
		return Reservation{}, fmt.Errorf("resolving effective options for program %d: %w", r.ProgramID, err)
	}
	res := Reservation{
		Site:      r.Site,
		ProgramId: r.ProgramID,
		Source:    source,
		State:     reservationState(r.RuleID, r.Base, row.NeverRecorded),
		Series:    row.Series,
		// site を返すのは容量超過の判定がサイトごとに独立しているため
		// （docs/data.md §6.5）。クライアントに単一サイト前提の定数を持たせると、
		// 多サイト化のときに「他サイトの不足を自分の不足として出す」形で静かに壊れる。
		Skip:        opts.IsSkipped(),
		Title:       snap.Title,
		ServiceName: snap.ServiceName,
		// program_snapshots 由来の予約時点のスナップショット（issue #440）。
		// ライブ画面が「同じチャンネル種別の予約か」を EPG 経由の programId
		// 突き合わせなしに直接判定できるようにするため（web/src/lib/live-interruption.ts）。
		ChannelType: ReservationChannelType(snap.ChannelType),
		StartAt:     snap.StartAt,
		DurationMs:  snap.DurationMs,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
	if r.RuleID != nil {
		res.RuleId = r.RuleID
	}
	// 2 列は必ず揃って設定/解除される（reservations_dedup_evidence_check）ので、
	// 片方だけ出る形にはならない。
	if r.DedupMatchRecordingID != nil {
		res.DedupMatchRecordingId = r.DedupMatchRecordingID
	}
	if r.DedupSimilarity != nil {
		res.DedupSimilarity = r.DedupSimilarity
	}
	if len(row.Overrides) > 0 && string(row.Overrides) != "{}" {
		var m map[string]interface{}
		if json.Unmarshal(row.Overrides, &m) == nil && len(m) > 0 {
			res.Overrides = &m
		}
	}
	return res, nil
}
