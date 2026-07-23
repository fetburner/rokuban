package mirakc

import (
	"fmt"
	"strconv"
	"strings"
)

const tagPrefix = "rokuban:reservation="

// ReservationTag は rokuban の reservation ID を mirakc の tag 形式に変換する。
func ReservationTag(reservationID int64) string {
	return fmt.Sprintf("%s%d", tagPrefix, reservationID)
}

// ParseReservationTag は tag 文字列から reservation ID を抽出する。
// rokuban のタグでなければ 0, false を返す。
func ParseReservationTag(tag string) (int64, bool) {
	if !strings.HasPrefix(tag, tagPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(tag[len(tagPrefix):], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// FindReservationID は tags スライスから rokuban の reservation ID を探す。
func FindReservationID(tags []string) (int64, bool) {
	for _, tag := range tags {
		if id, ok := ParseReservationTag(tag); ok {
			return id, true
		}
	}
	return 0, false
}
