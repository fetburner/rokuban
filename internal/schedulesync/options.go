// Package schedulesync は、予約同期が共有する schedule の比較規則を持つ。
//
// reconciler の外部 API 呼び出しと metrics の DB collector がそれぞれ比較を
// 書き下すと、どちらかだけが priority / tag / contentPath の変更を見逃して
// 「同期済み」と誤報告できる。このパッケージには副作用のない判定だけを置く。
package schedulesync

import (
	"time"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
)

// DefaultPriority は priority override が無い予約に reconciler が送る既定値。
// collector も同じ値で observed options を比較するため、二重に定義しない。
const DefaultPriority = 10

// OptionsDiff は desired な予約と observed schedule の実効オプション差分。
//
// 差分の対象は reconciler の再作成規則と同じく priority、Rokuban の program
// tag、明示指定された contentPath だけ。テンプレートから生成した path は
// EPG の題名変更のたびに schedule を再作成する churn を避けるため比較しない。
type OptionsDiff struct {
	Priority    bool
	Tag         bool
	ContentPath bool
}

// Any はいずれかの比較対象に差分があるかを返す。
func (d OptionsDiff) Any() bool {
	return d.Priority || d.Tag || d.ContentPath
}

// CompareOptions は observed schedule が desired reservation と一致するかを
// 判定する。owned=false は schedule に Rokuban の program tag がなく、Rokuban
// が所有していないため比較対象外だったことを表す。
//
// reconciler は owned=false の schedule を触らない。この判定を metrics 側でも
// 共有し、外部 schedule を options 不一致として「直せる差分」に数えない。
func CompareOptions(
	programID int64,
	desired reservation.Options,
	defaultPriority int,
	observed mirakc.Options,
	tags []string,
) (diff OptionsDiff, owned bool) {
	if !mirakc.IsOurs(tags) {
		return OptionsDiff{}, false
	}

	diff.Priority = observed.Priority != effectivePriority(defaultPriority, desired)
	tagProgramID, _ := mirakc.FindProgramTag(tags)
	diff.Tag = tagProgramID != programID

	wantContentPath, explicit := ExplicitContentPath(desired)
	if observed.ContentPath != nil && explicit {
		diff.ContentPath = *observed.ContentPath != wantContentPath
	} else if explicit {
		// nil は mirakc が返した observed schedule に path が無い状態。
		// desired は明示指定を持つので不一致として扱う。
		diff.ContentPath = true
	}

	return diff, true
}

// ExplicitContentPath は overrides.contentPath の明示値をサニタイズして返す。
// filenameTemplate の展開結果は明示指定ではないので false になる。
func ExplicitContentPath(opts reservation.Options) (string, bool) {
	if opts.ContentPath == nil || *opts.ContentPath == "" {
		return "", false
	}
	return contentpath.SanitizeContentPath(*opts.ContentPath), true
}

// ProgramActiveAt は番組終了時刻が now より前ではないかを返す。
// 終了時刻ちょうどは active 側に倒し、reconciler の programEnded と同じ境界を
// 共有する。presync は終了済み番組を未同期として警告してはならない。
func ProgramActiveAt(startAt time.Time, durationMs int64, now time.Time) bool {
	endAt := startAt.Add(time.Duration(durationMs) * time.Millisecond)
	return !endAt.Before(now)
}

func effectivePriority(defaultPriority int, opts reservation.Options) int {
	if opts.Priority != nil {
		return *opts.Priority
	}
	return defaultPriority
}
