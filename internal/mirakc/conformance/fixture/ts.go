//go:build conformance

// Package fixture は conformance テスト専用の MPEG-TS / ARIB SI 合成器である。
//
// **不変条件 6 の境界**: あの条件は「製品コードが TS を解釈しない」ことを守るためのもので、
// フィクスチャの生成（逆方向: 構造化データから TS バイト列を組み立てる）はその対象外である。
// ただし混同を避けるため、このパッケージは internal/mirakc/conformance/ の配下に閉じ、
// //go:build conformance タグの外（`go build ./...` の既定）からは一切コンパイルされない。
// 製品コード（internal/tsstat 等）から import できる場所には置かない。
package fixture

import "time"

// packetSize は MPEG-TS パケットの固定長。
const packetSize = 188

// packetizeSection は 1 つのセクション（CRC32 まで含む）を PID 宛ての TS パケット列に分割する。
// 先頭パケットのみ payload_unit_start_indicator を立て、pointer_field（0x00）を前置する。
// 末尾は 0xFF でスタッフィングする。adaptation_field は使わない（payload のみ）。
func packetizeSection(pid uint16, section []byte, cc *uint8) []byte {
	var out []byte
	data := section
	first := true
	for first || len(data) > 0 {
		pkt := make([]byte, packetSize)
		pkt[0] = 0x47
		pusi := byte(0)
		if first {
			pusi = 0x40
		}
		pkt[1] = pusi | byte(pid>>8&0x1F)
		pkt[2] = byte(pid & 0xFF)
		pkt[3] = 0x10 | (*cc & 0x0F) // adaptation_field_control=01（payload のみ）
		*cc = (*cc + 1) & 0x0F

		payload := pkt[4:]
		n := 0
		if first {
			payload[0] = 0x00 // pointer_field
			n = 1
			first = false
		}
		avail := len(payload) - n
		take := len(data)
		if take > avail {
			take = avail
		}
		copy(payload[n:], data[:take])
		n += take
		data = data[take:]
		for i := n; i < len(payload); i++ {
			payload[i] = 0xFF
		}
		out = append(out, pkt...)
	}
	return out
}

// pcrUpperBound は PCR の周期（27MHz 換算の全体値 base*300+extension が取りうる最大値+1）。
// mirakc-arib の kPcrUpperBound（tsduck_helper.hh）と同じ定義: ((2^33-1)*300+299)+1 = 2^33*300。
const pcrUpperBound = int64(1) << 33 * 300

// pcrPacket は adaptation_field のみ（payload なし）で PCR を運ぶ TS パケットを 1 つ作る。
// payload を持たないパケットは continuity_counter を進めない（規格どおり）ので引数に取らない。
//
// totalTicks は 27MHz 換算の PCR 全体値（program_clock_reference_base（90kHz, 33bit）*300 +
// program_clock_reference_extension（27MHz, 9bit））。base と extension に分解して書く —
// **base に 27MHz の値をそのまま書くと 300 倍速の PCR になる**（実際にこれで最初の実装が
// filter-program の start/end PCR を 1 秒未満で通過してしまい、mirakc 実物での検証で見つけた）。
func pcrPacket(pid uint16, totalTicks int64) []byte {
	pkt := make([]byte, packetSize)
	pkt[0] = 0x47
	pkt[1] = byte(pid >> 8 & 0x1F)
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x20 // adaptation_field_control=10（adaptation のみ）
	pkt[4] = 183  // adaptation_field_length（このバイト以降 183 バイト）
	pkt[5] = 0x10 // PCR_flag のみ立てる

	base := totalTicks / 300 % (1 << 33)
	ext := totalTicks % 300
	pkt[6] = byte(base >> 25)
	pkt[7] = byte(base >> 17)
	pkt[8] = byte(base >> 9)
	pkt[9] = byte(base >> 1)
	pkt[10] = byte((base&1)<<7) | 0x7E | byte(ext>>8&0x01)
	pkt[11] = byte(ext & 0xFF)
	for i := 12; i < packetSize; i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

// pcrTicksFromTime は絶対時刻を 27MHz 換算の PCR 全体値に変換する。Unix epoch を基準にする
// ことで、**別プロセスとして起動された mirakc-arib の呼び出し（EPG 収集ジョブ・実際の録画の
// それぞれが偽チューナーを別プロセスとして起動する）の間でも PCR が壁時計に対して一貫する**
// （sync-clocks が観測した (pcr, time) の組を、後から開始する別プロセスの録画にそのまま
// 使い回せる）。
func pcrTicksFromTime(t time.Time) int64 {
	return t.UnixMilli() * 27000 % pcrUpperBound
}
