package mirakc

import (
	"fmt"
	"strconv"
	"strings"
)

// oldReservationTagPrefix は M3-1 より前に焼いていた旧タグ形式
// （rokuban:reservation=<reservationId>）。reservations.id は ruler の導出削除で
// 再実体化すると別の値になるため、この形式は「自分が作った schedule か」の判定
// （IsOurs）にしか使わない。programId を読む用途では新形式だけを解決する
// （#53「導出器が作るキーを宛先にしない」）。
const oldReservationTagPrefix = "rokuban:reservation="

// programTagPrefix は mirakc の schedule に焼く tag の形式。programId は EPG に
// ある間ずっと安定しており、reservations.id のような再実体化での変化がない（#53）。
// site は含めない — site は mirakc インスタンス自身を指すので、その mirakc に
// 焼く tag では冗長（#53 の決定）。
const programTagPrefix = "program:"

// ProgramTag は programId を mirakc の tag 形式に変換する。
func ProgramTag(programID int64) string {
	return fmt.Sprintf("%s%d", programTagPrefix, programID)
}

// ParseProgramTag は新形式（program:{programId}）の tag から programId を抽出する。
// 旧形式（rokuban:reservation=）は意図的に解決しない。呼び出し元
// （reconciler.recreateChanged）はこれを「新形式でない」として再作成の契機にし、
// 既存の DELETE→POST 機構がレベルトリガーで移行を完了させる（#53）。
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

// FindProgramTag は tags スライスから新形式の programId を探す。
func FindProgramTag(tags []string) (int64, bool) {
	for _, tag := range tags {
		if id, ok := ParseProgramTag(tag); ok {
			return id, true
		}
	}
	return 0, false
}

// IsOurs は tags に rokuban が焼いた tag（新旧いずれかの形式）があるかを返す。
// 「自分が作った schedule か」の判定にのみ使う（reconciler の削除対象ループ）。
// 新旧両方を認識するのは、タグ形式の移行中に旧形式の schedule を「外部産」と
// 誤認して削除対象から取りこぼさないため（#53）。
func IsOurs(tags []string) bool {
	if _, ok := FindProgramTag(tags); ok {
		return true
	}
	for _, tag := range tags {
		if strings.HasPrefix(tag, oldReservationTagPrefix) {
			return true
		}
	}
	return false
}
