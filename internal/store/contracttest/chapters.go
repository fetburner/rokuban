// Package contracttest は store の契約を実装に対して検査する統合テストを持つ。
package contracttest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// RecordingOptions は契約のケースが要る録画の最小限の状態を表す。
type RecordingOptions struct {
	CMDetectionEnabled bool
	Trashed            bool
	Purged             bool
	EventID            int32
	ProgramStartAt     time.Time
}

// NewRecording は契約のケース用に永続化済みの録画を作る。
type NewRecording func(t *testing.T, options RecordingOptions) int64

// Fixture は契約の実装と、ケースが状態を作るためのフックをまとめる。
// フックは実装ごとの DB へ直接書くので、実装側のテストが与える。
type Fixture struct {
	Store        store.ChapterEdits
	NewRecording NewRecording
	// SaveCMDetection は CM 検出結果を commit する。
	SaveCMDetection func(t *testing.T, recordingID int64, ranges []chapters.Range)
	// BeginCMDetection は CM 検出 worker と同じく録画の行をロックして結果を書く
	// tx を開き、commit する関数を返す。commit までロックを保持する。
	BeginCMDetection func(t *testing.T, recordingID int64, ranges []chapters.Range) (commit func())
}

// NewStore は実在の契約実装とフィクスチャを作る。
type NewStore func(t *testing.T) Fixture

// Run は実在の store 実装に対してチャプター編集の契約を実行する。
func Run(t *testing.T, newStore NewStore) {
	t.Helper()

	t.Run("read and missing recording", func(t *testing.T) { runReadAndMissing(t, newStore) })
	t.Run("replace checks version and reset is idempotent", func(t *testing.T) { runReplaceAndReset(t, newStore) })
	t.Run("detection gate follows deployment setting", func(t *testing.T) { runDetectionGate(t, newStore) })
	t.Run("detection commit changes the version", func(t *testing.T) { runDetectionChangesVersion(t, newStore) })
	t.Run("replace serializes with detection commit", func(t *testing.T) { runDetectionSerialization(t, newStore) })
	t.Run("trashed recordings reject writes", func(t *testing.T) { runTrashed(t, newStore) })
	t.Run("database range conflict rolls back replacement", func(t *testing.T) { runConflictRollback(t, newStore) })
	t.Run("concurrent replacements serialize", func(t *testing.T) { runConcurrentReplacements(t, newStore) })
}

func runDetectionChangesVersion(t *testing.T, newStore NewStore) {
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t, RecordingOptions{CMDetectionEnabled: true})
	before, err := f.Store.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading before detection: %v", err)
	}
	f.SaveCMDetection(t, id, []chapters.Range{{StartMs: 1000, EndMs: 2000}})
	spans := []chapters.Span{{StartMs: 0, EndMs: 1001, Cut: true}}
	if err := f.Store.Replace(ctx, id, before.Version, spans, false); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("Replace(version before detection) error = %v, want ErrStaleVersion", err)
	}
	after, err := f.Store.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading after detection: %v", err)
	}
	if after.Owned || len(after.UserSpans) != 0 || after.Version == before.Version || !after.Detected {
		t.Fatalf("snapshot after stale replacement = %+v, want an unowned detected layer with a changed version", after)
	}
	if err := f.Store.Replace(ctx, id, after.Version, spans, true); err != nil {
		t.Fatalf("Replace after detection completed = %v, want success even when detection is required", err)
	}
}

// runDetectionSerialization は、進行中の検出 tx がある間 Replace が待ち、
// commit 後に新しい状態を見て ErrStaleVersion を返すことを確かめる。
// 待ちの判定は固定時間なので、遅い環境では偽陽性で通りうるが、偽陰性では落ちない。
func runDetectionSerialization(t *testing.T, newStore NewStore) {
	f := newStore(t)
	ctx := context.Background()
	id := f.NewRecording(t, RecordingOptions{CMDetectionEnabled: true})
	pending, err := f.Store.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading pending state: %v", err)
	}
	commit := f.BeginCMDetection(t, id, []chapters.Range{{StartMs: 1000, EndMs: 2000}})

	result := make(chan error, 1)
	go func() {
		result <- f.Store.Replace(ctx, id, pending.Version, []chapters.Span{{StartMs: 0, EndMs: 1001, Cut: true}}, true)
	}()
	select {
	case err := <-result:
		commit()
		t.Fatalf("Replace returned %v while the detection transaction was open, want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}
	commit()
	if err := <-result; !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("Replace after detection commit error = %v, want ErrStaleVersion", err)
	}
}

func runReadAndMissing(t *testing.T, newStore NewStore) {
	f := newStore(t)
	s, newRecording := f.Store, f.NewRecording
	ctx := context.Background()
	if _, err := s.Read(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Read(missing) error = %v, want ErrNotFound", err)
	}
	if err := s.Replace(ctx, -1, "", nil, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Replace(missing) error = %v, want ErrNotFound", err)
	}
	if err := s.Reset(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Reset(missing) error = %v, want ErrNotFound", err)
	}
	purgedID := newRecording(t, RecordingOptions{Purged: true})
	if _, err := s.Read(ctx, purgedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Read(purged) error = %v, want ErrNotFound", err)
	}
	if err := s.Replace(ctx, purgedID, "", nil, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Replace(purged) error = %v, want ErrNotFound", err)
	}
	if err := s.Reset(ctx, purgedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Reset(purged) error = %v, want ErrNotFound", err)
	}
	id := newRecording(t, RecordingOptions{})
	got, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("Read(recording) error = %v", err)
	}
	if got.Owned || got.Version == "" || len(got.UserSpans) != 0 || len(got.CMRanges) != 0 {
		t.Fatalf("initial chapter snapshot = %+v, want an unowned empty layer with a version", got)
	}
}

func runReplaceAndReset(t *testing.T, newStore NewStore) {
	f := newStore(t)
	s, newRecording := f.Store, f.NewRecording
	ctx := context.Background()
	id := newRecording(t, RecordingOptions{})
	initial, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading initial state: %v", err)
	}

	first := []chapters.Span{{StartMs: 1001, EndMs: 2002, Label: "CM", Cut: true}}
	if err := s.Replace(ctx, id, initial.Version, first, false); err != nil {
		t.Fatalf("adopting chapter layer: %v", err)
	}
	owned, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading adopted layer: %v", err)
	}
	if !owned.Owned || !reflect.DeepEqual(owned.UserSpans, first) || owned.Version == initial.Version {
		t.Fatalf("adopted snapshot = %+v, want owned spans and a changed version", owned)
	}

	second := []chapters.Span{{StartMs: 3003, EndMs: 4004, Label: "OP", Cut: false}}
	if err := s.Replace(ctx, id, initial.Version, second, false); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("Replace(stale) error = %v, want ErrStaleVersion", err)
	}
	current, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading after stale replacement: %v", err)
	}
	if !reflect.DeepEqual(current.UserSpans, first) {
		t.Fatalf("spans after stale replacement = %+v, want %+v", current.UserSpans, first)
	}
	if err := s.Replace(ctx, id, current.Version, second, false); err != nil {
		t.Fatalf("replacing chapter layer: %v", err)
	}
	current, err = s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading replaced layer: %v", err)
	}
	if !reflect.DeepEqual(current.UserSpans, second) || current.Version == owned.Version {
		t.Fatalf("replaced snapshot = %+v, want second spans and a changed version", current)
	}
	if err := s.Replace(ctx, id, owned.Version, first, false); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("Replace(version before second edit) error = %v, want ErrStaleVersion", err)
	}
	current, err = s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading after stale replacement: %v", err)
	}
	if !reflect.DeepEqual(current.UserSpans, second) {
		t.Fatalf("spans after stale replacement = %+v, want %+v", current.UserSpans, second)
	}

	if err := s.Reset(ctx, id); err != nil {
		t.Fatalf("resetting chapter layer: %v", err)
	}
	if err := s.Replace(ctx, id, current.Version, first, false); !errors.Is(err, store.ErrStaleVersion) {
		t.Fatalf("Replace(version before reset) error = %v, want ErrStaleVersion", err)
	}
	reset, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading reset layer: %v", err)
	}
	if reset.Owned || len(reset.UserSpans) != 0 {
		t.Fatalf("snapshot after reset = %+v, want automatic layer", reset)
	}
	if err := s.Reset(ctx, id); err != nil {
		t.Fatalf("resetting an already automatic layer: %v", err)
	}
}

func runDetectionGate(t *testing.T, newStore NewStore) {
	f := newStore(t)
	s, newRecording := f.Store, f.NewRecording
	ctx := context.Background()
	id := newRecording(t, RecordingOptions{CMDetectionEnabled: true})
	state, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading pending state: %v", err)
	}
	spans := []chapters.Span{{StartMs: 0, EndMs: 1001, Cut: true}}
	if err := s.Replace(ctx, id, state.Version, spans, true); !errors.Is(err, store.ErrDetectionPending) {
		t.Fatalf("Replace while required detection is pending = %v, want ErrDetectionPending", err)
	}
	if err := s.Replace(ctx, id, state.Version, spans, false); err != nil {
		t.Fatalf("Replace when deployment disables detection = %v", err)
	}
}

func runTrashed(t *testing.T, newStore NewStore) {
	f := newStore(t)
	s, newRecording := f.Store, f.NewRecording
	ctx := context.Background()
	id := newRecording(t, RecordingOptions{Trashed: true})
	state, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading trashed recording: %v", err)
	}
	if err := s.Replace(ctx, id, state.Version, nil, false); !errors.Is(err, store.ErrTrashed) {
		t.Fatalf("Replace(trashed) error = %v, want ErrTrashed", err)
	}
	if err := s.Reset(ctx, id); !errors.Is(err, store.ErrTrashed) {
		t.Fatalf("Reset(trashed) error = %v, want ErrTrashed", err)
	}
}

func runConflictRollback(t *testing.T, newStore NewStore) {
	f := newStore(t)
	s, newRecording := f.Store, f.NewRecording
	ctx := context.Background()
	id := newRecording(t, RecordingOptions{})
	initial, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading initial state: %v", err)
	}
	overlapping := []chapters.Span{
		{StartMs: 0, EndMs: 2002, Label: "CM", Cut: true},
		{StartMs: 1001, EndMs: 3003, Label: "OP", Cut: true},
	}
	if err := s.Replace(ctx, id, initial.Version, overlapping, false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Replace(overlapping) error = %v, want ErrConflict", err)
	}
	after, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("reading after failed replacement: %v", err)
	}
	if after.Owned || len(after.UserSpans) != 0 {
		t.Fatalf("snapshot after failed replacement = %+v, want original automatic layer", after)
	}
}

func runConcurrentReplacements(t *testing.T, newStore NewStore) {
	f := newStore(t)
	s, newRecording := f.Store, f.NewRecording
	ctx := context.Background()
	for round := range 20 {
		runReplacementRound(t, ctx, s, newRecording, round)
	}
}

func runReplacementRound(t *testing.T, ctx context.Context, s store.ChapterEdits, newRecording NewRecording, round int) {
	id := newRecording(t, RecordingOptions{})
	initial, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("round %d: reading initial state: %v", round, err)
	}
	if err := s.Replace(ctx, id, initial.Version, conflictWindowSpans("baseline"), false); err != nil {
		t.Fatalf("round %d: seeding existing chapter intervals: %v", round, err)
	}
	current, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("round %d: reading current version: %v", round, err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	candidates := [][]chapters.Span{conflictWindowSpans("first"), conflictWindowSpans("second")}
	for _, candidate := range candidates {
		candidate := candidate
		go func() {
			<-start
			results <- s.Replace(ctx, id, current.Version, candidate, false)
		}()
	}
	close(start)
	got := []error{<-results, <-results}
	successes, stale := 0, 0
	for _, err := range got {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrStaleVersion):
			stale++
		default:
			t.Fatalf("round %d: competing Replace error = %v, want success or ErrStaleVersion", round, err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("round %d: results = %v, want one success and one stale version", round, got)
	}

	final, err := s.Read(ctx, id)
	if err != nil {
		t.Fatalf("round %d: reading final state: %v", round, err)
	}
	if !reflect.DeepEqual(final.UserSpans, candidates[0]) && !reflect.DeepEqual(final.UserSpans, candidates[1]) {
		t.Fatalf("round %d: final spans = %+v, want one complete replacement", round, final.UserSpans)
	}
}

func conflictWindowSpans(label string) []chapters.Span {
	spans := make([]chapters.Span, 10)
	for i := range spans {
		start := int64(i * 10000)
		spans[i] = chapters.Span{StartMs: start, EndMs: start + 5000, Label: label, Cut: true}
	}
	return spans
}
