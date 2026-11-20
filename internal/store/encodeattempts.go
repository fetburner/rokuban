package store

import (
	"context"
	"time"
)

// EncodeAttemptStart は encode の次の fencing token と、取り消し時に戻す前の状態を表す。
type EncodeAttemptStart struct {
	Count int32
	// DeadAttempt は直前の行が running のまま残っていたときの、その試行の番号（0 なら無い）。
	// Begin はその試行を failed として数えてから Count を出す。
	DeadAttempt         int32
	Terminal            bool
	RestoreState        string
	RestoreError        *string
	RestoreAttemptedAt  time.Time
	RestoreAttemptCount int32
}

// EncodeAttempts は encode のドメイン試行状態を永続化する。
type EncodeAttempts interface {
	// Begin はエンコードの試行を開始し、上限に達した場合は Terminal を返す。
	// 同じ (recording, profile) の Begin どうしを直列化する。
	// 前の試行が running のまま残っていれば failed として数えてから次の token を出す。
	// 行が無ければ作り、あれば先に行をロックしてから、上限の判定・停止した試行の計数・token の記録を 1 tx で行う（契約テストでは未検証）。
	// attempted_at は DB の時計で付ける（契約テストでは未検証）。
	// 上限 limit は呼び出し側が渡し、store は判定に使うだけである。検査: `RunEncodePolicy` の「attempt begin enforces the supplied limit」。
	Begin(ctx context.Context, recordingID int64, profile string, limit int32) (EncodeAttemptStart, error)
	// Fail は token が一致する実行中の試行だけを失敗にし、更新できたかを返す。古い token では何も書かない。
	Fail(ctx context.Context, recordingID int64, profile string, attempt int32, message string) (bool, error)
	// Restore は token が一致する行だけを開始前の状態へ戻す。古い token では何も書かない。
	Restore(ctx context.Context, recordingID int64, profile string, start EncodeAttemptStart) error
	// Clear は token が一致する実行中の試行だけを消す。古い token では何も書かない。
	Clear(ctx context.Context, recordingID int64, profile string, attempt int32) error
}
