package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"runtime"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/mysqldb"
	"github.com/fetburner/rokuban/internal/notifier"
	"github.com/fetburner/rokuban/internal/notifier/mysqlsource"
	"github.com/fetburner/rokuban/internal/notifier/pgsource"
	"github.com/fetburner/rokuban/internal/role"
	"github.com/fetburner/rokuban/internal/role/mysqllocker"
	"github.com/fetburner/rokuban/internal/role/pglocker"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/mysqlstore"
	"github.com/fetburner/rokuban/internal/store/pgstore"
)

// dbHandle は起動時に開いた DB の接続 1 本（プール）と、その方言の store の組み立てを持つ。
//
// 方言の分岐はこの型のメソッドの中だけに置く。利用側は store の契約（interface）だけを受け取り、
// 方言を知らない。メソッドが返す型は、その組み立てを使う利用側が必要とする契約の和である。
// 同じ構築子を呼ぶ利用側でも必要な契約が違うので、構築子ごとに和を分ける。
type dbHandle struct {
	pg *pgxpool.Pool
	my *sql.DB
}

// openDB は cfg.DB の方言で接続を開く。roles・numSites・numStores は db.ConnBudget と同じ意味で、
// 0 と nil を渡すと単発 CLI と同じ既定のプールになる。
func openDB(ctx context.Context, cfg *config.Config, roles []string, numSites, numStores int) (*dbHandle, error) {
	if cfg.DB.Dialect != config.DBDialectMySQL {
		pool, err := db.NewPool(ctx, cfg.DB, roles, numSites, numStores)
		if err != nil {
			return nil, err
		}
		return &dbHandle{pg: pool}, nil
	}
	budget, err := db.ConnBudget(cfg.DB, roles, numSites, numStores)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(cfg.DB.Host, strconv.Itoa(cfg.DB.Port))
	pool, err := mysqldb.OpenPool(ctx, mysqldb.DSN(addr, cfg.DB.User, cfg.DB.Password, cfg.DB.Database))
	if err != nil {
		return nil, err
	}
	// 上限が 0 の単発 CLI は、PostgreSQL 側と同じ既定（max(4, NumCPU)）に合わせる。
	limit := int(budget)
	if limit == 0 {
		limit = max(4, runtime.NumCPU())
	}
	pool.SetMaxOpenConns(limit)
	// database/sql の既定の空き接続は 2 本で、上限に達した後の再接続が続く。pgxpool が上限まで
	// 空き接続を保つのに合わせ、上限と同じ本数まで空きに残す。
	pool.SetMaxIdleConns(limit)
	return &dbHandle{my: pool}, nil
}

// Close はプールを閉じる。MySQL の閉じる失敗は、終了の経路では呼び出し側が返せないのでログに出す。
func (h *dbHandle) Close() {
	if h.my != nil {
		if err := h.my.Close(); err != nil {
			slog.Error("closing MySQL pool", "err", err)
		}
		return
	}
	h.pg.Close()
}

// Ping は DB への疎通を確かめる（/readyz が使う）。
func (h *dbHandle) Ping(ctx context.Context) error {
	if h.my != nil {
		return h.my.PingContext(ctx)
	}
	return h.pg.Ping(ctx)
}

// pgxPool は PostgreSQL のプールを返す。PostgreSQL 専用の経路（River の adapter）だけが使う。
// MySQL 構成では nil を返す（River は MySQL 構成で起動しない。config の検査を参照）。
func (h *dbHandle) pgxPool() *pgxpool.Pool { return h.pg }

// mediaStores は録画メディアの契約の和。原本・派生物・転送・削除・観測の利用側がそれぞれ
// 必要な契約だけを受け取る。
type mediaStores interface {
	store.MediaReader
	store.MediaServing
	store.OriginalLocator
	store.EncodePolicy
	store.MediaReconcile
	store.IngestPrecheck
	store.MediaDelete
	store.TSScan
	store.StorageObservation
	store.StorageTransfer
	store.TransferBacklogReader
	store.StorageSyncReader
	store.PendingCopyReclaim
	store.DerivedMediaPublisher
	HasStoreCopies(ctx context.Context) (bool, error)
	AdoptUnassignedStoreRows(ctx context.Context) error
	store.MediaPublish
	store.EncodeAttempts
	store.EncodeProgressReporter
	store.IngestStoreChooser
	store.IngestWriterStateReader
	RequireAdoptedStoreRows(ctx context.Context) error
}

// MediaUnbound は置き場にも方針にも束ねない Media を返す（起動時の検査・API・collector）。
func (h *dbHandle) MediaUnbound() mediaStores {
	return h.MediaUnboundWithPolicy(store.StoragePolicy{})
}

// MediaUnboundWithPolicy は置き場で絞らず、方針で判定する Media を返す。
func (h *dbHandle) MediaUnboundWithPolicy(policy store.StoragePolicy) mediaStores {
	if h.my != nil {
		return mysqlstore.NewMediaUnboundWithPolicy(h.my, policy)
	}
	return pgstore.NewMediaUnboundWithPolicy(h.pg, policy)
}

// MediaInStore は storeName の置き場に束縛し、方針で削除を判定する Media を返す。
func (h *dbHandle) MediaInStore(storeName string, policy store.StoragePolicy) mediaStores {
	if h.my != nil {
		return mysqlstore.NewMediaInStore(h.my, storeName, policy)
	}
	return pgstore.NewMediaInStore(h.pg, storeName, policy)
}

// MediaInStoreWithoutPolicy は storeName の置き場に束縛し、方針を持たない Media を返す。
func (h *dbHandle) MediaInStoreWithoutPolicy(storeName string) mediaStores {
	if h.my != nil {
		return mysqlstore.NewMediaInStore(h.my, storeName, store.StoragePolicy{})
	}
	return pgstore.NewMediaInStoreWithoutPolicy(h.pg, storeName)
}

// MediaForDerivedStore は派生物を derivedStore に書き、原本は originalStore から読む Media を返す。
func (h *dbHandle) MediaForDerivedStore(derivedStore, originalStore string, policy store.StoragePolicy) mediaStores {
	if h.my != nil {
		return mysqlstore.NewMediaForDerivedStore(h.my, derivedStore, originalStore, policy)
	}
	return pgstore.NewMediaForDerivedStore(h.pg, derivedStore, originalStore, policy)
}

// MediaForStreamer は配信だけを行う Media を返す（読める置き場の一覧に絞る）。
func (h *dbHandle) MediaForStreamer(policy store.StoragePolicy, readableStores []string) mediaStores {
	if h.my != nil {
		return mysqlstore.NewMediaForStreamer(h.my, policy, readableStores)
	}
	return pgstore.NewMediaForStreamer(h.pg, policy, readableStores)
}

// recordingWrites は録画の観測・論理削除・購読の契約の和。
type recordingWrites interface {
	store.RecordingTrash
	store.RecordObservations
	store.RecordBacklog
	store.RecordingPurge
	store.IngestTarget
}

// RecordingWrites は録画の書き込み側の契約を返す。
func (h *dbHandle) RecordingWrites() recordingWrites {
	if h.my != nil {
		return mysqlstore.NewRecordings(h.my)
	}
	return pgstore.NewRecordings(h.pg, "")
}

// RecordingQueries は storeName の置き場を見る録画の読み取りを返す（空なら全置き場）。
func (h *dbHandle) RecordingQueries(storeName string) store.RecordingQuery {
	if h.my != nil {
		return mysqlstore.NewRecordingQueries(h.my, storeName)
	}
	return pgstore.NewRecordings(h.pg, storeName)
}

// Reservations は予約の契約の和。LoopMarkers は含めない（LoopMarkers を参照）。
type reservations interface {
	store.Reservations
	store.RulerDedupe
	store.RulerInputBumper
	store.RulerPass
}

// Reservations は予約の契約を返す。
func (h *dbHandle) Reservations() reservations {
	if h.my != nil {
		return mysqlstore.NewReservations(h.my)
	}
	return pgstore.NewReservations(h.pg)
}

// LoopMarkers はループの成功時刻の契約を返す。
func (h *dbHandle) LoopMarkers() store.LoopMarkers {
	if h.my != nil {
		return mysqlstore.NewLoopMarkers(h.my)
	}
	return pgstore.NewReservations(h.pg)
}

// Breakers はサーキットブレーカーの契約の和。
type breakers interface {
	store.Breakers
	store.BreakerAdmin
}

// Breakers はサーキットブレーカーの契約を返す。
func (h *dbHandle) Breakers() breakers {
	if h.my != nil {
		return mysqlstore.NewBreakers(h.my)
	}
	return pgstore.NewBreakers(h.pg)
}

// NeverScheduled は番組終了後に schedule が観測されなかった事実の契約を返す。
func (h *dbHandle) NeverScheduled() store.NeverScheduledEvents {
	if h.my != nil {
		return mysqlstore.NewNeverScheduledEvents(h.my)
	}
	return pgstore.NewNeverScheduledEvents(h.pg)
}

// scheduleSync は schedule 同期の契約の和。
type scheduleSync interface {
	store.ScheduleSync
	store.ScheduleSyncReader
}

// ScheduleSync は schedule 同期の契約を返す。
func (h *dbHandle) ScheduleSync() scheduleSync {
	if h.my != nil {
		return mysqlstore.NewScheduleSync(h.my)
	}
	return pgstore.NewScheduleSync(h.pg)
}

// epg は EPG の契約の和。
type epg interface {
	store.EPGSync
	store.EPGReader
	store.ProgramSearch
}

// EPG は EPG の契約を返す。
func (h *dbHandle) EPG() epg {
	if h.my != nil {
		return mysqlstore.NewEPG(h.my)
	}
	return pgstore.NewEPG(h.pg)
}

// tuners はチューナーの契約の和。
type tuners interface {
	store.TunerReader
	store.Capacity
	store.TunerSync
}

// Tuners はチューナーの契約を返す。
func (h *dbHandle) Tuners() tuners {
	if h.my != nil {
		return mysqlstore.NewTuners(h.my)
	}
	return pgstore.NewTuners(h.pg)
}

// cmStations は CM 検出の契約の和。
type cmStations interface {
	store.CMStation
	store.CMDetection
}

// CMStations は storeName の置き場に束縛した CM 検出の契約を返す（空なら全置き場を見る API 用）。
func (h *dbHandle) CMStations(storeName string) cmStations {
	if h.my != nil {
		return mysqlstore.NewAnalysis(h.my, storeName)
	}
	return pgstore.NewAnalysis(h.pg, storeName)
}

// ChapterEdits はチャプター編集の契約を返す。
func (h *dbHandle) ChapterEdits() store.ChapterEdits {
	if h.my != nil {
		return mysqlstore.NewChapterEdits(h.my)
	}
	return pgstore.NewAnalysis(h.pg, "")
}

// Playback は再生位置と視聴済み印の契約を返す。
func (h *dbHandle) Playback() store.PlaybackState {
	if h.my != nil {
		return mysqlstore.NewPlayback(h.my)
	}
	return pgstore.NewPlayback(h.pg)
}

// rules は録画ルールの契約の和。
type rules interface {
	store.Rules
	store.RuleImport
}

// Rules は録画ルールの契約を siteNames のサイトに絞って返す。
func (h *dbHandle) Rules(siteNames []string) rules {
	if h.my != nil {
		return mysqlstore.NewRules(h.my, siteNames)
	}
	return pgstore.NewRules(h.pg, siteNames)
}

// labelRules は分類ルールの契約の和。
type labelRules interface {
	store.LabelRules
	store.LabelRuleReevaluation
}

// LabelRules は分類ルールの契約を返す。
func (h *dbHandle) LabelRules() labelRules {
	if h.my != nil {
		return mysqlstore.NewLabelRules(h.my)
	}
	return pgstore.NewLabelRules(h.pg)
}

// Catalog は storeName の置き場のカタログの契約を返す。
func (h *dbHandle) Catalog(siteNames []string, storeName string) store.Catalog {
	if h.my != nil {
		return mysqlstore.NewCatalog(h.my, siteNames, storeName)
	}
	return pgstore.NewCatalog(h.pg, siteNames, storeName)
}

// NotificationSource は通知の購読元を返す。MySQL は通知表をポーリングし、PostgreSQL は LISTEN する。
func (h *dbHandle) NotificationSource() notifier.Source {
	if h.my != nil {
		return mysqlsource.New(h.my)
	}
	return pgsource.New(h.pg)
}

// NotificationPruner は通知表の掃除の契約を返す。PostgreSQL は通知表を持たないので nil を返す。
func (h *dbHandle) NotificationPruner() store.NotificationPruner {
	if h.my != nil {
		return mysqlstore.NewNotifications(h.my)
	}
	return nil
}

// Locker はリーダー選出の排他ロックの方言の実装を返す。
func (h *dbHandle) Locker() role.Locker {
	if h.my != nil {
		return mysqllocker.New(h.my)
	}
	return pglocker.New(h.pg)
}

// CheckMigrationsCurrent はスキーマが埋め込みの最新 migration であることを確かめる。
func (h *dbHandle) CheckMigrationsCurrent(ctx context.Context, cfg *config.Config) error {
	if h.my != nil {
		return mysqldb.CheckMigrationsCurrent(ctx, h.my)
	}
	return db.CheckMigrationsCurrent(ctx, cfg.DB.DSN())
}
