package store

import "context"

// PlaybackState は録画ごとの再生位置と、放送イベント単位の視聴済み印を永続化する。
// 各メソッドは自身のトランザクションを完了してから返す。
type PlaybackState interface {
	// SavePosition は録画の再生位置を保存する。
	// 録画が存在しない、または完全削除済みなら ErrNotFound を返す。
	SavePosition(ctx context.Context, recordingID, positionMs int64) error

	// ClearPosition は録画の再生位置を冪等に削除する。
	// 録画が存在しない、または完全削除済みなら ErrNotFound を返す。
	ClearPosition(ctx context.Context, recordingID int64) error

	// MarkWatched は録画に視聴済み印を付け、同じ録画の再生位置を原子的に消す。
	// 録画が存在しない、または完全削除済みなら ErrNotFound を返す。
	MarkWatched(ctx context.Context, recordingID int64) error

	// ClearWatchedForEvent は録画と同じ放送イベントに属する視聴済み印をすべて消す。
	// 録画が存在しない、または完全削除済みなら ErrNotFound を返す。
	ClearWatchedForEvent(ctx context.Context, recordingID int64) error
}
