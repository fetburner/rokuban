package store

import (
	"context"
	"time"
)

// EncodeAttemptStart は encode の次の fencing token と、取り消し時に戻す前の状態を表す。
type EncodeAttemptStart struct {
	Count               int32
	DeadAttempt         int32
	Terminal            bool
	RestoreState        string
	RestoreError        *string
	RestoreAttemptedAt  time.Time
	RestoreAttemptCount int32
}

// EncodeAttempts は encode のドメイン試行状態を永続化する。
type EncodeAttempts interface {
	// Begin は試行を開始し、上限に達した場合は Terminal を返す。
	Begin(ctx context.Context, recordingID int64, profile string, limit int32) (EncodeAttemptStart, error)
	// Fail は現在の fencing token が一致する実行中試行を失敗にし、更新できたかを返す。
	Fail(ctx context.Context, recordingID int64, profile string, attempt int32, message string) (bool, error)
	// Restore は現在の fencing token が一致するときだけ、開始前の状態へ戻す。
	Restore(ctx context.Context, recordingID int64, profile string, start EncodeAttemptStart) error
	// Clear は現在の fencing token が一致する実行中試行を消す。
	Clear(ctx context.Context, recordingID int64, profile string, attempt int32) error
}
