package store

import (
	"context"
	"time"
)

// RecordingTrash は録画の論理削除、復元、即時削除要求を永続化する。
// 各メソッドは自身のトランザクションを完了してから返す。
type RecordingTrash interface {
	// SoftDelete は録画を論理削除する。既にごみ箱にある場合も成功し、対象が無ければ ErrNotFound を返す。
	SoftDelete(ctx context.Context, recordingID int64) error

	// Restore は録画を復元し、同じ録画の即時削除要求を取り下げる。
	// 同一放送イベントの生きた録画と衝突する場合は ErrConflict、
	// ごみ箱に無い場合または完全削除済みの場合は ErrNotFound を返す。
	// 同じ録画の復元と即時削除要求は直列化される。
	// 要求が先に確定すれば、復元がそれを取り下げる。
	// 復元が先に確定すれば、後から来た要求は録画をごみ箱に戻して残る。
	// どちらの順でも、復元された録画に要求だけが残ることはない。
	Restore(ctx context.Context, recordingID int64) error

	// RequestPurge は即時削除を要求し、まだごみ箱に無ければ論理削除も行う。
	// 対象が無ければ ErrNotFound を返す。
	// 同じ録画の復元と即時削除要求は直列化される（保証の内容は Restore を参照）。
	RequestPurge(ctx context.Context, recordingID int64) error
}

// RecordingPurge は物理削除が終わった録画を tombstone として確定する。
type RecordingPurge interface {
	// MarkPurged は猶予の cutoff を過ぎ、削除対象アセットが残っていない録画を
	// 完全削除済みにする。初めて確定した録画だけを返し、繰り返し呼んでも結果は変わらない。
	MarkPurged(ctx context.Context, graceCutoff time.Time) ([]PurgedRecording, error)
}

// PurgedRecording は今回の呼び出しで完全削除を確定した録画の通知情報を持つ。
type PurgedRecording struct {
	ID    int64
	Site  string
	Title string
}
