//go:build conformance

package fixture

import "time"

// buildSection は PSI/SI の「長形式」セクション（section_syntax_indicator=1、
// version_number/current_next_indicator/section_number/last_section_number を持つ）を組み立てる。
// version は常に 0、current_next_indicator は常に 1（このフィクスチャはテーブルの版を
// 変えない）。
func buildSection(tableID byte, tableIDExt uint16, sectionNumber, lastSectionNumber byte, payload []byte) []byte {
	body := make([]byte, 0, 5+len(payload)+4)
	body = append(body, byte(tableIDExt>>8), byte(tableIDExt&0xFF))
	body = append(body, 0xC1) // reserved='11', version_number=00000, current_next_indicator=1
	body = append(body, sectionNumber, lastSectionNumber)
	body = append(body, payload...)

	sectionLength := len(body) + 4 // + CRC32
	full := make([]byte, 0, 3+len(body)+4)
	full = append(full, tableID)
	full = append(full, 0xB0|byte(sectionLength>>8&0x0F), byte(sectionLength&0xFF))
	full = append(full, body...)

	crc := crc32MPEG(full)
	full = append(full, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return full
}

// buildPAT は PAT（table_id=0x00）を組み立てる。program_number=0 のエントリで NIT の PID を、
// sid のエントリで pmtPID を指す。
func buildPAT(tsid, sid uint16, pmtPID, nitPID uint16) []byte {
	payload := []byte{
		0x00, 0x00, 0xE0 | byte(nitPID>>8&0x1F), byte(nitPID & 0xFF),
		byte(sid >> 8), byte(sid & 0xFF), 0xE0 | byte(pmtPID>>8&0x1F), byte(pmtPID & 0xFF),
	}
	return buildSection(0x00, tsid, 0, 0, payload)
}

// pmtStream は PMT のエレメンタリストリーム 1 本の宣言。
type pmtStream struct {
	StreamType byte
	PID        uint16
}

// buildPMT は PMT（table_id=0x02）を組み立てる。program_info / ES_info は常に空（不要）。
func buildPMT(sid, pcrPID uint16, streams []pmtStream) []byte {
	payload := []byte{0xE0 | byte(pcrPID>>8&0x1F), byte(pcrPID & 0xFF), 0xF0, 0x00}
	for _, s := range streams {
		payload = append(payload, s.StreamType, 0xE0|byte(s.PID>>8&0x1F), byte(s.PID&0xFF), 0xF0, 0x00)
	}
	return buildSection(0x02, sid, 0, 0, payload)
}

// buildSDT は SDT actual（table_id=0x42）を、サービス 1 件・service_descriptor（0x48）付きで組み立てる。
func buildSDT(tsid, onid, sid uint16, serviceType byte, serviceName string) []byte {
	name := []byte(serviceName)
	desc := []byte{0x48, byte(2 + len(name)), serviceType, 0x00, byte(len(name))}
	desc = append(desc, name...)
	descLen := len(desc)

	entry := []byte{
		byte(sid >> 8), byte(sid & 0xFF),
		0xFF,                                               // reserved_future_use=111111, EIT_schedule_flag=1, EIT_present_following_flag=1
		0x80 | byte(descLen>>8&0x0F), byte(descLen & 0xFF), // running_status=100(running), free_CA_mode=0
	}
	entry = append(entry, desc...)

	payload := []byte{byte(onid >> 8), byte(onid & 0xFF), 0xFF}
	payload = append(payload, entry...)
	return buildSection(0x42, tsid, 0, 0, payload)
}

// buildNIT は NIT actual（table_id=0x40）を、自局 1 トランスポートストリームぶんの
// エントリだけで組み立てる。ネットワーク記述子・トランスポート記述子はいずれも空。
func buildNIT(nid, tsid, onid uint16) []byte {
	tsLoop := []byte{
		byte(tsid >> 8), byte(tsid & 0xFF),
		byte(onid >> 8), byte(onid & 0xFF),
		0xF0, 0x00, // reserved=1111, transport_descriptors_length=0
	}
	payload := []byte{0xF0, 0x00} // reserved=1111, network_descriptors_length=0
	payload = append(payload, 0xF0|byte(len(tsLoop)>>8&0x0F), byte(len(tsLoop)&0xFF))
	payload = append(payload, tsLoop...)
	return buildSection(0x40, nid, 0, 0, payload)
}

// buildTOT は TOT（table_id=0x73）を組み立てる。TOT は section_syntax_indicator=0 の
// 「短形式」でありながら CRC32 を持つ規格上の例外なので buildSection は使わない。
func buildTOT(t time.Time) []byte {
	body := mjdBcdTime(t)
	body = append(body, 0xF0, 0x00) // reserved=1111, descriptors_loop_length=0

	sectionLength := len(body) + 4
	full := []byte{0x73, 0x70 | byte(sectionLength>>8&0x0F), byte(sectionLength & 0xFF)}
	full = append(full, body...)

	crc := crc32MPEG(full)
	full = append(full, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return full
}

// eitEvent は EIT に載せる 1 イベント（番組）。
type eitEvent struct {
	EventID uint16
	Start   time.Time // jstNow() と同じ規約
	// Duration が UndefinedDuration のときは未定尺（0xFFFFFF）を表す。
	Duration time.Duration
	// RunningStatus は EIT の running_status。0 は指定なしとして既定値 4
	// （running）にするので、既存のフィクスチャの挙動は変わらない。
	RunningStatus byte
	Name          string
}

// buildEIT は EIT（table_id は呼び出し側が指定。schedule なら 0x50、p/f actual なら 0x4E）を
// 組み立てる。events は 0〜2 件（p/f の present/following、schedule の 1 件のいずれか）。
func buildEIT(tableID byte, sid, tsid, nid uint16, sectionNumber, lastSectionNumber, segmentLastSectionNumber, lastTableID byte, events []eitEvent) []byte {
	payload := []byte{
		byte(tsid >> 8), byte(tsid & 0xFF),
		byte(nid >> 8), byte(nid & 0xFF),
		segmentLastSectionNumber, lastTableID,
	}
	for _, e := range events {
		payload = append(payload, byte(e.EventID>>8), byte(e.EventID&0xFF))
		payload = append(payload, mjdBcdTime(e.Start)...)
		payload = append(payload, bcdDuration(e.Duration)...)

		var desc []byte
		if e.Name != "" {
			name := []byte(e.Name)
			desc = append(desc, 0x4D, byte(5+len(name)), 'j', 'p', 'n', byte(len(name)))
			desc = append(desc, name...)
			desc = append(desc, 0x00) // text_length=0
		}
		descLen := len(desc)
		runningStatus := e.RunningStatus
		if runningStatus == 0 {
			runningStatus = 4
		}
		payload = append(payload, runningStatus<<5|byte(descLen>>8&0x0F), byte(descLen&0xFF))
		payload = append(payload, desc...)
	}
	return buildSection(tableID, sid, sectionNumber, lastSectionNumber, payload)
}
