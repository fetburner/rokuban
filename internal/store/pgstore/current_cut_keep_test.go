package pgstore

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

type currentCutKeepTraceKey struct{}

type currentCutKeepBarrier struct {
	stateRead    chan struct{}
	continueRead chan struct{}
	once         sync.Once
}

func (tracer *currentCutKeepBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "GetRecordingChapterState") {
		return context.WithValue(ctx, currentCutKeepTraceKey{}, true)
	}
	return ctx
}

func (tracer *currentCutKeepBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if waiting, _ := ctx.Value(currentCutKeepTraceKey{}).(bool); !waiting {
		return
	}
	tracer.once.Do(func() {
		close(tracer.stateRead)
		<-tracer.continueRead
	})
}

func TestGetCurrentCutKeepReadsOwnershipAndSpansFromOneSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.ProgramDurationMs = 60_000
	})
	if _, err := pool.Exec(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
		t.Fatalf("seeding chapter ownership: %v", err)
	}
	for _, span := range []struct {
		start int64
		end   int64
		label string
		cut   bool
	}{
		{start: 0, end: 30_000, label: "main"},
		{start: 30_000, end: 45_000, label: "CM", cut: true},
		{start: 45_000, end: 60_000, label: "end"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range($2, $3), $4, $5)`,
			recordingID, span.start, span.end, span.label, span.cut); err != nil {
			t.Fatalf("seeding chapter span: %v", err)
		}
	}

	want, wantOwned, err := NewMedia(pool, "tokyo").GetCurrentCutKeep(ctx, recordingID)
	if err != nil || !wantOwned || len(want) != 2 {
		t.Fatalf("pre-reset current keep = (%v, %v, %v), want two owned ranges", want, wantOwned, err)
	}

	tracer := &currentCutKeepBarrier{stateRead: make(chan struct{}), continueRead: make(chan struct{})}
	config := pool.Config()
	config.ConnConfig.Tracer = tracer
	tracedPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("creating traced database pool: %v", err)
	}
	t.Cleanup(tracedPool.Close)

	type result struct {
		keep  []chapters.Range
		owned bool
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		keep, owned, err := NewMedia(tracedPool, "tokyo").GetCurrentCutKeep(ctx, recordingID)
		resultCh <- result{keep: keep, owned: owned, err: err}
	}()

	select {
	case <-tracer.stateRead:
	case <-time.After(10 * time.Second):
		t.Fatal("GetCurrentCutKeep did not read chapter ownership")
	}
	if err := NewAnalysis(pool).Reset(ctx, recordingID); err != nil {
		t.Fatalf("resetting chapters between the two reads: %v", err)
	}
	close(tracer.continueRead)

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("GetCurrentCutKeep did not finish after the chapter reset")
	}
	if got.err != nil || !got.owned || !chapters.SameRanges(got.keep, want) {
		t.Fatalf("GetCurrentCutKeep across reset = (%v, %v, %v), want (%v, true, nil) from the pre-reset snapshot", got.keep, got.owned, got.err, want)
	}
}
