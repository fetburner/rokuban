package epgstation

import "time"

// Reserve は GET /api/reserves のレスポンス要素のうち、M2-14 のシャドー差分に
// 必要なフィールドだけを持つ。EPGStation の完全なレスポンスにはこの他にも
// programId 由来のチャンネル情報などが含まれるが、差分照合には使わない。
type Reserve struct {
	// ID は EPGStation 内部の予約 id（Rokuban の予約とは別体系）。
	ID int64 `json:"id"`
	// RuleID はルール予約の場合のみ入る。手動予約では nil。
	RuleID *int64 `json:"ruleId,omitempty"`
	// IsSkip は EPGStation 側でユーザーが除外した予約であることを示す。
	IsSkip bool `json:"isSkip"`
	// IsConflict はチューナー競合で録画されない予約であることを示す。
	IsConflict bool `json:"isConflict"`
	// IsOverlap は EPGStation の重複排除ロジックで除外された予約であることを示す。
	IsOverlap bool `json:"isOverlap"`
	// IsTimeSpecified は時刻指定予約（programId を持たない）であることを示す。
	IsTimeSpecified bool `json:"isTimeSpecified"`
	// ProgramID は mirakc の programId。時刻指定予約では欠ける。
	ProgramID *int64 `json:"programId,omitempty"`
	// StartAt / EndAt は UnixtimeMS。
	StartAt int64  `json:"startAt"`
	EndAt   int64  `json:"endAt"`
	Name    string `json:"name"`
}

// StartAtTime は StartAt (UnixtimeMS) を time.Time に変換する。
func (r Reserve) StartAtTime() time.Time {
	return time.UnixMilli(r.StartAt)
}

// EndAtTime は EndAt (UnixtimeMS) を time.Time に変換する。
func (r Reserve) EndAtTime() time.Time {
	return time.UnixMilli(r.EndAt)
}

// reservesResponse は GET /api/reserves のレスポンス全体。
// total はページング終了判定に使う（limit/offset で全件を回収する）。
type reservesResponse struct {
	Reserves []Reserve `json:"reserves"`
	Total    int       `json:"total"`
}
