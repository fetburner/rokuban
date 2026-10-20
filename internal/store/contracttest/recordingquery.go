package contracttest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// RecordingQueryRecording は録画検索契約のケースに必要な録画状態を表す。
type RecordingQueryRecording struct {
	Title       string
	Description string
	Genres      []int16
	Site        string
	ChannelType string
	NetworkID   int32
	ServiceID   int32
	EventID     int32
	StartAt     time.Time
	Status      string
	Source      string
	RuleID      *int64
	Trashed     bool
	Purged      bool
	Superseded  bool
	Watched     bool
	Playable    bool

	PlaybackPositionMs *int64
	PlaybackUpdatedAt  *time.Time
}

// NewQueryRecording は検索契約のケース用に永続化済み録画を作る。
type NewQueryRecording func(t *testing.T, options RecordingQueryRecording) int64

// RecordingQueryFixture は契約実装と、テスト状態を作るフックをまとめる。
type RecordingQueryFixture struct {
	Store        store.RecordingQuery
	NewRecording NewQueryRecording
	RuleID       int64
	OtherRuleID  int64
}

// NewRecordingQueryStore は実在の契約実装とフィクスチャを作る。
type NewRecordingQueryStore func(t *testing.T) RecordingQueryFixture

// RunRecordingQuery は実在の store 実装に対して録画検索の契約を実行する。
func RunRecordingQuery(t *testing.T, newStore NewRecordingQueryStore) {
	t.Helper()
	t.Run("get reports missing and preserves trash facts", func(t *testing.T) { runRecordingQueryGet(t, newStore) })
	t.Run("list applies every filter axis", func(t *testing.T) { runRecordingQueryFilterAxes(t, newStore) })
	t.Run("keyword target and LIKE characters", func(t *testing.T) { runRecordingQueryKeyword(t, newStore) })
	t.Run("series and trash scope", func(t *testing.T) { runRecordingQuerySeriesAndTrash(t, newStore) })
	t.Run("keyset order and cursor", func(t *testing.T) { runRecordingQueryCursor(t, newStore) })
	t.Run("continue watching uses playback and watched facts", func(t *testing.T) { runRecordingQueryContinueWatching(t, newStore) })
	t.Run("shelves share filtered population with list", func(t *testing.T) { runRecordingQueryShelves(t, newStore) })
}

func runRecordingQueryGet(t *testing.T, newStore NewRecordingQueryStore) {
	f := newStore(t)
	ctx := context.Background()
	if _, err := f.Store.Get(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	trashed := f.NewRecording(t, queryRecording("ごみ箱の行", base, RecordingQueryRecording{Trashed: true}))
	row, err := f.Store.Get(ctx, trashed)
	if err != nil {
		t.Fatalf("Get(trashed) error = %v", err)
	}
	if row.ID != trashed || row.DeletedAt == nil {
		t.Fatalf("Get(trashed) = %+v, want row with deleted_at", row)
	}
	purged := f.NewRecording(t, queryRecording("purged row", base.Add(time.Minute), RecordingQueryRecording{Trashed: true, Purged: true}))
	if _, err := f.Store.Get(ctx, purged); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(purged) error = %v, want ErrNotFound", err)
	}
}

func runRecordingQueryFilterAxes(t *testing.T, newStore NewRecordingQueryStore) {
	f := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	end := base.Add(time.Hour)
	makeMatch := func(title string, start time.Time, networkID, serviceID int32) RecordingQueryRecording {
		return queryRecording(title, start, RecordingQueryRecording{
			Description: "matching description",
			Genres:      []int16{7},
			Site:        "tokyo",
			ChannelType: "BS",
			NetworkID:   networkID,
			ServiceID:   serviceID,
			Status:      "failed",
			Source:      "rule",
			RuleID:      &f.RuleID,
		})
	}
	primary := f.NewRecording(t, makeMatch("News 100% report", base, 11, 21))
	paired := f.NewRecording(t, makeMatch("News 100% report", base.Add(time.Minute), 12, 22))
	ids := []int64{primary, paired}
	add := func(change func(*RecordingQueryRecording)) {
		o := makeMatch("News 100% report", base.Add(2*time.Minute), 11, 21)
		change(&o)
		id := f.NewRecording(t, o)
		ids = append(ids, id)
	}
	add(func(o *RecordingQueryRecording) { o.Genres = []int16{3} })
	add(func(o *RecordingQueryRecording) { o.ChannelType = "GR" })
	add(func(o *RecordingQueryRecording) { o.Site = "osaka" })
	add(func(o *RecordingQueryRecording) { o.NetworkID, o.ServiceID = 11, 22 }) // not one of the allowed service pairs
	add(func(o *RecordingQueryRecording) { o.Status = "finished" })
	add(func(o *RecordingQueryRecording) { o.Source = "manual" })
	add(func(o *RecordingQueryRecording) { o.RuleID = &f.OtherRuleID })
	add(func(o *RecordingQueryRecording) { o.StartAt = base.Add(-time.Nanosecond) })
	add(func(o *RecordingQueryRecording) { o.StartAt = end })
	add(func(o *RecordingQueryRecording) { o.Title = "News 100x report" }) // % must be literal
	add(func(o *RecordingQueryRecording) { o.Title, o.Description = "No title match", "100% in description" })
	add(func(o *RecordingQueryRecording) { o.Superseded = true })
	// This row matches every value filter but is absent from the supplied ID set.
	excludedByID := f.NewRecording(t, makeMatch("News 100% report", base.Add(3*time.Minute), 11, 21))

	from := base
	idSet := append([]int64(nil), ids...)
	filter := store.RecordingFilter{
		Q:                  "100%",
		QTarget:            "title",
		Genres:             []int16{7},
		ChannelTypes:       []string{"BS"},
		Sites:              []string{"tokyo"},
		Services:           []store.RecordingServiceRef{{NetworkID: 11, ServiceID: 21}, {NetworkID: 12, ServiceID: 22}},
		Status:             "failed",
		EncodeRecordingIDs: &idSet,
		Source:             "rule",
		RuleID:             &f.RuleID,
		From:               &from,
		To:                 &end,
		SortDesc:           true,
		Limit:              100,
	}
	rows, err := f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(all filter axes) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{paired, primary})
	for _, row := range rows {
		if row.ID == excludedByID {
			t.Fatalf("List ignored EncodeRecordingIDs and returned %d", excludedByID)
		}
	}
	filter.EncodeRecordingIDs = &[]int64{}
	rows, err = f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(empty EncodeRecordingIDs) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{})
}

func runRecordingQueryKeyword(t *testing.T, newStore NewRecordingQueryStore) {
	f := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	literal := f.NewRecording(t, queryRecording("ＮＨＫ 100%_ report", base, RecordingQueryRecording{}))
	wildcardDecoy := f.NewRecording(t, queryRecording("NHK 100xx report", base.Add(time.Minute), RecordingQueryRecording{}))
	description := f.NewRecording(t, queryRecording("別番組", base.Add(2*time.Minute), RecordingQueryRecording{Description: "ＮＨＫ 特集"}))

	filter := store.RecordingFilter{Q: "nhk 100%_", QTarget: "title", SortDesc: true, Limit: 10}
	rows, err := f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(title keyword) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{literal})

	filter = store.RecordingFilter{Q: "ＮＨＫ", SortDesc: true, Limit: 10}
	rows, err = f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(title and description keyword) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{description, wildcardDecoy, literal})
}

func runRecordingQuerySeriesAndTrash(t *testing.T, newStore NewRecordingQueryStore) {
	f := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	first := f.NewRecording(t, queryRecording("作品A", base, RecordingQueryRecording{}))
	second := f.NewRecording(t, queryRecording("作品A", base.Add(time.Hour), RecordingQueryRecording{}))
	differentSeries := f.NewRecording(t, queryRecording("作品B", base.Add(2*time.Hour), RecordingQueryRecording{}))
	superseded := f.NewRecording(t, queryRecording("作品A", base.Add(3*time.Hour), RecordingQueryRecording{Superseded: true}))
	trashed := f.NewRecording(t, queryRecording("作品A", base.Add(4*time.Hour), RecordingQueryRecording{Trashed: true}))
	f.NewRecording(t, queryRecording("作品A", base.Add(5*time.Hour), RecordingQueryRecording{Trashed: true, Purged: true}))

	filter := store.RecordingFilter{SeriesOf: &first, SortDesc: true, Limit: 20}
	rows, err := f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(seriesOf) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{second, first})

	filter = store.RecordingFilter{Trash: true, SortDesc: true, Limit: 20}
	rows, err = f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(trash) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{trashed})

	filter = store.RecordingFilter{SortDesc: true, Limit: 20}
	rows, err = f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(live) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{superseded, differentSeries, second, first})
}

func runRecordingQueryCursor(t *testing.T, newStore NewRecordingQueryStore) {
	f := newStore(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	first := f.NewRecording(t, queryRecording("同時刻 A", start, RecordingQueryRecording{}))
	middle := f.NewRecording(t, queryRecording("同時刻 B", start, RecordingQueryRecording{}))
	last := f.NewRecording(t, queryRecording("同時刻 C", start, RecordingQueryRecording{}))

	filter := store.RecordingFilter{SortDesc: true, Limit: 3}
	rows, err := f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(desc) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{last, middle, first})

	beforeID := middle
	filter.Before, filter.BeforeID = &start, &beforeID
	rows, err = f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(desc cursor) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{first})

	filter.SortDesc = false
	rows, err = f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(asc cursor) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{last})
}

func runRecordingQueryContinueWatching(t *testing.T, newStore NewRecordingQueryStore) {
	f := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	updatedEarly := base.Add(time.Hour)
	updatedLate := base.Add(2 * time.Hour)
	position := int64(5000)
	first := f.NewRecording(t, queryRecording("再開 1", base, RecordingQueryRecording{PlaybackPositionMs: &position, PlaybackUpdatedAt: &updatedLate}))
	second := f.NewRecording(t, queryRecording("再開 2", base.Add(time.Minute), RecordingQueryRecording{PlaybackPositionMs: &position, PlaybackUpdatedAt: &updatedEarly}))
	watchedCandidate := f.NewRecording(t, queryRecording("再開済み", base.Add(2*time.Minute), RecordingQueryRecording{PlaybackPositionMs: &position, PlaybackUpdatedAt: &base, NetworkID: 100, ServiceID: 200, Site: "tokyo"}))
	f.NewRecording(t, queryRecording("視聴済みの同一イベント", base.Add(2*time.Minute), RecordingQueryRecording{Watched: true, NetworkID: 100, ServiceID: 200, Site: "osaka"}))
	f.NewRecording(t, queryRecording("失敗", base.Add(3*time.Minute), RecordingQueryRecording{Status: "failed", PlaybackPositionMs: &position}))
	f.NewRecording(t, queryRecording("ごみ箱", base.Add(4*time.Minute), RecordingQueryRecording{Trashed: true, PlaybackPositionMs: &position}))
	f.NewRecording(t, queryRecording("superseded", base.Add(5*time.Minute), RecordingQueryRecording{Superseded: true, PlaybackPositionMs: &position}))
	f.NewRecording(t, queryRecording("位置なし", base.Add(6*time.Minute), RecordingQueryRecording{}))

	filter := store.RecordingFilter{ContinueWatching: true, SortDesc: true, Limit: 10}
	rows, err := f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(continue watching) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{first, second})
	for _, row := range rows {
		if row.ID == watchedCandidate {
			t.Fatalf("List(continue watching) returned an event watched on a sibling recording: %d", watchedCandidate)
		}
	}
	filter.Limit = 1
	rows, err = f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(continue watching limit) error = %v", err)
	}
	assertRecordingIDs(t, rows, []int64{first})
}

func runRecordingQueryShelves(t *testing.T, newStore NewRecordingQueryStore) {
	f := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	matchGenre := []int16{7}
	first := f.NewRecording(t, queryRecording("作品A", base, RecordingQueryRecording{Genres: matchGenre, Playable: true, NetworkID: 100, ServiceID: 200, Site: "tokyo"}))
	latest := f.NewRecording(t, queryRecording("作品A", base.Add(time.Hour), RecordingQueryRecording{Genres: matchGenre, Playable: true, NetworkID: 100, ServiceID: 200, Site: "tokyo"}))
	// A watched row outside the filter still marks the same broadcast event watched.
	f.NewRecording(t, queryRecording("作品A", base, RecordingQueryRecording{Genres: []int16{3}, Watched: true, NetworkID: 100, ServiceID: 200, Site: "osaka"}))

	filter := store.RecordingFilter{Genres: matchGenre, SortDesc: true, Limit: 10}
	list, err := f.Store.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(shelf filter) error = %v", err)
	}
	assertRecordingIDs(t, list, []int64{latest, first})
	shelves, err := f.Store.Shelves(ctx, filter)
	if err != nil {
		t.Fatalf("Shelves(filter) error = %v", err)
	}
	if len(shelves) != 1 {
		t.Fatalf("Shelves(filter) returned %d groups, want 1: %+v", len(shelves), shelves)
	}
	shelf := shelves[0]
	if shelf.Count != int64(len(list)) || shelf.Count != 2 {
		t.Fatalf("shelf count = %d, list rows = %d, want 2", shelf.Count, len(list))
	}
	if shelf.PlayableCount != 2 || shelf.UnwatchedCount != 1 {
		t.Fatalf("playable/unwatched = %d/%d, want 2/1", shelf.PlayableCount, shelf.UnwatchedCount)
	}
	if shelf.Title != "作品A" || shelf.RepresentativeID != latest || !shelf.LatestStartAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("shelf representative = %+v, want latest recording %d", shelf, latest)
	}
}

func queryRecording(title string, start time.Time, options RecordingQueryRecording) RecordingQueryRecording {
	options.Title = title
	options.StartAt = start
	return options
}

func assertRecordingIDs(t *testing.T, rows []store.RecordingQueryRow, want []int64) {
	t.Helper()
	got := make([]int64, len(rows))
	for i, row := range rows {
		got[i] = row.ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recording IDs = %v, want %v", got, want)
	}
}
