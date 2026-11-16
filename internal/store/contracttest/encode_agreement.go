package contracttest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// encodeAgreementAttemptLimit は試行上限。上限の手前（count = limit-1）と上限（count = limit）を
// 分けるために使う。
const encodeAgreementAttemptLimit = 3

// runEncodePendingAgreement は、encode 投入の候補（ListMissingEncodeProfiles）と待機列
// （ListEncodeQueue）が同じ録画の集合を返すことを、述語の各境界の状態から確かめる。
//
// 両方は「エンコードしてよいか」の述語（unencoded_desired_profiles・encodable_recordings・
// encodable_profile・unsatisfied_encode_rebuilds・encode_attempts_exhausted）を SQL で共有する。
// 片方にだけ条件を書き足すと、待機列に出るのに投入されない録画（またはその逆）ができる。
//
// 全区間カットで keep が空になった cut は、この組の一致の対象に含めない。reconcile は Go で keep を
// 導出して投入せず、待機列は SQL だけで数えるので、両者は意図的に食い違う（docs/data/jobs.md §エンコード待機列）。
// その食い違いは api の TestEncodeReconcileAndQueueAgreeOnPendingSet が見る。
func runEncodePendingAgreement(t *testing.T, newFixture NewMediaFixture) {
	f := newFixture(t)
	requireEncodeAgreementFixture(t, f)
	ctx := context.Background()
	const h264, cut = "h264", "cut"
	known := []string{h264, cut}
	cutProfiles := []string{cut}
	keep := []chapters.Range{{StartMs: 0, EndMs: 1000}}

	titles := map[int64]string{}
	newRecording := func(title string, profiles []string, withOriginal bool) int64 {
		t.Helper()
		id := f.NewRecording(t)
		titles[id] = title
		if withOriginal {
			f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/%s/%d-original.m2ts", f.Site, id), "active")
		}
		f.SetEncodeProfiles(t, id, profiles)
		return id
	}

	// 待機列と投入の両方に入る。
	newRecording("不足", []string{h264}, true)
	deleting := newRecording("削除中の版", []string{h264}, true)
	h264Profile := h264
	deletingAsset := f.NewAsset(t, deleting, "encoded", &h264Profile, fmt.Sprintf("sites/%s/%d-h264.mp4", f.Site, deleting), "active")
	f.SetAssetState(t, deletingAsset, "deleting")
	owned := newRecording("cut 確認済み", []string{cut}, true)
	f.SetChapterSpans(t, owned, nil)
	rebuild := newRecording("作り直し要求", []string{cut}, true)
	f.SetEncodedAssetRebuildRequest(t, rebuild, cut, keep)
	below := newRecording("試行上限の手前", []string{h264}, true)
	f.BeginEncodeAttempt(t, below, h264)
	f.FailEncodeAttempt(t, below, h264, encodeAgreementAttemptLimit-1)
	f.BeginEncodeAttempt(t, newRecording("実行中", []string{h264}, true), h264)

	// 待機列にも投入にも入らない。
	trashed := newRecording("ごみ箱", []string{h264}, true)
	f.SetRecordingTrashed(t, trashed)
	newRecording("原本なし", []string{h264}, false)
	newRecording("設定に無い", []string{"gone"}, true)
	newRecording("cut 未確認", []string{cut}, true)
	trashedRebuild := newRecording("ごみ箱の作り直し要求", []string{cut}, true)
	f.SetRecordingTrashed(t, trashedRebuild)
	f.SetEncodedAssetRebuildRequest(t, trashedRebuild, cut, keep)
	noOriginalRebuild := newRecording("原本なしの作り直し要求", []string{cut}, false)
	f.SetEncodedAssetRebuildRequest(t, noOriginalRebuild, cut, keep)
	exhausted := newRecording("試行上限", []string{h264}, true)
	f.BeginEncodeAttempt(t, exhausted, h264)
	f.FailEncodeAttempt(t, exhausted, h264, encodeAgreementAttemptLimit)
	exhaustedRebuild := newRecording("試行上限の作り直し", []string{cut}, true)
	f.SetEncodedAssetRebuildRequest(t, exhaustedRebuild, cut, keep)
	f.BeginEncodeAttempt(t, exhaustedRebuild, cut)
	f.FailEncodeAttempt(t, exhaustedRebuild, cut, encodeAgreementAttemptLimit)

	wantPending := []string{"不足", "削除中の版", "cut 確認済み", "作り直し要求", "試行上限の手前", "実行中"}
	wantRunning := []string{"実行中"}
	want := map[string]int{}
	for _, title := range wantPending {
		want[title] = 1
	}

	missing, err := f.Reconcile.ListMissingEncodeProfiles(ctx, store.MissingEncodeProfilesParams{
		KnownProfiles: known, CutProfiles: cutProfiles, AttemptLimit: encodeAgreementAttemptLimit, RowLimit: 100,
	})
	if err != nil {
		t.Fatalf("ListMissingEncodeProfiles: %v", err)
	}
	gotMissing := map[string]int{}
	for _, row := range missing {
		gotMissing[titles[row.RecordingID]]++
	}
	if !maps.Equal(gotMissing, want) {
		t.Fatalf("ListMissingEncodeProfiles recordings = %v, want %v", gotMissing, want)
	}

	queue, err := f.Reader.ListEncodeQueue(ctx, store.EncodeQueueParams{
		KnownProfiles: known, CutProfiles: cutProfiles, AttemptLimit: encodeAgreementAttemptLimit,
	})
	if err != nil {
		t.Fatalf("ListEncodeQueue: %v", err)
	}
	gotQueue := map[string]int{}
	var gotRunning []string
	for _, entry := range queue {
		title := titles[entry.RecordingID]
		gotQueue[title]++
		if entry.Running {
			gotRunning = append(gotRunning, title)
		}
	}
	if !maps.Equal(gotQueue, want) {
		t.Fatalf("ListEncodeQueue recordings = %v, want %v (must agree with ListMissingEncodeProfiles)", gotQueue, want)
	}
	slices.Sort(gotRunning)
	if !slices.Equal(gotRunning, wantRunning) {
		t.Fatalf("ListEncodeQueue running recordings = %v, want %v", gotRunning, wantRunning)
	}
}

// requireEncodeAgreementFixture は runEncodePendingAgreement が使う状態作りのフックを確かめる。
func requireEncodeAgreementFixture(t *testing.T, f MediaFixture) {
	t.Helper()
	requireMediaReaderFixture(t, f)
	if f.Reconcile == nil || f.SetChapterSpans == nil || f.SetRecordingTrashed == nil ||
		f.SetEncodedAssetRebuildRequest == nil || f.SetAssetState == nil {
		t.Fatal("media fixture is missing encode agreement setup functions")
	}
}
