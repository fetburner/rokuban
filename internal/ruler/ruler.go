// Package ruler はルール評価から reservations.base を導出する 1 パス評価ロジック。
//
// 1 パスで全ルールと全射影番組を評価し、変わった行だけを書く。永続化は store の契約に委ね、
// mirakc には触れない（不変条件 1）。
package ruler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

const maxRulerInputRevisionMismatches = 3

// beforeRulerWriteTx は入力を読んだ後、予約の書き込み tx を始める前にテストが割り込むための hook。
var beforeRulerWriteTx func()

// Config は Ruler の設定。
type Config struct {
	// MaxDeletesPerPass は 1 サイト・1 パスあたりの導出削除許容数。
	MaxDeletesPerPass int
	// RetentionGrace は番組終了後、予約と意図を GC するまでの猶予。
	RetentionGrace time.Duration
	// RetractGrace は放送開始直前にルールから外れた予約を削除しない猶予。0 は無効。
	// このパッケージは既定値を持たない。持たせると Config{} で動く既存のテストと
	// 呼び出し元が黙って猶予ありに変わる。本番の既定（1h）は internal/config が埋める。
	RetractGrace time.Duration
}

func defaultConfig() Config {
	return Config{MaxDeletesPerPass: 50, RetentionGrace: 24 * time.Hour}
}

// Ruler はルール評価から reservations.base を導出する 1 パス評価を行う。
type Ruler struct {
	sites          []string
	passes         store.RulerPass
	breakers       store.Breakers
	neverScheduled store.NeverScheduledEvents
	markers        store.LoopMarkers
	programSearch  store.ProgramSearch
	cfg            Config
}

// New は store 契約を使う Ruler を生成する。
func New(
	sites []string,
	passes store.RulerPass,
	breakers store.Breakers,
	neverScheduled store.NeverScheduledEvents,
	markers store.LoopMarkers,
	cfg *Config,
	programSearch store.ProgramSearch,
) *Ruler {
	c := defaultConfig()
	if cfg != nil {
		if cfg.MaxDeletesPerPass > 0 {
			c.MaxDeletesPerPass = cfg.MaxDeletesPerPass
		}
		if cfg.RetentionGrace > 0 {
			c.RetentionGrace = cfg.RetentionGrace
		}
		// RetractGrace は 0 も意味のある値（無効）なので、>0 のときだけ上書きにしない。
		c.RetractGrace = cfg.RetractGrace
	}
	return &Ruler{
		sites: sites, passes: passes, breakers: breakers, neverScheduled: neverScheduled,
		markers: markers, programSearch: programSearch, cfg: c,
	}
}

// RunPass は全サイトの全量評価を実行し、続けて番組終了後の GC を 1 回行う。
// 成功 marker は、その site の評価と GC の両方が成功したときだけ更新する。
func (r *Ruler) RunPass(ctx context.Context) error {
	if r.programSearch == nil {
		return fmt.Errorf("program search store is required")
	}
	start := time.Now()
	var firstErr error
	successfulSites := make([]string, 0, len(r.sites))
	for _, site := range r.sites {
		if err := r.runPassForSite(ctx, site); err != nil {
			slog.Error("ruler: pass failed for site", "site", site, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("site %s: %w", site, err)
			}
			continue
		}
		successfulSites = append(successfulSites, site)
	}

	gcErr := r.runGC(ctx)
	if gcErr != nil {
		slog.Error("ruler: GC failed", "err", gcErr)
		if firstErr == nil {
			firstErr = fmt.Errorf("gc: %w", gcErr)
		}
	}
	if gcErr == nil && len(successfulSites) > 0 {
		if err := r.markers.MarkRulerPasses(ctx, successfulSites); err != nil {
			slog.Error("ruler: marking successful passes failed", "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("marking successful passes: %w", err)
			}
		}
	}

	metrics.RulerPassDuration.Observe(time.Since(start).Seconds())
	if firstErr == nil {
		metrics.RulerLastPass.SetToCurrentTime()
	}
	return firstErr
}

// runPassForSite は 1 サイト分の評価と差分書き込みを行う。
func (r *Ruler) runPassForSite(ctx context.Context, site string) error {
	for mismatches := 0; ; {
		inputRevision, err := r.passes.GetRulerInputRevision(ctx, site)
		if err != nil {
			return fmt.Errorf("getting ruler input revision for site %q: %w", site, err)
		}
		if mismatches == 0 {
			// snapshot 追従（ApplyPass）より前に観測する。後だと追従が旧値を上書きして
			// 検出材料が消える。検出の失敗はパスを止めない。
			observeProgramIDReuses(ctx, r.passes, site, time.Now())
		}

		changed, err := r.runPassForSiteAttempt(ctx, site, inputRevision)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		mismatches++
		if mismatches >= maxRulerInputRevisionMismatches {
			return fmt.Errorf("ruler input changed during %d consecutive passes for site %q", mismatches, site)
		}
		slog.Info("ruler: inputs changed during pass; re-evaluating", "site", site, "retry", mismatches)
	}
}

func (r *Ruler) runPassForSiteAttempt(ctx context.Context, site string, inputRevision int64) (bool, error) {
	evaluationNow := time.Now()

	tripped, err := r.observeDeleteBreaker(ctx, site)
	if err != nil {
		return false, err
	}
	rules, err := r.passes.ListEnabled(ctx)
	if err != nil {
		return false, fmt.Errorf("listing enabled rules: %w", err)
	}
	ruleByID, winner, err := r.resolveWinners(ctx, site, rules)
	if err != nil {
		return false, err
	}
	desired, skipIntent, fulfilled, err := r.collectDesired(ctx, site, winner)
	if err != nil {
		return false, err
	}
	desiredIDs := make([]int64, 0, len(desired))
	for id := range desired {
		desiredIDs = append(desiredIDs, id)
	}
	existingSet, deleteCandidates, toDelete, err := r.collectDeleteCandidates(ctx, site, desired)
	if err != nil {
		return false, err
	}
	// fulfilled は専用経路でブレーカーの外から消すので、導出削除に数えない。
	toDelete = subtract(toDelete, fulfilled)

	dedupeMatches, err := r.applyDedupe(ctx, site, evaluationNow, desiredIDs, winner, ruleByID)
	if err != nil {
		return false, err
	}
	inputs, err := buildReservationInputs(desiredIDs, winner, ruleByID, dedupeMatches)
	if err != nil {
		return false, err
	}

	applyNow := time.Now()
	if beforeRulerWriteTx != nil {
		beforeRulerWriteTx()
	}
	result, err := r.passes.ApplyPass(ctx, site, store.RulerPassPlan{
		InputRevision:       &inputRevision,
		SnapshotProgramIDs:  snapshotProgramIDs(desiredIDs, skipIntent, existingSet),
		ExistingProgramIDs:  keys(existingSet),
		Reservations:        inputs,
		FulfilledProgramIDs: fulfilled,
		DeleteProgramIDs:    toDelete,
		Now:                 applyNow,
		RetractGrace:        r.cfg.RetractGrace,
		MaxDeletesPerPass:   r.cfg.MaxDeletesPerPass,
		BreakerName:         breaker.RulerDeletes,
		BreakerTripped:      tripped,
	})
	if err != nil {
		// 入力削除による外部キー違反などで ApplyPass が版照合まで進めない場合も、
		// ロールバック後に版を確認して、変更済みなら新しい入力で再試行する。
		currentRevision, revisionErr := r.passes.GetRulerInputRevision(ctx, site)
		if revisionErr != nil {
			return false, errors.Join(err, fmt.Errorf("getting ruler input revision after failed apply: %w", revisionErr))
		}
		if currentRevision != inputRevision {
			return true, nil
		}
		return false, err
	}
	if result.InputRevisionChanged {
		return true, nil
	}
	for _, id := range result.UnmaterializedProgramIDs {
		slog.Warn("ruler: cannot materialize reservation without a program snapshot", "site", site, "program_id", id)
	}
	if result.BreakerTripped {
		breaker.AnnounceTrip(site, breaker.RulerDeletes, r.cfg.MaxDeletesPerPass, result.BreakerSample, result.BreakerTrippedAt)
	}
	if result.NewlyTripped {
		metrics.RulerCircuitBreakerTrips.Inc()
	}
	if result.BreakerLatched {
		slog.Warn("ruler: circuit breaker latched — withholding derived deletes until manually resumed",
			"site", site, "breaker", breaker.RulerDeletes, "pending_deletes", len(toDelete)-len(result.ReleasedProgramIDs)-result.GraceProtected)
	}

	metrics.RulerReservations.WithLabelValues("created").Add(float64(result.Created))
	metrics.RulerReservations.WithLabelValues("updated").Add(float64(result.Updated))
	metrics.RulerReservations.WithLabelValues("deleted").Add(float64(result.Deleted))
	metrics.RulerReservations.WithLabelValues("released").Add(float64(len(result.ReleasedProgramIDs)))
	metrics.RulerReservations.WithLabelValues("fulfilled").Add(float64(result.FulfilledDeleted))

	slog.Info("ruler: pass complete",
		"site", site,
		"rules", len(rules),
		"desired", len(desired),
		"created", result.Created,
		"updated", result.Updated,
		"deleted", result.Deleted,
		"released", len(result.ReleasedProgramIDs),
		"fulfilled", result.FulfilledDeleted,
		"grace_protected", result.GraceProtected,
		"delete_candidates", len(deleteCandidates),
	)
	return false, nil
}

func observeProgramIDReuses(ctx context.Context, passes store.RulerPass, site string, now time.Time) {
	reuses, err := passes.ListProgramIDReuses(ctx, site, now)
	if err != nil {
		slog.Error("ruler: program_id reuse detection failed", "site", site, "err", err)
		return
	}
	for _, reuse := range reuses {
		slog.Warn("ruler: possible program_id reuse detected",
			"site", site,
			"program_id", reuse.ProgramID,
			"old_start_at", reuse.SnapshotStartAt,
			"new_start_at", reuse.ProjectionStartAt,
		)
		metrics.RulerProgramIDReuses.Inc()
	}
}

func (r *Ruler) observeDeleteBreaker(ctx context.Context, site string) (bool, error) {
	tripped, err := breaker.ObserveState(ctx, r.breakers, site, breaker.RulerDeletes)
	if err != nil {
		return false, fmt.Errorf("observing %s circuit breaker: %w", breaker.RulerDeletes, err)
	}
	return tripped, nil
}

func (r *Ruler) resolveWinners(ctx context.Context, site string, rules []store.Rule) (map[int64]store.Rule, map[int64]int64, error) {
	ruleByID := make(map[int64]store.Rule, len(rules))
	winner := make(map[int64]int64)
	for _, rule := range rules {
		ruleByID[rule.ID] = rule
		matched, err := r.programSearch.MatchProgramIDsForRule(ctx, site, rule.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("matching rule %d: %w", rule.ID, err)
		}
		for _, programID := range matched {
			if _, exists := winner[programID]; !exists {
				winner[programID] = rule.ID
			}
		}
	}
	return ruleByID, winner, nil
}

func (r *Ruler) collectDesired(ctx context.Context, site string, winner map[int64]int64) (map[int64]struct{}, map[int64]struct{}, []int64, error) {
	intents, err := r.passes.ListProgramIntents(ctx, site)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing program intents: %w", err)
	}
	skipIntent := make(map[int64]struct{})
	for _, intent := range intents {
		if intent.Action == reservation.IntentSkip {
			skipIntent[intent.ProgramID] = struct{}{}
		}
	}
	investments, err := r.passes.ListInvestmentProgramIDs(ctx, site)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing program investments: %w", err)
	}
	desired := make(map[int64]struct{}, len(winner)+len(investments))
	for programID := range winner {
		if _, skipped := skipIntent[programID]; !skipped {
			desired[programID] = struct{}{}
		}
	}
	for _, programID := range investments {
		desired[programID] = struct{}{}
	}
	fulfilled, err := r.passes.ListFulfilledProgramIDs(ctx, site)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing fulfilled reservations: %w", err)
	}
	for _, programID := range fulfilled {
		delete(desired, programID)
	}
	return desired, skipIntent, fulfilled, nil
}

func (r *Ruler) collectDeleteCandidates(ctx context.Context, site string, desired map[int64]struct{}) (map[int64]struct{}, []int64, []int64, error) {
	existingProgramIDs, err := r.passes.ListReservationProgramIDs(ctx, site)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing existing reservation program ids: %w", err)
	}
	existingSet := make(map[int64]struct{}, len(existingProgramIDs))
	for _, id := range existingProgramIDs {
		existingSet[id] = struct{}{}
	}
	var candidates []int64
	for id := range existingSet {
		if _, ok := desired[id]; !ok {
			candidates = append(candidates, id)
		}
	}
	projected, err := r.passes.ListProjectedProgramIDs(ctx, site, candidates)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("checking projected delete candidates: %w", err)
	}
	return existingSet, candidates, projected, nil
}

func (r *Ruler) applyDedupe(ctx context.Context, site string, now time.Time, desiredIDs []int64, winner map[int64]int64, ruleByID map[int64]store.Rule) (map[int64]store.RulerDedupeMatch, error) {
	var candidates []store.RulerDedupeCandidate
	for _, programID := range desiredIDs {
		ruleID, ok := winner[programID]
		if !ok || !ruleByID[ruleID].DedupeEnabled {
			continue
		}
		candidates = append(candidates, store.RulerDedupeCandidate{ProgramID: programID, RuleID: ruleID})
	}
	matches, err := r.passes.EvaluateDedupe(ctx, site, candidates, now)
	if err != nil {
		return nil, fmt.Errorf("evaluating dedupe: %w", err)
	}
	return matches, nil
}

func buildReservationInputs(desiredIDs []int64, winner map[int64]int64, ruleByID map[int64]store.Rule, dedupeMatches map[int64]store.RulerDedupeMatch) ([]store.RulerReservationInput, error) {
	inputs := make([]store.RulerReservationInput, 0, len(desiredIDs))
	for _, programID := range desiredIDs {
		var input store.RulerReservationInput
		input.ProgramID = programID
		if ruleID, ok := winner[programID]; ok {
			ruleIDCopy := ruleID
			input.RuleID = &ruleIDCopy
			// マッチしなければ dedup 根拠 2 列は nil のまま（前パスの根拠を残さない。不変条件 9）。
			match, matched := dedupeMatches[programID]
			if matched {
				recordingID, similarity := match.RecordingID, match.Similarity
				input.DedupMatchRecordingID = &recordingID
				input.DedupSimilarity = &similarity
			}
			base, err := computeBase(ruleByID[ruleID], matched)
			if err != nil {
				return nil, fmt.Errorf("computing base for rule %d: %w", ruleID, err)
			}
			input.Base = base
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

func snapshotProgramIDs(desiredIDs []int64, skipIntent, existing map[int64]struct{}) []int64 {
	ids := make([]int64, 0, len(desiredIDs)+len(skipIntent)+len(existing))
	seen := make(map[int64]struct{}, cap(ids))
	add := func(id int64) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, id := range desiredIDs {
		add(id)
	}
	for id := range skipIntent {
		add(id)
	}
	for id := range existing {
		add(id)
	}
	return ids
}

func keys(values map[int64]struct{}) []int64 {
	ids := make([]int64, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	return ids
}

// runGC は終了時刻に保持猶予を足した cutoff より古い番組と、古い欠測観測を削除する。
// GC は導出削除のブレーカー対象ではなく、ブレーカーが発動中でも実行する。
func (r *Ruler) runGC(ctx context.Context) error {
	cutoff := time.Now().Add(-r.cfg.RetentionGrace)
	deletedSnapshots, err := r.passes.PruneEndedSnapshots(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("deleting ended program snapshots: %w", err)
	}
	horizon := r.cfg.RetentionGrace + 30*24*time.Hour
	deletedNeverScheduledEvents, err := r.neverScheduled.PruneNeverScheduled(ctx, horizon)
	if err != nil {
		return fmt.Errorf("deleting stale never-scheduled events: %w", err)
	}
	metrics.RulerReservations.WithLabelValues("gc").Add(float64(deletedSnapshots))
	if deletedSnapshots > 0 || deletedNeverScheduledEvents > 0 {
		slog.Info("ruler: GC complete",
			"cutoff", cutoff,
			"deleted_program_snapshots", deletedSnapshots,
			"never_scheduled_horizon", horizon,
			"deleted_never_scheduled_events", deletedNeverScheduledEvents,
		)
	}
	return nil
}

func subtract(ids, removed []int64) []int64 {
	if len(removed) == 0 {
		return ids
	}
	removedSet := make(map[int64]struct{}, len(removed))
	for _, id := range removed {
		removedSet[id] = struct{}{}
	}
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := removedSet[id]; !ok {
			result = append(result, id)
		}
	}
	return result
}

// computeBase は勝者ルールから reservations.base に入れる予約オプションを組む。
func computeBase(rule store.Rule, dedupeSkip bool) (json.RawMessage, error) {
	priority := int(rule.Priority)
	keepOriginal := rule.KeepOriginal
	profiles := append([]string(nil), rule.EncodeProfiles...)
	opts := reservation.Options{
		Priority:       &priority,
		KeepOriginal:   &keepOriginal,
		EncodeProfiles: &profiles,
	}
	if rule.FilenameTemplate != "" {
		filenameTemplate := rule.FilenameTemplate
		opts.FilenameTemplate = &filenameTemplate
	}
	if dedupeSkip {
		skip := true
		opts.Skip = &skip
	}
	out, err := json.Marshal(opts)
	if err != nil {
		return nil, fmt.Errorf("marshalling base options: %w", err)
	}
	return out, nil
}
