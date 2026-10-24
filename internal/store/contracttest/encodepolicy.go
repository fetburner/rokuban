package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// EncodePolicyFixture はエンコード方針契約と DB 状態を作るためのフックをまとめる。
type EncodePolicyFixture struct {
	Policy      store.EncodePolicy
	Attempts    store.EncodeAttempts
	Recordings  store.RecordingQuery
	Reconcile   store.MediaReconcile
	Removals    store.MediaDelete
	CMDetection store.CMDetection

	NewRecording        func(t *testing.T) int64
	NewAsset            func(t *testing.T, recordingID int64, kind string, profile *string) int64
	SetRecordingTrashed func(t *testing.T, recordingID int64)
	SetDesired          func(t *testing.T, recordingID int64, profiles []string)

	SetRemovalRequest func(t *testing.T, recordingID int64, profile string)
	// HasRemovalRequest は削除要求の行があるかを返す。契約の読み取りは desired に残る profile の
	// 要求を除いて返すので、再追加や却下の後に行が残っていても見分けられない。
	HasRemovalRequest  func(t *testing.T, recordingID int64, profile string) bool
	SetAttempt         func(t *testing.T, recordingID int64, profile, state string, count int32)
	SetChapterSpans    func(t *testing.T, recordingID int64, durationMs int64, spans []chapters.Span)
	BeginChapterEdit   func(t *testing.T, recordingID int64, durationMs int64, spans []chapters.Span) (commit func())
	BeginPolicyUpdate  func(t *testing.T, recordingID int64, profiles []string) (commit func())
	BeginRemoval       func(t *testing.T, recordingID int64, profile string) (commit func())
	BeginRecordingLock func(t *testing.T, recordingID int64) (commit func())
	TryPolicyLock      func(t *testing.T, recordingID int64)
	WaitRecordingLock  func(t *testing.T)
	WaitPolicyLock     func(t *testing.T)
}

// NewEncodePolicyFixture は実在の契約実装と fixture を作る。
type NewEncodePolicyFixture func(t *testing.T) EncodePolicyFixture

// RunEncodePolicy はエンコード方針と作り直し要求の契約を実装に対して検査する。
func RunEncodePolicy(t *testing.T, newFixture NewEncodePolicyFixture) {
	t.Helper()
	t.Run("append validates the locked desired and restores only failed attempts", func(t *testing.T) {
		runAppendProfiles(t, newFixture)
	})
	t.Run("append validates after a concurrent removal commits", func(t *testing.T) {
		runAppendAfterRemoval(t, newFixture)
	})
	t.Run("concurrent encoded removals preserve one viewable copy", func(t *testing.T) {
		runConcurrentRemovals(t, newFixture)
	})
	t.Run("removal takes the recording lock before the policy lock", func(t *testing.T) {
		runRemovalLockOrder(t, newFixture)
	})
	t.Run("removal validates desired after taking the policy lock", func(t *testing.T) {
		runRemovalValidatesLockedDesired(t, newFixture)
	})
	t.Run("removal skips validation for a profile outside desired", func(t *testing.T) {
		runRemovalSkipsValidateWhenNotDesired(t, newFixture)
	})
	t.Run("removal remains available for trashed recordings", func(t *testing.T) {
		runRemovalOnTrashedRecording(t, newFixture)
	})
	t.Run("cut rebuild stores keep derived from owned chapters", func(t *testing.T) {
		runCutRebuild(t, newFixture)
	})
	t.Run("cut rebuild remains available for trashed recordings", func(t *testing.T) {
		runCutRebuildOnTrashedRecording(t, newFixture)
	})
	t.Run("cut rebuild reads chapters after waiting for the recording lock", func(t *testing.T) {
		runCutRebuildAfterChapterEdit(t, newFixture)
	})
	t.Run("policy update is atomic and returns typed precondition errors", func(t *testing.T) {
		runSetPolicy(t, newFixture)
	})
	t.Run("attempt begin enforces the supplied limit", func(t *testing.T) {
		runAttemptLimit(t, newFixture)
	})
	t.Run("stale attempt tokens cannot mutate a newer running attempt", func(t *testing.T) {
		runAttemptFencing(t, newFixture)
	})
}

func runAppendProfiles(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	f.SetDesired(t, id, []string{"h264"})
	cut := "cut"
	cutAssetID := f.NewAsset(t, id, "encoded", &cut)
	f.SetRemovalRequest(t, id, "cut")
	if !removedEncodedAssetListed(t, f, cutAssetID) {
		t.Fatal("seeded removal request is missing from ListRemovedEncodedAssetsToDelete")
	}
	f.SetAttempt(t, id, "cut", "failed", 25)
	f.SetAttempt(t, id, "h265", "running", 2)

	var calls int
	var validated []string
	err := f.Policy.AppendProfiles(ctx, id, []string{"cut"}, func(desired []string) error {
		calls++
		validated = append([]string(nil), desired...)
		return nil
	})
	if err != nil {
		t.Fatalf("AppendProfiles: %v", err)
	}
	if calls != 1 {
		t.Fatalf("validate calls = %d, want 1", calls)
	}
	if !reflect.DeepEqual(validated, []string{"h264", "cut"}) {
		t.Fatalf("validated desired = %v, want current desired followed by requested names", validated)
	}
	if got := encodePolicyRow(t, f, id).EncodeProfiles; !sameStrings(got, []string{"cut", "h264"}) {
		t.Fatalf("RecordingQuery.Get(%d).EncodeProfiles = %v, want [cut h264]", id, got)
	}
	if f.HasRemovalRequest(t, id, "cut") {
		t.Fatal("re-added profile still has a removal request")
	}
	if state, exists := encodeAttemptState(t, f, id, "cut"); exists || state != "" {
		t.Fatalf("RecordingQuery.Get(%d).EncodeAttempts for cut = (%q, %v), want removed", id, state, exists)
	}
	if state, exists := encodeAttemptState(t, f, id, "h265"); !exists || state != "running" {
		t.Fatalf("RecordingQuery.Get(%d).EncodeAttempts for h265 = (%q, %v), want running", id, state, exists)
	}

	noPolicyID := f.NewRecording(t)
	f.NewAsset(t, noPolicyID, "original", nil)
	var unfreezedDesired []string
	err = f.Policy.AppendProfiles(ctx, noPolicyID, []string{"h265"}, func(desired []string) error {
		unfreezedDesired = append([]string(nil), desired...)
		return nil
	})
	if err != nil {
		t.Fatalf("AppendProfiles without a policy row: %v", err)
	}
	if len(unfreezedDesired) != 1 || unfreezedDesired[0] != "h265" {
		t.Fatalf("validation for an unfrozen policy = %v, want [h265]", unfreezedDesired)
	}
	row := encodePolicyRow(t, f, noPolicyID)
	if row.KeepOriginal != "always" || !sameStrings(row.EncodeProfiles, []string{"h265"}) {
		t.Fatalf("RecordingQuery.Get(%d).KeepOriginal/EncodeProfiles = (%q, %v), want always/[h265]", noPolicyID, row.KeepOriginal, row.EncodeProfiles)
	}

	if err := f.Policy.AppendProfiles(ctx, -1, []string{"h264"}, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AppendProfiles(missing) error = %v, want ErrNotFound", err)
	}
	noOriginalID := f.NewRecording(t)
	if err := f.Policy.AppendProfiles(ctx, noOriginalID, []string{"h264"}, nil); !errors.Is(err, store.ErrOriginalUnavailable) {
		t.Fatalf("AppendProfiles(without original) error = %v, want ErrOriginalUnavailable", err)
	}
}

func runAppendAfterRemoval(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	profile := "h264"
	assetID := f.NewAsset(t, id, "encoded", &profile)
	f.SetDesired(t, id, []string{profile})
	commitRemoval := f.BeginRemoval(t, id, profile)

	finished := make(chan error, 1)
	errRejected := errors.New("selection rejected")
	var calls int
	var validated []string
	go func() {
		finished <- f.Policy.AppendProfiles(ctx, id, []string{"cut"}, func(desired []string) error {
			calls++
			validated = append([]string(nil), desired...)
			return errRejected
		})
	}()
	f.WaitPolicyLock(t)
	commitRemoval()
	err := <-finished
	if !errors.Is(err, errRejected) {
		t.Fatalf("AppendProfiles error = %v, want the validate result returned as is", err)
	}
	if calls != 1 || !reflect.DeepEqual(validated, []string{"cut"}) {
		t.Fatalf("validation calls/desired = %d/%v, want 1/[cut] after removal", calls, validated)
	}
	if got := encodePolicyRow(t, f, id).EncodeProfiles; len(got) != 0 {
		t.Fatalf("RecordingQuery.Get(%d).EncodeProfiles after rejected append = %v, want empty", id, got)
	}
	if !removedEncodedAssetListed(t, f, assetID) {
		t.Fatal("rejected append cleared the committed removal request")
	}
}

func runConcurrentRemovals(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	first, second := "h264", "h265"
	f.NewAsset(t, id, "encoded", &first)
	f.NewAsset(t, id, "encoded", &second)
	f.SetDesired(t, id, []string{first, second})

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, profile := range []string{first, second} {
		profile := profile
		go func() {
			<-start
			results <- f.Policy.RemoveEncodedAsset(ctx, id, profile, nil)
		}()
	}
	close(start)
	var success, lastCopy int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case errors.Is(err, store.ErrLastViewableCopy):
			lastCopy++
		default:
			t.Fatalf("RemoveEncodedAsset error = %v, want success or ErrLastViewableCopy", err)
		}
	}
	if success != 1 || lastCopy != 1 {
		t.Fatalf("remove outcomes = success %d, last-copy %d; want one of each", success, lastCopy)
	}
	desired := encodePolicyRow(t, f, id).EncodeProfiles
	if len(desired) != 1 {
		t.Fatalf("RecordingQuery.Get(%d).EncodeProfiles after concurrent removals = %v, want one remaining profile", id, desired)
	}
	if f.HasRemovalRequest(t, id, desired[0]) {
		t.Fatalf("last-copy rejection left a removal request for remaining profile %q", desired[0])
	}
}

func runRemovalValidatesLockedDesired(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	profile := "h264"
	f.NewAsset(t, id, "encoded", &profile)
	f.SetDesired(t, id, []string{profile})
	commitPolicyUpdate := f.BeginPolicyUpdate(t, id, []string{profile, "cut"})

	finished := make(chan error, 1)
	errRejected := errors.New("selection rejected")
	var calls int
	var validated []string
	go func() {
		finished <- f.Policy.RemoveEncodedAsset(ctx, id, profile, func(desired []string) error {
			calls++
			validated = append([]string(nil), desired...)
			if len(desired) == 1 && desired[0] == "cut" {
				return errRejected
			}
			return nil
		})
	}()
	f.WaitPolicyLock(t)
	commitPolicyUpdate()
	if err := <-finished; !errors.Is(err, errRejected) {
		t.Fatalf("RemoveEncodedAsset error = %v, want the validate result returned as is", err)
	}
	if calls != 1 || !reflect.DeepEqual(validated, []string{"cut"}) {
		t.Fatalf("validation calls/desired = %d/%v, want 1/[cut]", calls, validated)
	}
	if got := encodePolicyRow(t, f, id).EncodeProfiles; !sameStrings(got, []string{profile, "cut"}) {
		t.Fatalf("RecordingQuery.Get(%d).EncodeProfiles after rejected removal = %v, want [h264 cut]", id, got)
	}
	if f.HasRemovalRequest(t, id, profile) {
		t.Fatal("rejected removal left a removal request")
	}
}

func runRemovalSkipsValidateWhenNotDesired(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	profile := "h264"
	assetID := f.NewAsset(t, id, "encoded", &profile)
	f.SetDesired(t, id, []string{"cut"})

	var calls int
	err := f.Policy.RemoveEncodedAsset(ctx, id, profile, func([]string) error {
		calls++
		return errors.New("selection rejected")
	})
	if err != nil || calls != 0 {
		t.Fatalf("RemoveEncodedAsset error/validate calls = %v/%d, want nil/0 for a profile outside desired", err, calls)
	}
	if got := encodePolicyRow(t, f, id).EncodeProfiles; !sameStrings(got, []string{"cut"}) {
		t.Fatalf("RecordingQuery.Get(%d).EncodeProfiles after removing an undesired profile = %v, want [cut]", id, got)
	}
	if !removedEncodedAssetListed(t, f, assetID) {
		t.Fatal("removal request missing for the undesired profile")
	}
}

func runRemovalLockOrder(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	profile := "h264"
	f.NewAsset(t, id, "encoded", &profile)
	f.SetDesired(t, id, []string{profile})
	commitRecordingLock := f.BeginRecordingLock(t, id)

	finished := make(chan error, 1)
	go func() { finished <- f.Policy.RemoveEncodedAsset(ctx, id, profile, nil) }()
	f.WaitRecordingLock(t)
	f.TryPolicyLock(t, id)
	commitRecordingLock()
	if err := <-finished; !errors.Is(err, store.ErrLastViewableCopy) {
		t.Fatalf("RemoveEncodedAsset error = %v, want ErrLastViewableCopy after it acquires both locks", err)
	}
	if got := encodePolicyRow(t, f, id).EncodeProfiles; !sameStrings(got, []string{profile}) {
		t.Fatalf("RecordingQuery.Get(%d).EncodeProfiles after rejected last-copy removal = %v, want [%s]", id, got, profile)
	}
	if f.HasRemovalRequest(t, id, profile) {
		t.Fatal("rejected last-copy removal left a removal request")
	}
}

func runRemovalOnTrashedRecording(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	profile := "h264"
	assetID := f.NewAsset(t, id, "encoded", &profile)
	f.SetDesired(t, id, []string{profile})
	f.SetRecordingTrashed(t, id)

	if err := f.Policy.RemoveEncodedAsset(ctx, id, profile, nil); err != nil {
		t.Fatalf("RemoveEncodedAsset for trashed recording: %v", err)
	}
	if got := encodePolicyRow(t, f, id).EncodeProfiles; len(got) != 0 {
		t.Fatalf("RecordingQuery.Get(%d).EncodeProfiles after removing from trashed recording = %v, want empty", id, got)
	}
	if !removedEncodedAssetListed(t, f, assetID) {
		t.Fatal("removal request missing for trashed recording")
	}
}

func runCutRebuild(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	f.SetDesired(t, id, []string{"cut"})
	spans := []chapters.Span{
		{StartMs: 0, EndMs: 10_000},
		{StartMs: 10_000, EndMs: 15_000, Cut: true},
	}
	f.SetChapterSpans(t, id, 20_000, spans)
	if err := f.Policy.RequestCutRebuild(ctx, id, "cut"); err != nil {
		t.Fatalf("RequestCutRebuild: %v", err)
	}
	if got := requestedKeep(t, f, id, "cut"); !reflect.DeepEqual(got, []chapters.Range{{StartMs: 0, EndMs: 10_010}, {StartMs: 15_015, EndMs: 20_000}}) {
		t.Fatalf("requested keep = %v, want [[0,10010) [15015,20000)]", got)
	}

	unownedID := f.NewRecording(t)
	f.NewAsset(t, unownedID, "original", nil)
	f.SetDesired(t, unownedID, []string{"cut"})
	if err := f.Policy.RequestCutRebuild(ctx, unownedID, "cut"); !errors.Is(err, store.ErrChaptersNotOwned) {
		t.Fatalf("RequestCutRebuild(unowned chapters) error = %v, want ErrChaptersNotOwned", err)
	}
}

func runCutRebuildOnTrashedRecording(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	f.SetDesired(t, id, []string{"cut"})
	f.SetChapterSpans(t, id, 20_000, []chapters.Span{
		{StartMs: 0, EndMs: 10_000},
		{StartMs: 10_000, EndMs: 15_000, Cut: true},
	})
	f.SetRecordingTrashed(t, id)

	if err := f.Policy.RequestCutRebuild(ctx, id, "cut"); err != nil {
		t.Fatalf("RequestCutRebuild for trashed recording: %v", err)
	}
	if got := requestedKeep(t, f, id, "cut"); !reflect.DeepEqual(got, []chapters.Range{{StartMs: 0, EndMs: 10_010}, {StartMs: 15_015, EndMs: 20_000}}) {
		t.Fatalf("requested keep for trashed recording = %v, want [[0,10010) [15015,20000)]", got)
	}
}

func runCutRebuildAfterChapterEdit(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	f.SetDesired(t, id, []string{"cut"})
	oldSpans := []chapters.Span{
		{StartMs: 0, EndMs: 10_000},
		{StartMs: 10_000, EndMs: 15_000, Cut: true},
	}
	f.SetChapterSpans(t, id, 20_000, oldSpans)
	newSpans := []chapters.Span{
		{StartMs: 0, EndMs: 5_000},
		{StartMs: 5_000, EndMs: 25_000, Cut: true},
	}
	commitChapterEdit := f.BeginChapterEdit(t, id, 25_000, newSpans)

	finished := make(chan error, 1)
	go func() { finished <- f.Policy.RequestCutRebuild(ctx, id, "cut") }()
	f.WaitRecordingLock(t)
	commitChapterEdit()
	if err := <-finished; err != nil {
		t.Fatalf("RequestCutRebuild after chapter edit: %v", err)
	}
	if got := requestedKeep(t, f, id, "cut"); !reflect.DeepEqual(got, []chapters.Range{{StartMs: 0, EndMs: 5_005}}) {
		t.Fatalf("requested keep = %v, want [[0,5005)] from committed chapter edit", got)
	}
}

func runSetPolicy(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	f.NewAsset(t, id, "original", nil)
	f.SetDesired(t, id, []string{"h264"})
	keep := "until_encoded"
	enabled := true
	if err := f.Policy.SetPolicy(ctx, id, store.EncodePolicyUpdate{KeepOriginal: &keep, CMDetect: &enabled}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	row := encodePolicyRow(t, f, id)
	if row.KeepOriginal != keep || !sameStrings(row.EncodeProfiles, []string{"h264"}) {
		t.Fatalf("RecordingQuery.Get(%d).KeepOriginal/EncodeProfiles after SetPolicy = (%q, %v), want %q/[h264]", id, row.KeepOriginal, row.EncodeProfiles, keep)
	}
	desired, err := f.CMDetection.IsDetectionDesired(ctx, id)
	if err != nil || !desired {
		t.Fatalf("IsDetectionDesired after SetPolicy = (%t, %v), want true", desired, err)
	}
	attempt, shouldRun, err := f.CMDetection.BeginAttempt(ctx, id, 1)
	if err != nil || !shouldRun || attempt.PolicyGeneration != 1 {
		t.Fatalf("BeginAttempt after SetPolicy = (generation=%d, shouldRun=%t, err=%v), want generation 1 and run", attempt.PolicyGeneration, shouldRun, err)
	}

	withoutOriginalID := f.NewRecording(t)
	f.SetDesired(t, withoutOriginalID, []string{"h264"})
	if err := f.Policy.SetPolicy(ctx, withoutOriginalID, store.EncodePolicyUpdate{KeepOriginal: &keep, CMDetect: &enabled}); !errors.Is(err, store.ErrOriginalUnavailable) {
		t.Fatalf("SetPolicy(without original) error = %v, want ErrOriginalUnavailable", err)
	}
	// Get は行が無いときも KeepOriginal を "always"、EncodeProfiles を空に補うので、
	// 行が残っていることは SetDesired で入れた EncodeProfiles で確かめる。
	rolledBack := encodePolicyRow(t, f, withoutOriginalID)
	if rolledBack.KeepOriginal != "always" {
		t.Fatalf("RecordingQuery.Get(%d).KeepOriginal after failed combined update = %q, want rollback to %q", withoutOriginalID, rolledBack.KeepOriginal, "always")
	}
	if !sameStrings(rolledBack.EncodeProfiles, []string{"h264"}) {
		t.Fatalf("RecordingQuery.Get(%d).EncodeProfiles after failed combined update = %v, want %v kept", withoutOriginalID, rolledBack.EncodeProfiles, []string{"h264"})
	}
	unfrozenID := f.NewRecording(t)
	if err := f.Policy.SetPolicy(ctx, unfrozenID, store.EncodePolicyUpdate{KeepOriginal: &keep}); !errors.Is(err, store.ErrEncodeProfilesRequired) {
		t.Fatalf("SetPolicy(unfrozen until_encoded) error = %v, want ErrEncodeProfilesRequired", err)
	}
	if err := f.Policy.SetPolicy(ctx, -1, store.EncodePolicyUpdate{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetPolicy(missing) error = %v, want ErrNotFound", err)
	}
}

func runAttemptLimit(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	for want := int32(1); want <= 2; want++ {
		start, err := f.Attempts.Begin(ctx, id, "h264", 2)
		if err != nil {
			t.Fatalf("Begin attempt %d: %v", want, err)
		}
		if start.Count != want || start.Terminal {
			t.Fatalf("Begin attempt %d = %+v", want, start)
		}
		updated, err := f.Attempts.Fail(ctx, id, "h264", start.Count, "failure")
		if err != nil || !updated {
			t.Fatalf("Fail attempt %d = (%v, %v), want updated", want, updated, err)
		}
	}
	terminal, err := f.Attempts.Begin(ctx, id, "h264", 2)
	if err != nil {
		t.Fatalf("Begin after limit: %v", err)
	}
	if !terminal.Terminal || terminal.Count != 2 {
		t.Fatalf("Begin after limit = %+v, want terminal count 2", terminal)
	}
	if state, exists := encodeAttemptState(t, f, id, "h264"); !exists || state != "failed" {
		t.Fatalf("RecordingQuery.Get(%d).EncodeAttempts for h264 after limit = (%q, %v), want failed", id, state, exists)
	}
}

func runAttemptFencing(t *testing.T, newFixture NewEncodePolicyFixture) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.NewRecording(t)
	first, err := f.Attempts.Begin(ctx, id, "h264", 3)
	if err != nil || first.Count != 1 || first.Terminal {
		t.Fatalf("first Begin = %+v, %v", first, err)
	}
	second, err := f.Attempts.Begin(ctx, id, "h264", 3)
	if err != nil || second.Count != 2 || second.Terminal {
		t.Fatalf("second Begin = %+v, %v", second, err)
	}
	if updated, err := f.Attempts.Fail(ctx, id, "h264", first.Count, "stale failure"); err != nil || updated {
		t.Fatalf("stale Fail = (%v, %v), want false without error", updated, err)
	}
	if err := f.Attempts.Restore(ctx, id, "h264", first); err != nil {
		t.Fatalf("stale Restore: %v", err)
	}
	if err := f.Attempts.Clear(ctx, id, "h264", first.Count); err != nil {
		t.Fatalf("stale Clear: %v", err)
	}
	state, exists := encodeAttemptState(t, f, id, "h264")
	if !exists || state != "running" {
		t.Fatalf("RecordingQuery.Get(%d).EncodeAttempts for h264 after stale writes = (%q, %v), want running", id, state, exists)
	}
	third, err := f.Attempts.Begin(ctx, id, "h264", 3)
	if err != nil || third.Terminal || third.Count != 3 || third.DeadAttempt != 2 {
		t.Fatalf("Begin after stale writes: Terminal = %t, Count = %d, DeadAttempt = %d, err = %v; want Terminal = false, Count = 3, DeadAttempt = 2, err = nil",
			third.Terminal, third.Count, third.DeadAttempt, err)
	}
}

func encodePolicyRow(t *testing.T, f EncodePolicyFixture, recordingID int64) store.RecordingQueryRow {
	t.Helper()
	row, err := f.Recordings.Get(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("RecordingQuery.Get(%d).KeepOriginal/EncodeProfiles: %v", recordingID, err)
	}
	return row
}

func encodeAttemptState(t *testing.T, f EncodePolicyFixture, recordingID int64, profile string) (string, bool) {
	t.Helper()
	row := encodePolicyRow(t, f, recordingID)
	var attempts []struct {
		Profile string `json:"profile"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal(row.EncodeAttempts, &attempts); err != nil {
		t.Fatalf("decoding RecordingQuery.Get(%d).EncodeAttempts: %v", recordingID, err)
	}
	for _, attempt := range attempts {
		if attempt.Profile == profile {
			return attempt.State, true
		}
	}
	return "", false
}

func removedEncodedAssetListed(t *testing.T, f EncodePolicyFixture, assetID int64) bool {
	t.Helper()
	assets, err := f.Removals.ListRemovedEncodedAssetsToDelete(context.Background(), 100)
	if err != nil {
		t.Fatalf("ListRemovedEncodedAssetsToDelete: %v", err)
	}
	for _, asset := range assets {
		if asset.ID == assetID {
			return true
		}
	}
	return false
}

func requestedKeep(t *testing.T, f EncodePolicyFixture, recordingID int64, profile string) []chapters.Range {
	t.Helper()
	state, err := f.Reconcile.GetEncodedPublishState(context.Background(), recordingID, profile, true, false)
	if err != nil {
		t.Fatalf("GetEncodedPublishState(%d, %q): %v", recordingID, profile, err)
	}
	if !state.HasRebuildRequest {
		t.Fatalf("GetEncodedPublishState(%d, %q).RebuildRequestKeep has no rebuild request", recordingID, profile)
	}
	return state.RebuildRequestKeep
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, value := range got {
		seen[value]++
	}
	for _, value := range want {
		seen[value]--
		if seen[value] < 0 {
			return false
		}
	}
	return true
}
