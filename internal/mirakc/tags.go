package mirakc

import (
	"fmt"
	"strconv"
	"strings"
)

// programTagPrefix は mirakc の schedule に焼く tag の形式。programId は EPG に
// ある間ずっと安定しており、予約行の導出キーのように再実体化で変化しない（#53）。
// site は含めない — site は mirakc インスタンス自身を指すので、その mirakc に
// 焼く tag では冗長（#53 の決定）。
const programTagPrefix = "program:"

// ProgramTag は programId を mirakc の tag 形式に変換する。
func ProgramTag(programID int64) string {
	return fmt.Sprintf("%s%d", programTagPrefix, programID)
}

// ParseProgramTag は tag（program:{programId}）から programId を抽出する。
func ParseProgramTag(tag string) (int64, bool) {
	if !strings.HasPrefix(tag, programTagPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(tag[len(programTagPrefix):], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// FindProgramTag は tags スライスから programId を探す。
func FindProgramTag(tags []string) (int64, bool) {
	for _, tag := range tags {
		if id, ok := ParseProgramTag(tag); ok {
			return id, true
		}
	}
	return 0, false
}

// IsOurs は tags に rokuban が焼いた tag があるかを返す。
//
// **中身は FindProgramTag と同じだが、名前で残す。** 呼び出し側（reconciler の
// 削除ループと再作成ループ）が主張しているのは「programId が読める」ではなく
// 「自分が作った schedule だけ触る」という不変条件であり、mirakc に他人が
// 作った schedule が並んでいる前提でそれを消さないための境界そのものである。
func IsOurs(tags []string) bool {
	_, ok := FindProgramTag(tags)
	return ok
}
