package store

import "context"

// PlaybackState は録画ごとの再生位置と、放送イベント単位の視聴済み印を永続化する。
// 各メソッドは自身のトランザクションを完了してから返す。
type PlaybackState interface {
	// SavePosition は録画の再生位置を保存する。
	// 録画が存在しない、または完全削除済みなら ErrNotFound を返す。
	// 検査: contracttest.RunPlayback の「position save and clear are idempotent」、「missing and purged recordings are not found」。
	SavePosition(ctx context.Context, recordingID, positionMs int64) error

	// ClearPosition は録画の再生位置を冪等に削除する。
	// 録画が存在しない、または完全削除済みなら ErrNotFound を返す。
	// 検査: contracttest.RunPlayback の「position save and clear are idempotent」、「missing and purged recordings are not found」。
	ClearPosition(ctx context.Context, recordingID int64) error

	// MarkWatched は録画に視聴済み印を付け、同じ録画の再生位置を原子的に消す。
	// 再生位置の削除が失敗したら、印の書き込みも巻き戻す。
	// 録画が存在しない、または完全削除済みなら ErrNotFound を返す。
	// 検査: contracttest.RunPlayback の「mark watched clears position atomically」、「mark watched rolls back when clearing position fails」、「missing and purged recordings are not found」。
	MarkWatched(ctx context.Context, recordingID int64) error

	// ClearWatchedForEvent は録画と同じ放送イベントの視聴済み印をすべて消す。
	// 読み取り側が印を放送イベント単位で束ねるため、対象は録画ではなくイベントの全行である。
	// 録画が存在しない、または完全削除済みなら ErrNotFound を返す。
	// 検査: contracttest.RunPlayback の「clear watched removes every marker for the event」、「missing and purged recordings are not found」。
	ClearWatchedForEvent(ctx context.Context, recordingID int64) error
}
