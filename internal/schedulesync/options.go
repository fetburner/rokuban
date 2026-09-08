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

	diff.Priority = observed.Priority != EffectivePriority(defaultPriority, desired)
	tagProgramID, _ := mirakc.FindProgramTag(tags)
	diff.Tag = tagProgramID != programID

	wantContentPath, explicit := ExplicitContentPath(desired)
	var observedContentPath string
	if observed.ContentPath != nil {
		observedContentPath = *observed.ContentPath
	}
	diff.ContentPath = explicit && observedContentPath != wantContentPath

	return diff, true
}

// ExplicitContentPath はユーザーが overrides.contentPath に明示指定した値を
// サニタイズ済みで返す。ok=false は「明示指定が無い」= テンプレート生成に委ねる。
// filenameTemplate の展開結果は明示指定ではないので false になる。
//
// effective の ContentPath が非 nil であることが「ユーザーが書いた」と同値である
// のは、reservations.base に contentPath を載せる書き手が存在しないため
// （ruler の computeBase が意図的に除外している）。ruler が base に contentPath を
// 載せるようになったらこの同値が崩れ、テンプレート生成値が差分対象に混ざって
// #19 が潰した churn が戻る。
func ExplicitContentPath(opts reservation.Options) (string, bool) {
	if opts.ContentPath == nil || *opts.ContentPath == "" {
		return "", false
	}
	return contentpath.SanitizeContentPath(*opts.ContentPath), true
}

// ProgramEnded は番組終了時刻が now より前かを返す。
// 終了時刻ちょうどは「終了していない」側に倒し、reconciler の programEnded と
// 同じ境界を共有する。presync は終了済み番組を未同期として警告してはならない。
func ProgramEnded(startAt time.Time, durationMs int64, now time.Time) bool {
	endAt := startAt.Add(time.Duration(durationMs) * time.Millisecond)
	return endAt.Before(now)
}

// EffectivePriority は mirakc に送る priority を決める: opts.Priority が
// あればそれ、なければ defaultPriority。初回作成（createSchedule）と予約
// オプション差分反映の再作成（recreateSchedule）の両方から呼ばれる必要が
// あるため、この 1 箇所に抽出してある。同じ式を 2 箇所に書き下すと、片方だけ
// 直してもう片方を直し忘れる事故が起きる。collector（CompareOptions 経由）も
// 同じ値で observed options を比較するため、二重に定義しない。
func EffectivePriority(defaultPriority int, opts reservation.Options) int {
	if opts.Priority != nil {
		return *opts.Priority
	}
	return defaultPriority
}

// IsRecreateAllowed は schedule の state が、予約オプション差分を反映する
// DELETE→POST の再作成を安全に実行できる状態かを返す。
//
// mirakc が将来 state を追加しても、未知の値は安全側（再作成しない）に倒す。
// reconciler と presync collector は同じ allowlist を使い、片方だけが「直せる
// 差分」と判定することを防ぐ。
func IsRecreateAllowed(state string) bool {
	return state == mirakc.ScheduleStateScheduled
}
