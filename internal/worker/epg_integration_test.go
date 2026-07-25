package worker

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

func integrationMirakcURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MIRAKC_URL")
	if url == "" {
		t.Skip("MIRAKC_URL not set")
	}
	return url
}

// TestIntegration_EpgSync は実 mirakc の EPG が DB に載り、放送済み番組が刈り取られることを
// 確認する（M1-6 の受け入れ基準）。MIRAKC_URL と ROKUBAN_TEST_DATABASE_URL の両方が必要。
func TestIntegration_EpgSync(t *testing.T) {
	mirakcURL := integrationMirakcURL(t)
	pool := setupTestPool(t)
	ctx := context.Background()

	w := &EpgSyncWorker{
		MirakcClient:   mirakc.NewClient(mirakcURL, nil),
		Pool:           pool,
		RetentionGrace: 24 * time.Hour,
	}

	start := time.Now()
	runEpgSync(t, w)
	t.Logf("full EPG sync took %v", time.Since(start))

	q := sqlcgen.New(pool)

	services, err := q.ListEpgServices(ctx, testSite)
	if err != nil {
		t.Fatalf("ListEpgServices: %v", err)
	}
	if len(services) == 0 {
		t.Fatal("no services projected from real mirakc")
	}
	for _, s := range services {
		t.Logf("service: %s (%s/%s) remote_key=%d network=%d service=%d",
			s.Name, s.ChannelType, s.Channel, s.RemoteControlKeyID, s.NetworkID, s.ServiceID)
	}

	count, err := q.CountEpgPrograms(ctx, testSite)
	if err != nil {
		t.Fatalf("CountEpgPrograms: %v", err)
	}
	if count == 0 {
		t.Fatal("no programs projected from real mirakc")
	}
	t.Logf("projected %d programs", count)

	// ローリングウィンドウ: 猶予を超えた放送済み番組は残っていない
	var tooOld int64
	cutoff := time.Now().Add(-24 * time.Hour)
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM epg_programs WHERE site = $1 AND end_at < $2", testSite, cutoff,
	).Scan(&tooOld); err != nil {
		t.Fatalf("counting aired programs: %v", err)
	}
	if tooOld != 0 {
		t.Errorf("%d programs older than the retention window survived", tooOld)
	}

	// 現在放送中の番組が UI 完全形で載っていること
	now := time.Now()
	airing, err := q.ListEpgPrograms(ctx, sqlcgen.ListEpgProgramsParams{
		Site:        testSite,
		WindowStart: now,
		WindowEnd:   now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ListEpgPrograms: %v", err)
	}
	if len(airing) == 0 {
		t.Fatal("no programs airing right now — EPG window looks wrong")
	}
	t.Logf("%d programs airing now", len(airing))

	var withName, withGenre, withVideo, withAudios, withExtended int
	for _, p := range airing {
		if p.Name != "" {
			withName++
		}
		if len(p.GenreLv1) > 0 {
			withGenre++
		}
		if p.Video != nil {
			withVideo++
		}
		if p.Audios != nil {
			withAudios++
		}
		if p.Extended != nil {
			withExtended++
		}
		if !p.EndAt.Equal(p.StartAt.Add(time.Duration(p.DurationMs) * time.Millisecond)) {
			t.Errorf("program %d: end_at %v != start_at + duration_ms", p.ProgramID, p.EndAt)
		}
	}
	t.Logf("airing programs with name=%d genre=%d video=%d audios=%d extended=%d (of %d)",
		withName, withGenre, withVideo, withAudios, withExtended, len(airing))
	if withName == 0 {
		t.Error("no airing program has a name")
	}
	if withVideo == 0 || withAudios == 0 {
		t.Error("映像・音声属性がまったく投影されていない（UI 完全形が崩れている）")
	}

	// jsonb 詳細が実データで往復すること
	sample := airing[0]
	if sample.Video != nil {
		var v mirakc.VideoInfo
		if err := json.Unmarshal(sample.Video, &v); err != nil {
			t.Errorf("unmarshalling real video payload: %v", err)
		}
	}
	if sample.Genres != nil {
		var g []mirakc.Genre
		if err := json.Unmarshal(sample.Genres, &g); err != nil {
			t.Errorf("unmarshalling real genres payload: %v", err)
		}
	}
	t.Logf("sample: %q on service %d, %v - %v",
		sample.Name, sample.ServiceID, sample.StartAt, sample.EndAt)

	// 2 回目の同期が冪等（行数が増えない）こと
	before := count
	runEpgSync(t, w)
	after, err := q.CountEpgPrograms(ctx, testSite)
	if err != nil {
		t.Fatalf("CountEpgPrograms: %v", err)
	}
	// EPG は同期の合間にも更新されるため厳密一致は求めず、桁が変わらないことを見る
	if after < before/2 || after > before*2 {
		t.Errorf("second sync changed program count from %d to %d (upsert may be duplicating)", before, after)
	}
	t.Logf("second sync: %d programs (was %d)", after, before)
}
