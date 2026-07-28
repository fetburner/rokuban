package tsstat

import (
	"bytes"
	"io"
	"testing"

	"github.com/Comcast/gots/v3"
	"github.com/Comcast/gots/v3/packet"
	"github.com/Comcast/gots/v3/psi"
)

// --- 合成 PSI のためのヘルパー ---

// finishSection は section_length を埋めて CRC_32 を付け、セクションを完成させる。
// CRC は gots.ComputeCRC（標準の CRC-32/MPEG-2 と一致することは
// TestComputeCRCIsStandardMPEG2 で確認している）。
func finishSection(body []byte) []byte {
	sectionLength := len(body) - 3 + crcLen
	body[1] = (body[1] & 0xF0) | byte(sectionLength>>8)
	body[2] = byte(sectionLength)
	return append(body, gots.ComputeCRC(body)...)
}

type program struct {
	number int
	pmtPID int
}

func patSection(version int, programs ...program) []byte {
	body := []byte{
		tableIDPAT,
		0xB0, 0x00, // section_syntax_indicator + section_length（finishSection が埋める）
		0x00, 0x01, // transport_stream_id
		byte(0xC1 | (version&0x1F)<<1), // version_number + current_next_indicator
		0x00, 0x00,                     // section_number / last_section_number
	}
	for _, p := range programs {
		body = append(body,
			byte(p.number>>8), byte(p.number),
			byte(0xE0|p.pmtPID>>8), byte(p.pmtPID))
	}
	return finishSection(body)
}

type esSpec struct {
	streamType byte
	pid        int
	// descriptors は ES info ループに入れる記述子。分類が記述子に依存して
	// いないことを確認するために使う（実装は Descriptors() を呼ばない）。
	descriptors []byte
}

func pmtSection(version, pcrPID int, streams ...esSpec) []byte {
	body := []byte{
		tableIDPMT,
		0xB0, 0x00,
		0x00, 0x01, // program_number
		byte(0xC1 | (version&0x1F)<<1),
		0x00, 0x00, // section_number / last_section_number
		byte(0xE0 | pcrPID>>8), byte(pcrPID),
		0xF0, 0x00, // program_info_length = 0
	}
	for _, es := range streams {
		body = append(body,
			es.streamType,
			byte(0xE0|es.pid>>8), byte(es.pid),
			byte(0xF0|len(es.descriptors)>>8), byte(len(es.descriptors)))
		body = append(body, es.descriptors...)
	}
	return finishSection(body)
}

// psiPackets はセクションを TS パケット列に詰める。
//
// セクションが 1 パケットに収まらなければ payload を使い切って次のパケットに続ける
// （途中にスタッフィングを挟まない実際の放送と同じ形）。最後のパケットの余りだけを
// 0xFF で埋める。
func psiPackets(pid, startCC int, section []byte) []byte {
	var out []byte
	rest := section
	first := true
	for cc := startCC; first || len(rest) > 0; cc++ {
		var pkt [packet.PacketSize]byte
		pkt[0] = packet.SyncByte
		pkt[1] = byte((pid >> 8) & 0x1F)
		pkt[2] = byte(pid & 0xFF)
		pkt[3] = 0x10 | byte(cc&0x0F)
		for i := 4; i < packet.PacketSize; i++ {
			pkt[i] = stuffingByte
		}

		off := 4
		if first {
			pkt[1] |= 0x40 // payload_unit_start_indicator
			pkt[4] = 0x00  // pointer_field
			off = 5
			first = false
		}

		n := packet.PacketSize - off
		if n > len(rest) {
			n = len(rest)
		}
		copy(pkt[off:off+n], rest[:n])
		rest = rest[n:]
		out = append(out, pkt[:]...)
	}
	return out
}

// dataPackets は種別を確認したい PID を Stats() に載せるためのダミー ES パケット。
func dataPackets(pid, n int) []byte {
	var out []byte
	for i := 0; i < n; i++ {
		out = append(out, makePacket(pid, i)...)
	}
	return out
}

func typeOf(t *testing.T, stats map[int]PIDStat, pid int) string {
	t.Helper()
	s, ok := stats[pid]
	if !ok {
		t.Fatalf("PID 0x%04x が統計に無い", pid)
	}
	return s.Type
}

// --- CRC の前提 ---

// gots.ComputeCRC が本当に標準の CRC-32/MPEG-2 かを確かめる。
// ここが違っていると「自分で作ったセクションだけ検証を通る」テストになり、
// 実際の放送波のセクションを全部捨てる実装を通してしまう。
func TestComputeCRCIsStandardMPEG2(t *testing.T) {
	// CRC catalogue の CRC-32/MPEG-2 check 値。
	if got := gots.ComputeCRC([]byte("123456789")); !bytes.Equal(got, []byte{0x03, 0x76, 0xE6, 0xE7}) {
		t.Errorf("check value = %x, want 0376e6e7", got)
	}

	// gots が同梱する実データの PAT パケット（payload の末尾 4 バイトが CRC）。
	payload, err := packet.Payload(&packet.TestPatPacket)
	if err != nil {
		t.Fatal(err)
	}
	section := payload[1 : 1+3+int(psi.SectionLength(payload))]
	if !validCRC(section) {
		t.Errorf("実データの PAT セクションが CRC 検証に落ちた: %x", section)
	}
}

// --- 分類 ---

func TestClassifier_VideoAudioOther(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	mustWrite(t, c, psiPackets(0x1000, 0, pmtSection(0, 0x0100,
		esSpec{streamType: 0x02, pid: 0x0100}, // MPEG-2 Video
		esSpec{streamType: 0x1B, pid: 0x0101}, // H.264
		esSpec{streamType: 0x0F, pid: 0x0110}, // AAC (ADTS)
		// ARIB の字幕・文字スーパーはどちらも 0x06。区別には component_tag
		// 記述子（タグ 0x52）が必要だが読まないので、記述子があっても other。
		esSpec{streamType: 0x06, pid: 0x0120, descriptors: []byte{0x52, 0x01, 0x30}},
	)))

	for _, pid := range []int{0x0100, 0x0101, 0x0110, 0x0120, 0x0200} {
		mustWrite(t, c, dataPackets(pid, 3))
	}

	stats := c.Stats()
	want := map[int]string{
		0x0000: PIDTypePAT,   // 固定表
		0x1000: PIDTypePMT,   // PAT が指した PID
		0x0100: PIDTypeVideo, //
		0x0101: PIDTypeVideo, //
		0x0110: PIDTypeAudio, //
		0x0120: PIDTypeOther, // 字幕（記述子を読まないので other 止まり）
		0x0200: "",           // PMT に無い PID は未分類
	}
	for pid, wantType := range want {
		if got := typeOf(t, stats, pid); got != wantType {
			t.Errorf("PID 0x%04x type = %q, want %q", pid, got, wantType)
		}
	}
	if c.TypeChanges() != 0 {
		t.Errorf("TypeChanges = %d, want 0（初回の確定は変化ではない）", c.TypeChanges())
	}
}

// 固定 PID は PSI を解析しなくても名前が付く（docs/recording.md の静的表）。
// payload はゴミなので、解析に依存していたら分類できない。
func TestClassifier_FixedPIDsNeedNoParsing(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	want := map[int]string{
		0x0000: PIDTypePAT,
		0x0001: PIDTypeCAT,
		0x0010: PIDTypeNIT,
		0x0011: PIDTypeSDT,
		0x0012: PIDTypeEIT,
		0x0014: PIDTypeTOT,
	}
	for pid := range want {
		mustWrite(t, c, dataPackets(pid, 2))
	}

	stats := c.Stats()
	for pid, wantType := range want {
		if got := typeOf(t, stats, pid); got != wantType {
			t.Errorf("PID 0x%04x type = %q, want %q", pid, got, wantType)
		}
	}
}

// 3 パケットに跨る PMT が再構成されること。
func TestClassifier_SectionSpanningPackets(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// ES 80 個 = 12 + 400 + 4 = 416 バイト。payload 183 + 184 + 49 の 3 パケットになる。
	const esCount = 80
	streams := make([]esSpec, 0, esCount)
	for i := 0; i < esCount; i++ {
		streams = append(streams, esSpec{streamType: 0x0F, pid: 0x0200 + i})
	}
	streams[0] = esSpec{streamType: 0x1B, pid: 0x0200}
	section := pmtSection(0, 0x0200, streams...)
	if len(section) <= 2*packet.PacketSize {
		t.Fatalf("セクションが 2 パケットに収まってしまう: %d バイト", len(section))
	}

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	pmtPackets := psiPackets(0x1000, 0, section)
	if got := len(pmtPackets) / packet.PacketSize; got < 3 {
		t.Fatalf("PMT が %d パケットしかない", got)
	}
	mustWrite(t, c, pmtPackets)
	mustWrite(t, c, dataPackets(0x0200, 2))
	lastPID := 0x0200 + esCount - 1
	mustWrite(t, c, dataPackets(lastPID, 2)) // 最後の ES

	stats := c.Stats()
	if got := typeOf(t, stats, 0x0200); got != PIDTypeVideo {
		t.Errorf("先頭 ES type = %q, want video", got)
	}
	if got := typeOf(t, stats, lastPID); got != PIDTypeAudio {
		t.Errorf("末尾 ES type = %q, want audio（セクションが繋がっていない）", got)
	}
}

// PAT は PMT より高頻度に流れる。複数パケットに跨る PMT の途中に PAT が
// 割り込んでも、収集中の PMT セクションを捨ててはいけない（捨てると実際の放送で
// 大きい PMT が永久に完成しない）。
func TestClassifier_InterleavedPATKeepsPMTCollector(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	pat := patSection(0, program{number: 1, pmtPID: 0x1000})
	mustWrite(t, c, psiPackets(patPID, 0, pat))

	streams := make([]esSpec, 0, 40)
	streams = append(streams, esSpec{streamType: 0x02, pid: 0x0100})
	for i := 1; i < 40; i++ {
		streams = append(streams, esSpec{streamType: 0x0F, pid: 0x0300 + i})
	}
	pmtPackets := psiPackets(0x1000, 0, pmtSection(0, 0x0100, streams...))
	if len(pmtPackets) != 2*packet.PacketSize {
		t.Fatalf("PMT のパケット数 = %d, want 2", len(pmtPackets)/packet.PacketSize)
	}

	mustWrite(t, c, pmtPackets[:packet.PacketSize]) // PMT 前半
	mustWrite(t, c, psiPackets(patPID, 1, pat))     // 同じ PAT が割り込む
	mustWrite(t, c, pmtPackets[packet.PacketSize:]) // PMT 後半
	mustWrite(t, c, dataPackets(0x0100, 2))

	if got := typeOf(t, c.Stats(), 0x0100); got != PIDTypeVideo {
		t.Errorf("type = %q, want video（PAT の割り込みで PMT を落としている）", got)
	}
}

// PUSI パケットの pointer_field が指す前セクションの残りを捨てると、
// 同じパケットで終わるセクションを 1 つ落とす。
func TestClassifier_PointerFieldCarriesPreviousSectionTail(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))

	// first は 1 パケットの payload（PUSI 付きで 183 バイト）に収まらない長さにして、
	// 残りが 2 パケット目の先頭に来るようにする。ES 38 個 = 16 + 190 = 206 バイト。
	streams := make([]esSpec, 0, 38)
	streams = append(streams, esSpec{streamType: 0x02, pid: 0x0100})
	for i := 1; i < 38; i++ {
		streams = append(streams, esSpec{streamType: 0x0F, pid: 0x0300 + i})
	}
	first := pmtSection(0, 0x0100, streams...)
	second := pmtSection(1, 0x0100, esSpec{streamType: 0x0F, pid: 0x0100})

	// 1 パケット目: PUSI + pointer_field 0 + first を payload いっぱいまで
	const firstPayload = packet.PacketSize - 5
	if len(first) <= firstPayload {
		t.Fatalf("first が 1 パケットに収まってしまう: %d バイト", len(first))
	}
	mustWrite(t, c, psiPackets(0x1000, 0, first)[:packet.PacketSize])

	// 2 パケット目: PUSI + pointer_field = first の残り長、その後ろに first の残りと second。
	// 実際の PMT 更新でこの形になる（新セクションの手前に前セクションの尻尾が入る）。
	tail := first[firstPayload:]
	if len(tail)+len(second)+1 > packet.PacketSize-4 {
		t.Fatalf("2 パケット目に収まらない: tail=%d second=%d", len(tail), len(second))
	}
	var pkt [packet.PacketSize]byte
	pkt[0] = packet.SyncByte
	pkt[1] = 0x40 | byte((0x1000>>8)&0x1F)
	pkt[2] = 0x00
	pkt[3] = 0x11 // CC 1
	for i := 4; i < packet.PacketSize; i++ {
		pkt[i] = stuffingByte
	}
	pkt[4] = byte(len(tail))
	copy(pkt[5:], tail)
	copy(pkt[5+len(tail):], second)
	mustWrite(t, c, pkt[:])

	mustWrite(t, c, dataPackets(0x0100, 2))

	stats := c.Stats()
	// second が後から来ているので audio が最後。
	if got := typeOf(t, stats, 0x0100); got != PIDTypeAudio {
		t.Errorf("type = %q, want audio", got)
	}
	// first（video）も完成していたことを変化回数で確認する。
	// pointer_field 分を捨てる実装だと first が完成せず変化は 0 になる。
	if c.TypeChanges() != 1 {
		t.Errorf("TypeChanges = %d, want 1（前セクションの残りが繋がっていない）", c.TypeChanges())
	}
}

// PMT 更新で同一 PID の分類が変わったら最後に見たものを採用し、変化を数える。
func TestClassifier_PMTUpdateLastSeenWins(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	mustWrite(t, c, psiPackets(0x1000, 0, pmtSection(0, 0x0100,
		esSpec{streamType: 0x02, pid: 0x0100},
	)))
	mustWrite(t, c, dataPackets(0x0100, 2))

	if got := typeOf(t, c.Stats(), 0x0100); got != PIDTypeVideo {
		t.Fatalf("更新前 type = %q, want video", got)
	}

	// 同じ PMT を繰り返しても変化ではない（実波形では 100ms ごとに再送される）。
	mustWrite(t, c, psiPackets(0x1000, 1, pmtSection(0, 0x0100,
		esSpec{streamType: 0x02, pid: 0x0100},
	)))
	if c.TypeChanges() != 0 {
		t.Errorf("同一 PMT の再送で TypeChanges = %d, want 0", c.TypeChanges())
	}

	// version を上げて同じ PID を音声に付け替える。
	mustWrite(t, c, psiPackets(0x1000, 2, pmtSection(1, 0x0100,
		esSpec{streamType: 0x0F, pid: 0x0100},
	)))

	if got := typeOf(t, c.Stats(), 0x0100); got != PIDTypeAudio {
		t.Errorf("更新後 type = %q, want audio（最後に見たものを採用する）", got)
	}
	if c.TypeChanges() != 1 {
		t.Errorf("TypeChanges = %d, want 1", c.TypeChanges())
	}
}

// PAT が PMT の PID を付け替えたら、古い PID の PMT は見ない。
func TestClassifier_PATRepointsPMTPID(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	mustWrite(t, c, psiPackets(0x1000, 0, pmtSection(0, 0x0100,
		esSpec{streamType: 0x02, pid: 0x0100},
	)))
	// PAT が PMT を 0x1001 に付け替える
	mustWrite(t, c, psiPackets(patPID, 1, patSection(1, program{number: 1, pmtPID: 0x1001})))
	// 古い PID に来た PMT は無視される
	mustWrite(t, c, psiPackets(0x1000, 1, pmtSection(1, 0x0100,
		esSpec{streamType: 0x0F, pid: 0x0100},
	)))
	mustWrite(t, c, dataPackets(0x0100, 2))
	mustWrite(t, c, dataPackets(0x1001, 2))

	stats := c.Stats()
	if got := typeOf(t, stats, 0x0100); got != PIDTypeVideo {
		t.Errorf("type = %q, want video（PAT から外れた PID の PMT を読んでいる）", got)
	}
	if got := typeOf(t, stats, 0x1001); got != PIDTypePMT {
		t.Errorf("新しい PMT PID type = %q, want pmt", got)
	}
}

// --- 壊れた PSI ---

// CRC が合わないセクションは捨てる。統計自体は成立する。
func TestClassifier_BadCRCIsIgnored(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	broken := pmtSection(0, 0x0100, esSpec{streamType: 0x02, pid: 0x0100})
	broken[len(broken)-1] ^= 0xFF // CRC の最終バイトを壊す
	mustWrite(t, c, psiPackets(0x1000, 0, broken))
	mustWrite(t, c, dataPackets(0x0100, 4))

	stats := c.Stats()
	if got := typeOf(t, stats, 0x0100); got != "" {
		t.Errorf("type = %q, want \"\"（CRC 不一致のセクションを採用している）", got)
	}
	if stats[0x0100].Packets != 4 {
		t.Errorf("packets = %d, want 4（壊れた PSI で統計が壊れている）", stats[0x0100].Packets)
	}
}

// section_length が不正なセクションは捨てる。panic しない。
func TestClassifier_BadSectionLength(t *testing.T) {
	cases := map[string]func(section []byte){
		"過大":  func(s []byte) { s[1] = (s[1] & 0xF0) | 0x03; s[2] = 0xFD }, // 1021 を宣言
		"過小":  func(s []byte) { s[1] &= 0xF0; s[2] = 0x05 },                // 5 を宣言（gots のアンダーフロー領域）
		"上限超": func(s []byte) { s[1] |= 0x0F; s[2] = 0xFF },                // 4095 を宣言
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			c := NewCounter(&buf)

			mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
			section := pmtSection(0, 0x0100, esSpec{streamType: 0x02, pid: 0x0100})
			corrupt(section)
			mustWrite(t, c, psiPackets(0x1000, 0, section))
			mustWrite(t, c, dataPackets(0x0100, 4))

			stats := c.Stats()
			if got := typeOf(t, stats, 0x0100); got != "" {
				t.Errorf("type = %q, want \"\"", got)
			}
			if stats[0x0100].Packets != 4 {
				t.Errorf("packets = %d, want 4", stats[0x0100].Packets)
			}
		})
	}
}

// CRC は合うが短すぎるセクション。CRC 検証を通ってしまう入力なので、
// 長さの下限だけが門になる。分類されず、統計は成立する。
func TestClassifier_ShortSectionWithValidCRC(t *testing.T) {
	for _, pid := range []int{patPID, 0x1000} {
		var buf bytes.Buffer
		c := NewCounter(&buf)
		mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))

		tableID := byte(tableIDPMT)
		if pid == patPID {
			tableID = tableIDPAT
		}
		// section_length = 5（CRC 4 バイト + 1 バイト）。CRC は正しく計算する。
		section := finishSection([]byte{tableID, 0xB0, 0x00, 0x00})
		if len(section) != 8 {
			t.Fatalf("セクション長 = %d, want 8", len(section))
		}
		if !validCRC(section) {
			t.Fatal("テストの前提が崩れている: CRC が合っていない")
		}

		mustWrite(t, c, psiPackets(pid, 1, section))
		mustWrite(t, c, dataPackets(0x0100, 4))

		stats := c.Stats()
		if got := typeOf(t, stats, 0x0100); got != "" {
			t.Errorf("pid 0x%04x: type = %q, want \"\"", pid, got)
		}
		if stats[0x0100].Packets != 4 {
			t.Errorf("pid 0x%04x: packets = %d, want 4", pid, stats[0x0100].Packets)
		}
	}
}

// CRC は合うが構造が壊れている PMT（program_info_length がセクションを突き抜ける）。
// 長さ検査と CRC 検証を通ってしまうので、ここから先は解析器の堅牢性に頼ることになる。
func TestClassifier_StructurallyBrokenPMTWithValidCRC(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	body := []byte{
		tableIDPMT, 0xB0, 0x00,
		0x00, 0x01,
		0xC1,
		0x00, 0x00,
		0xE1, 0x00,
		0xFF, 0xFF, // program_info_length = 0x0FFF（セクション長を超える）
		0x02, 0xE1, 0x00, 0xF0, 0x00, // ES: video 0x0100
	}
	section := finishSection(body)
	if !validCRC(section) {
		t.Fatal("テストの前提が崩れている: CRC が合っていない")
	}

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	mustWrite(t, c, psiPackets(0x1000, 0, section))
	mustWrite(t, c, dataPackets(0x0100, 4))

	// panic しないことと、統計が成立することが要求。
	stats := c.Stats()
	if stats[0x0100].Packets != 4 {
		t.Errorf("packets = %d, want 4", stats[0x0100].Packets)
	}
}

// 解析器が panic しても ingest を落とさない（recover が効いている）。
//
// 長さ検査 + CRC 検証を通り抜けて gots が panic する入力は合成できないので、
// 解析関数を差し替えて panic 経路そのものを確認する。
func TestClassifier_RecoversFromParserPanic(t *testing.T) {
	orig := parsePMT
	t.Cleanup(func() { parsePMT = orig })
	called := false
	parsePMT = func([]byte) (psi.PMT, error) {
		called = true
		panic("boom")
	}

	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	mustWrite(t, c, psiPackets(0x1000, 0, pmtSection(0, 0x0100,
		esSpec{streamType: 0x02, pid: 0x0100},
	)))
	mustWrite(t, c, dataPackets(0x0100, 4))

	if !called {
		t.Fatal("差し替えた解析関数が呼ばれていない")
	}
	stats := c.Stats()
	if got := typeOf(t, stats, 0x0100); got != "" {
		t.Errorf("type = %q, want \"\"", got)
	}
	if stats[0x0100].Packets != 4 {
		t.Errorf("packets = %d, want 4", stats[0x0100].Packets)
	}
	if buf.Len() == 0 {
		t.Error("下流への透過が止まっている")
	}
}

// PAT の解析が panic しても同じ。
func TestClassifier_RecoversFromPATParserPanic(t *testing.T) {
	orig := parsePAT
	t.Cleanup(func() { parsePAT = orig })
	parsePAT = func([]byte) (psi.PAT, error) { panic("boom") }

	var buf bytes.Buffer
	c := NewCounter(&buf)
	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	mustWrite(t, c, dataPackets(0x1000, 2))

	if got := typeOf(t, c.Stats(), 0x1000); got != "" {
		t.Errorf("type = %q, want \"\"", got)
	}
}

// 壊れた PAT が固定 PID を PMT として指しても、固定名は上書きされない。
func TestClassifier_FixedNameNotOverwrittenByPAT(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x0012})))
	mustWrite(t, c, dataPackets(0x0012, 2))

	if got := typeOf(t, c.Stats(), 0x0012); got != PIDTypeEIT {
		t.Errorf("type = %q, want eit", got)
	}
	if c.TypeChanges() != 0 {
		t.Errorf("TypeChanges = %d, want 0", c.TypeChanges())
	}
}

// 任意のバイト列で panic しないこと。壊れた放送波を模す最後の網。
func FuzzCounterWrite(f *testing.F) {
	f.Add(psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})))
	f.Add(psiPackets(0x1000, 0, pmtSection(0, 0x0100,
		esSpec{streamType: 0x02, pid: 0x0100},
		esSpec{streamType: 0x0F, pid: 0x0110},
	)))
	f.Add(append(psiPackets(patPID, 0, patSection(0, program{number: 1, pmtPID: 0x1000})),
		psiPackets(0x1000, 0, pmtSection(0, 0x0100, esSpec{streamType: 0x02, pid: 0x0100}))...))
	f.Add([]byte{0x47, 0x40, 0x00, 0x10})

	f.Fuzz(func(t *testing.T, data []byte) {
		c := NewCounter(io.Discard)
		if _, err := c.Write(data); err != nil {
			t.Fatal(err)
		}
		c.Stats()
		c.TypeChanges()
	})
}
