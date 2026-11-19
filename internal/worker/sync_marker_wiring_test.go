package worker

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// NewWorkers で登録した EPG・チューナーの同期ワーカーが、確定時刻の印を実際に書くことを見る。
// Deps.LoopMarkers の配線が外れると、ワーカーの Markers が nil になって印が黙って書かれなくなる。
func TestNewWorkersRegisteredSyncJobsWriteSiteMarkers(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	now := time.Now().Truncate(time.Second)

	fake := mirakctest.New(t)
	fake.SetServices([]mirakc.Service{testService(32736, 1024, 1, "ＮＨＫ総合", "27")})
	fake.SetPrograms([]mirakc.Program{testProgram(32736, 1024, 1, "番組A", now.Add(time.Hour), time.Hour)})
	fake.SetTuners([]mirakc.Tuner{{Index: 0, Name: "GR-0", Types: []string{"GR"}}})
	tunerStore := pgstoretest.Tuners(pool)

	registry := NewWorkers(&Deps{
		MirakcClients:     singleSiteClients("", mirakc.NewClient(fake.URL(), nil)),
		EPGSync:           pgstoretest.EPG(pool),
		EpgRetentionGrace: 24 * time.Hour,
		TunerStore:        tunerStore,
		CapacityStore:     tunerStore,
		LoopMarkers:       pgstoretest.Reservations(pool),
	})

	testutil.RequireNoError(t, registry.Work(t.Context(), jobs.EpgSyncArgs{Site: testSite}), "registered EPG sync")
	if _, ok := siteMarkerAt(t, pool, store.LoopEPGSync); !ok {
		t.Fatal("registered EPG sync wrote no epg_sync marker; Deps.LoopMarkers is not wired to EpgSyncWorker")
	}

	testutil.RequireNoError(t, registry.Work(t.Context(), jobs.TunerSyncArgs{Site: testSite}), "registered tuner sync")
	if _, ok := siteMarkerAt(t, pool, store.LoopTunerSync); !ok {
		t.Fatal("registered tuner sync wrote no tuner_sync marker; Deps.LoopMarkers is not wired to TunerSyncWorker")
	}
}
