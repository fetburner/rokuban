//go:build conformance

package fixture

import "time"

// mjdEpoch は Modified Julian Date の起点（1858-11-17 00:00:00 UTC）。標準暦計算を
// 手で再導出せず time.Time の差分に委ねる（ponytail: stdlib で足りる計算を自前実装しない）。
var mjdEpoch = time.Date(1858, time.November, 17, 0, 0, 0, 0, time.UTC)

// jstNow は「TOT/EIT に埋め込む時刻」を返す。
//
// **ARIB の TOT/EIT の時刻フィールドは JST の暦をそのまま UTC 相当のバイト列として運ぶ**
// （mirakc-arib の tsduck_helper.hh: ConvertJstTimeToUnixTime は
// `jst_time - UnixEpoch - 9h` で真の UNIX 時刻に戻す。つまり格納する暦値は
// 「壁時計の JST 表示」をそのまま UTC の年月日時分秒として書いた値）。
// よってここでは実時刻に 9 時間を加算した time.Time を返し、その Year/Month/.../Second を
// そのまま MJD/BCD にエンコードする。
func jstNow() time.Time {
	return time.Now().UTC().Add(9 * time.Hour)
}

// mjd は暦日（年月日のみ）を Modified Julian Date に変換する。
func mjd(t time.Time) uint16 {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	days := int(d.Sub(mjdEpoch).Hours() / 24)
	return uint16(days)
}

// bcdByte は 0-99 の整数を BCD 1 バイトにエンコードする。
func bcdByte(v int) byte {
	return byte((v/10)<<4 | v%10)
}

// mjdBcdTime は ARIB の 5 バイト時刻表現（MJD 2 バイト + BCD 時分秒 3 バイト）を作る。
// t は jstNow() と同じ規約（JST の暦値を保持する time.Time）で渡す。
func mjdBcdTime(t time.Time) []byte {
	m := mjd(t)
	return []byte{byte(m >> 8), byte(m & 0xFF), bcdByte(t.Hour()), bcdByte(t.Minute()), bcdByte(t.Second())}
}

// bcdDuration は継続時間を ARIB の 3 バイト BCD（時分秒）にエンコードする。
func bcdDuration(d time.Duration) []byte {
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return []byte{bcdByte(h), bcdByte(m), bcdByte(s)}
}
