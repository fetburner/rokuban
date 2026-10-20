package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
)

// ReservationRecord は予約行のうち永続化されている部分。
type ReservationRecord struct {
	Site                  string
	ProgramID             int64
	RuleID                *int64
	Base                  json.RawMessage
	CreatedAt             time.Time
	UpdatedAt             time.Time
	DedupMatchRecordingID *int64
	DedupSimilarity       *float32
}

// ProgramSnapshot は予約の読み取りが使う、番組スナップショットの永続化された値。
type ProgramSnapshot struct {
	Title       string
	StartAt     time.Time
	DurationMs  int64
	ChannelType string
	ServiceName string
}

// ReservationDetails は予約行に、番組スナップショットとユーザー入力（意図・上書き）を結合したもの。
type ReservationDetails struct {
	Reservation   ReservationRecord
	Snapshot      ProgramSnapshot
	Overrides     json.RawMessage
	IntentAction  *string
	NeverRecorded bool
	Series        *string
}

// ReservationOverlap は重なり判定の対象として返す予約の候補。
type ReservationOverlap struct {
	ProgramID    int64
	Title        string
	StartAt      time.Time
	DurationMs   int64
	Base         json.RawMessage
	Overrides    json.RawMessage
	IntentAction *string
}

// Reservations は利用者の録画意図・オプション上書きの書き込みと、予約の読み取りを担う。
// 書き込みメソッドは 1 メソッド = 1 トランザクションで、返る前に commit する。
//
// PutIntent と PatchOverrides は、同じ (site, programID) に対する書き込み同士を直列化する。
// 直列化は、EPG 射影にある番組と、射影から外れて凍結スナップショットだけが残る番組の
// どちらにも及ぶ。PatchOverrides は、直列化した後の既存の上書きにパッチをマージするので、
// 別フィールドの同時 PATCH は互いの値を消さない。
type Reservations interface {
	// PutIntent は番組のスナップショットを確保し、録画意図（record / skip）を書く。
	// 2 つは同じ tx で確定する。射影にも凍結スナップショットにも番組が無ければ ErrNotFound を返す。
	PutIntent(ctx context.Context, site string, programID int64, action string) error
	// DeleteIntent は番組の録画意図を消す。意図が無くても成功する。
	DeleteIntent(ctx context.Context, site string, programID int64) error
	// PatchOverrides は、直列化した後の既存の上書きと予約の base にパッチをマージし、
	// その実効値を検証して書く。マージ結果が空なら上書きの行を消す。
	// 実効値が不正なら *reservation.InvalidEffectiveOptionsError を、射影にも凍結スナップショットにも
	// 番組が無ければ ErrNotFound を返す。
	PatchOverrides(ctx context.Context, site string, programID int64, patch reservation.OverridesPatch) error
	// DeleteOverrides は番組の上書きをすべて消す。行が無くても成功する。
	DeleteOverrides(ctx context.Context, site string, programID int64) error
	// GetReservation は (site, programID) で予約を詳細つきで返す。無ければ ErrNotFound を返す。
	GetReservation(ctx context.Context, site string, programID int64) (ReservationDetails, error)
	// ListReservations は予約を詳細つきで、site と番組の開始時刻の順に返す。
	ListReservations(ctx context.Context) ([]ReservationDetails, error)
	// ListOverlaps は、EPG 射影の番組の放送時間帯（半開区間）と重なる予約の候補を返す。
	// 対象自身と欠測除外済みの予約は含まない。skip の判定は実効値が要るので呼び出し側が行う。
	// 射影に番組が無ければ ErrNotFound を返す。
	ListOverlaps(ctx context.Context, site string, programID int64) ([]ReservationOverlap, error)
}
