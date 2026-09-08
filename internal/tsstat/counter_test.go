package tsstat

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/Comcast/gots/v3/packet"
)

// makePacket は指定フィールドで 188 バイトの TS パケットを構築する。
func makePacket(pid int, cc int, opts ...packetOpt) []byte {
	var pkt [packet.PacketSize]byte
	pkt[0] = packet.SyncByte
	// PID: byte[1] bits 4-0 = PID high 5 bits, byte[2] = PID low 8 bits
	pkt[1] = byte((pid >> 8) & 0x1F)
	pkt[2] = byte(pid & 0xFF)
	// adaptation_field_control = 01 (payload only), CC = lower 4 bits
	pkt[3] = 0x10 | byte(cc&0x0F)

	for _, o := range opts {
		o(&pkt)
	}
	return pkt[:]
}

type packetOpt func(*[packet.PacketSize]byte)

func withTEI() packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		pkt[1] |= 0x80
	}
}

func withScrambling(tsc byte) packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		pkt[3] = (pkt[3] & 0x3F) | (tsc << 6)
	}
}

func withAdaptationFieldOnly() packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		// adaptation_field_control = 10 (AF only, no payload)
		pkt[3] = (pkt[3] & 0xCF) | 0x20
		pkt[4] = 183 // adaptation_field_length
	}
}

// withPayloadByte は payload 全体を指定バイトで埋める。
// 重複判定が payload を比較していることを検証するために使う。
func withPayloadByte(b byte) packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		for i := 4; i < packet.PacketSize; i++ {
			pkt[i] = b
		}
	}
}

func withDiscontinuity() packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		// adaptation_field_control = 11 (AF + payload)
		pkt[3] = (pkt[3] & 0xCF) | 0x30
		pkt[4] = 1    // adaptation_field_length = 1
		pkt[5] = 0x80 // discontinuity_indicator = 1
	}
}

// withPCRBase は adaptation field に指定した PCR base を入れる。
// PCR extension は使わず、base の値だけをテスト対象にする。
func withPCRBase(base uint64) packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		// adaptation_field_control = 11 (AF + payload), length = flags 1 + PCR 6
		pkt[3] = (pkt[3] & 0xCF) | 0x30
		pkt[4] = 7
		pkt[5] = 0x10 // PCR_flag
		pkt[6] = byte(base >> 25)
		pkt[7] = byte(base >> 17)
		pkt[8] = byte(base >> 9)
		pkt[9] = byte(base >> 1)
		pkt[10] = byte((base&1)<<7) | 0x7E // reserved bits = 111111, extension = 0
		pkt[11] = 0
	}
}

// withDiscontinuousFlag は withPCRBase が設定した adaptation field はそのままに
// discontinuity_indicator だけを立てる。withPCRBase の後に適用すること。
func withDiscontinuousFlag() packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		pkt[5] |= 0x80
	}
}

// withZeroLengthAdaptationFieldPayload は adaptation_field_control = 11 かつ
// adaptation_field_length = 0 のパケットを作る。この形では pkt[5] は適応
// フィールドではなく payload の先頭バイトになる。gots の
// adaptationfield.IsDiscontinuous は adaptation_field_length を見ずに無条件で
// pkt[5]&0x80 を読むので、ここに 0x80 以上の payload があると
// discontinuity_indicator と誤読されうる（counter.go の Length(pkt) > 0
// ガードがこれを防ぐ）。
func withZeroLengthAdaptationFieldPayload(b byte) packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		pkt[3] = (pkt[3] & 0xCF) | 0x30 // adaptation_field_control = 11 (AF + payload)
		pkt[4] = 0                      // adaptation_field_length = 0
		for i := 5; i < packet.PacketSize; i++ {
			pkt[i] = b
		}
	}
}

func mustWrite(t *testing.T, c *Counter, p []byte) {
	t.Helper()
	if _, err := c.Write(p); err != nil {
		t.Fatal(err)
	}
}

func TestCounter_NormalStream(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100, CC 0→3: 正常な 4 パケット
	for cc := 0; cc < 4; cc++ {
		if _, err := c.Write(makePacket(0x100, cc)); err != nil {
			t.Fatal(err)
		}
	}

	stats := c.Stats()
	s, ok := stats[0x100]
	if !ok {
		t.Fatal("PID 0x100 not found in stats")
	}
	if s.Packets != 4 {
		t.Errorf("packets = %d, want 4", s.Packets)
	}
	if s.Drops != 0 {
		t.Errorf("drops = %d, want 0", s.Drops)
	}
	if s.Errors != 0 {
		t.Errorf("errors = %d, want 0", s.Errors)
	}
	if s.Scrambled != 0 {
		t.Errorf("scrambled = %d, want 0", s.Scrambled)
	}
}

func TestCounter_CCDiscontinuity(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: CC 0, 1, 2, 5 (3,4 をスキップ → 1 drop)
	for _, cc := range []int{0, 1, 2, 5} {
		if _, err := c.Write(makePacket(0x100, cc)); err != nil {
			t.Fatal(err)
		}
	}

	stats := c.Stats()
	if stats[0x100].Drops != 1 {
		t.Errorf("drops = %d, want 1", stats[0x100].Drops)
	}
}

func TestCounter_CCWrap(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: CC 14, 15, 0 → 正常（15→0 はラップ）
	for _, cc := range []int{14, 15, 0} {
		if _, err := c.Write(makePacket(0x100, cc)); err != nil {
			t.Fatal(err)
		}
	}

	stats := c.Stats()
	if stats[0x100].Drops != 0 {
		t.Errorf("drops = %d, want 0 (CC wrap 15→0 is normal)", stats[0x100].Drops)
	}
}

func TestCounter_DuplicateAllowed(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: CC 0, 1, 1, 2 → 重複 1 回は許容
	for _, cc := range []int{0, 1, 1, 2} {
		if _, err := c.Write(makePacket(0x100, cc)); err != nil {
			t.Fatal(err)
		}
	}

	stats := c.Stats()
	if stats[0x100].Drops != 0 {
		t.Errorf("drops = %d, want 0 (single duplicate is allowed)", stats[0x100].Drops)
	}
}

func TestCounter_DoubleDuplicateDrop(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: CC 0, 1, 1, 1 → 2回目の重複はドロップ
	for _, cc := range []int{0, 1, 1, 1} {
		if _, err := c.Write(makePacket(0x100, cc)); err != nil {
			t.Fatal(err)
		}
	}

	stats := c.Stats()
	if stats[0x100].Drops != 1 {
		t.Errorf("drops = %d, want 1 (double duplicate is a drop)", stats[0x100].Drops)
	}
}

func TestCounter_TEI(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: 正常パケット、TEI パケット、正常パケット
	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 1, withTEI()))
	mustWrite(t, c, makePacket(0x100, 2))

	stats := c.Stats()
	if stats[0x100].Errors != 1 {
		t.Errorf("errors = %d, want 1", stats[0x100].Errors)
	}
	if stats[0x100].Packets != 3 {
		t.Errorf("packets = %d, want 3", stats[0x100].Packets)
	}
}

func TestCounter_TEIDoesNotUpdateCC(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: CC 0, 1(TEI), 1
	// TEI パケットは CC トラッカーを更新しない。
	// TEI 後の CC 1 は expected=(0+1)=1 に一致するのでドロップなし。
	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 1, withTEI()))
	mustWrite(t, c, makePacket(0x100, 1))

	stats := c.Stats()
	if stats[0x100].Drops != 0 {
		t.Errorf("drops = %d, want 0 (TEI should not update CC tracker)", stats[0x100].Drops)
	}
}

func TestCounter_Scrambled(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 1, withScrambling(2))) // even key
	mustWrite(t, c, makePacket(0x100, 2, withScrambling(3))) // odd key
	mustWrite(t, c, makePacket(0x100, 3))                    // not scrambled

	stats := c.Stats()
	if stats[0x100].Scrambled != 2 {
		t.Errorf("scrambled = %d, want 2", stats[0x100].Scrambled)
	}
}

func TestCounter_NullPIDIgnored(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x1FFF, 0))
	mustWrite(t, c, makePacket(0x1FFF, 0))

	stats := c.Stats()
	if _, ok := stats[0x1FFF]; ok {
		t.Error("null PID (0x1FFF) should not appear in stats")
	}
}

// payload のないパケットでは CC が増えないのが正常。
func TestCounter_AdaptationFieldOnlyKeepsCC(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: CC 0(payload), CC 0(AF only), CC 1(payload)
	// AF-only は CC を増やさないので、次の payload 付きが CC 1 で連続している。
	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 0, withAdaptationFieldOnly()))
	mustWrite(t, c, makePacket(0x100, 1))

	stats := c.Stats()
	if stats[0x100].Drops != 0 {
		t.Errorf("drops = %d, want 0 (AF-only packet keeps CC)", stats[0x100].Drops)
	}
}

// payload のないパケットで CC が変わっていたら、間に payload 付きパケットが
// 欠落している。単に読み飛ばすと見逃す（tspacketchk が持つ検査）。
func TestCounter_AdaptationFieldOnlyWithChangedCCIsDrop(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 5, withAdaptationFieldOnly()))

	stats := c.Stats()
	if stats[0x100].Drops != 1 {
		t.Errorf("drops = %d, want 1 (AF-only packet must not change CC)", stats[0x100].Drops)
	}
}

// CC が直前と同じで payload も同一なら規格が許す重複。1 回までは drop にしない。
func TestCounter_DuplicateWithSamePayloadIsTolerated(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	dup := makePacket(0x100, 3, withPayloadByte(0xAA))
	mustWrite(t, c, dup)
	mustWrite(t, c, dup)
	mustWrite(t, c, makePacket(0x100, 4, withPayloadByte(0xBB)))

	stats := c.Stats()
	if stats[0x100].Drops != 0 {
		t.Errorf("drops = %d, want 0 (規格が許す重複)", stats[0x100].Drops)
	}
}

// CC が直前と同じでも payload が違えば重複ではなく、CC が一周する数の欠落。
// CC だけを見ていると欠落を重複として飲み込んでしまう。
func TestCounter_SameCCWithDifferentPayloadIsDrop(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 3, withPayloadByte(0xAA)))
	mustWrite(t, c, makePacket(0x100, 3, withPayloadByte(0xBB)))

	stats := c.Stats()
	if stats[0x100].Drops != 1 {
		t.Errorf("drops = %d, want 1 (payload が違うので重複ではない)", stats[0x100].Drops)
	}
}

// 重複が 2 回以上続くのは異常。
func TestCounter_DuplicateBeyondOnceIsDrop(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	dup := makePacket(0x100, 3, withPayloadByte(0xAA))
	mustWrite(t, c, dup)
	mustWrite(t, c, dup)
	mustWrite(t, c, dup)

	stats := c.Stats()
	if stats[0x100].Drops != 1 {
		t.Errorf("drops = %d, want 1 (重複は 1 回まで)", stats[0x100].Drops)
	}
}

func TestCounter_DiscontinuityIndicatorResets(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: CC 0, 1, discontinuity+CC=10, 11
	// discontinuity_indicator でリセットされるので CC 10 はドロップにならない
	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 1))
	mustWrite(t, c, makePacket(0x100, 10, withDiscontinuity()))
	mustWrite(t, c, makePacket(0x100, 11))

	stats := c.Stats()
	if stats[0x100].Drops != 0 {
		t.Errorf("drops = %d, want 0 (discontinuity indicator should reset CC tracking)", stats[0x100].Drops)
	}
}

// adaptation_field_control = 11 かつ adaptation_field_length = 0 では pkt[5] は
// payload の先頭バイトであり、そこに 0x80 以上の値があっても discontinuity では
// ない。CC が本物の欠落（0→2）を示していれば、誤って discontinuity と読んで
// CC トラッカーをリセットし、取りこぼしてはならない。
func TestCounter_ZeroLengthAdaptationFieldPayloadIsNotMisreadAsDiscontinuity(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 2, withZeroLengthAdaptationFieldPayload(0x80)))

	stats := c.Stats()
	if stats[0x100].Drops != 1 {
		t.Errorf("drops = %d, want 1 (payload 先頭バイトの 0x80 を discontinuity と誤読してはならない)", stats[0x100].Drops)
	}
}

func TestCounter_MultiplePIDs(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: 正常 3 パケット
	// PID 0x200: CC gap あり
	for _, cc := range []int{0, 1, 2} {
		mustWrite(t, c, makePacket(0x100, cc))
	}
	for _, cc := range []int{0, 1, 5} {
		mustWrite(t, c, makePacket(0x200, cc))
	}

	stats := c.Stats()
	if stats[0x100].Drops != 0 {
		t.Errorf("PID 0x100 drops = %d, want 0", stats[0x100].Drops)
	}
	if stats[0x200].Drops != 1 {
		t.Errorf("PID 0x200 drops = %d, want 1", stats[0x200].Drops)
	}
}

func TestCounter_TransparentPassthrough(t *testing.T) {
	var downstream bytes.Buffer
	c := NewCounter(&downstream)

	input := make([]byte, 0)
	for cc := 0; cc < 10; cc++ {
		input = append(input, makePacket(0x100, cc)...)
	}

	n, err := c.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Errorf("Write returned %d, want %d", n, len(input))
	}
	if !bytes.Equal(downstream.Bytes(), input) {
		t.Error("downstream did not receive exact input bytes")
	}
}

func TestCounter_ChunkedWrite(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// 2 パケット分のデータを 100 バイトずつに分割して書き込み
	data := append(makePacket(0x100, 0), makePacket(0x100, 1)...)
	for i := 0; i < len(data); i += 100 {
		end := i + 100
		if end > len(data) {
			end = len(data)
		}
		if _, err := c.Write(data[i:end]); err != nil {
			t.Fatal(err)
		}
	}

	stats := c.Stats()
	if stats[0x100].Packets != 2 {
		t.Errorf("packets = %d, want 2 (chunked write should reassemble)", stats[0x100].Packets)
	}
}

func TestCounter_DropPositionUsesOriginalOffsetAndPCR(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// 先頭の 5 バイトは findSync が読み飛ばすゴミ。最初の PCR は録画内 5 バイト
	// 地点で 10 秒、ドロップを検知する次のパケットは 11 秒の PCR を持つ。
	data := []byte{0x00, 0x01, 0x02, 0x03, 0x04}
	data = append(data, makePacket(0x100, 0, withPCRBase(900000))...)
	data = append(data, makePacket(0x100, 3, withPCRBase(990000))...)

	// c.buf に残る分割を含めて処理する。2 回目の Write では 2 パケットを
	// 同じ feed で消費させ、offset はバッファ連結後も元の TS ストリーム上の
	// 位置でなければならないことを固定する。
	mustWrite(t, c, data[:97])
	mustWrite(t, c, data[97:])

	positions := c.Stats()[0x100].Positions
	if len(positions) != 1 {
		t.Fatalf("positions = %d, want 1: %+v", len(positions), positions)
	}
	if positions[0].ByteOffset != 5+packet.PacketSize {
		t.Errorf("byte_offset = %d, want %d", positions[0].ByteOffset, 5+packet.PacketSize)
	}
	if positions[0].ElapsedMs == nil {
		t.Fatal("elapsed_ms = nil, want 1000")
	}
	if *positions[0].ElapsedMs != 1000 {
		t.Errorf("elapsed_ms = %d, want 1000", *positions[0].ElapsedMs)
	}
}

// TestCounter_DropPositionElapsedMsWithHighPCRBit は PCR base の上位ビット
// （bit 25 以上）が立った値でも elapsed_ms が正しくデコードされることを固定する。
// 2^25 tick ≒ 372.8 秒なので、実放送の PCR base はほぼ常にこの範囲を超える ---
// counter_test.go の他のテストは全て 2^25 未満の値しか使っておらず、
// pcrBase の `uint64(raw[0])<<25` を `<<26` に変えても go test は緑のままだった
// （実測済み）。この 2 点は raw[0] が 1→2 に変わる境界をまたぐので、シフトを
// 1 ビットずらすと最上位バイトの寄与だけが 2 点で異なる量ずれ、差分（elapsed_ms）
// が変わって検出できる。
func TestCounter_DropPositionElapsedMsWithHighPCRBit(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	const (
		base1 = 1 << 25           // 33554432, raw[0] = 1
		base2 = base1 + 90000*373 // raw[0] = 2 になる境界を越えて 373 秒後
	)

	mustWrite(t, c, makePacket(0x100, 0, withPCRBase(base1)))
	mustWrite(t, c, makePacket(0x100, 4, withPCRBase(base2))) // CC gap → drop

	positions := c.Stats()[0x100].Positions
	if len(positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(positions))
	}
	if positions[0].ElapsedMs == nil {
		t.Fatal("elapsed_ms = nil, want 373000")
	}
	if *positions[0].ElapsedMs != 373000 {
		t.Errorf("elapsed_ms = %d, want 373000", *positions[0].ElapsedMs)
	}
}

func TestCounter_DropPositionWithoutPCRHasNullElapsed(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 4))

	positions := c.Stats()[0x100].Positions
	if len(positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(positions))
	}
	if positions[0].ByteOffset != packet.PacketSize {
		t.Errorf("byte_offset = %d, want %d", positions[0].ByteOffset, packet.PacketSize)
	}
	if positions[0].ElapsedMs != nil {
		t.Errorf("elapsed_ms = %d, want nil", *positions[0].ElapsedMs)
	}
}

func TestCounter_DropPositionsAreCappedPerPIDButDropsAreNot(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 0))
	for i := 0; i < maxDropPositionsPerPID+1; i++ {
		cc := 2
		if i%2 == 1 {
			cc = 0
		}
		mustWrite(t, c, makePacket(0x100, cc))
	}

	// 別 PID の位置は、映像 PID が上限に達しても採取できる。
	mustWrite(t, c, makePacket(0x200, 0))
	mustWrite(t, c, makePacket(0x200, 5))

	stats := c.Stats()
	if stats[0x100].Drops != maxDropPositionsPerPID+1 {
		t.Errorf("PID 0x100 drops = %d, want %d", stats[0x100].Drops, maxDropPositionsPerPID+1)
	}
	if len(stats[0x100].Positions) != maxDropPositionsPerPID {
		t.Errorf("PID 0x100 positions = %d, want %d", len(stats[0x100].Positions), maxDropPositionsPerPID)
	}
	if stats[0x200].Drops != 1 || len(stats[0x200].Positions) != 1 {
		t.Errorf("PID 0x200 = drops %d, positions %d; want 1, 1", stats[0x200].Drops, len(stats[0x200].Positions))
	}
}

func TestCounter_PCRBackwardDisablesElapsedPositions(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 0, withPCRBase((1<<33)-90000)))
	// PCR base の一周をまたいだ直後。ここで時計を無効にし、以後も NULL にする。
	mustWrite(t, c, makePacket(0x100, 3, withPCRBase(0)))
	mustWrite(t, c, makePacket(0x100, 0, withPCRBase(90000)))

	positions := c.Stats()[0x100].Positions
	if len(positions) != 2 {
		t.Fatalf("positions = %d, want 2", len(positions))
	}
	for i, position := range positions {
		if position.ElapsedMs != nil {
			t.Errorf("positions[%d].elapsed_ms = %d, want nil after PCR wrap", i, *position.ElapsedMs)
		}
	}
}

// PCR を運ばないパケットの discontinuity_indicator は、そのパケットの PID の
// continuity_counter が不連続であることしか意味しない（ISO/IEC 13818-1 の
// system time-base discontinuity は PCR を運ぶパケット自身の
// discontinuity_indicator で通知される）。時計を無効にしてはならない。
func TestCounter_DiscontinuityWithoutPCRDoesNotDisableElapsedPositions(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 0, withPCRBase(900000)))
	// PCR を持たない discontinuity は時計に触れない。
	mustWrite(t, c, makePacket(0x100, 7, withDiscontinuity()))
	mustWrite(t, c, makePacket(0x100, 0, withPCRBase(990000)))

	positions := c.Stats()[0x100].Positions
	if len(positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(positions))
	}
	if positions[0].ElapsedMs == nil {
		t.Fatal("elapsed_ms = nil, want 1000")
	}
	if *positions[0].ElapsedMs != 1000 {
		t.Errorf("elapsed_ms = %d, want 1000", *positions[0].ElapsedMs)
	}
}

// PCR を運ぶパケット自身が discontinuous なら、規格どおり時計を無効にする。
func TestCounter_DiscontinuousPCRPacketDisablesElapsedPositions(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	mustWrite(t, c, makePacket(0x100, 0, withPCRBase(900000)))
	// PCR を運ぶパケット自身の discontinuity_indicator。CC は連続しているので
	// このパケット自体はドロップにならない。
	mustWrite(t, c, makePacket(0x100, 1, withPCRBase(990000), withDiscontinuousFlag()))
	// 時計は無効のまま。CC gap でドロップを発生させて確認する。
	mustWrite(t, c, makePacket(0x100, 5))

	positions := c.Stats()[0x100].Positions
	if len(positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(positions))
	}
	if positions[0].ElapsedMs != nil {
		t.Errorf("elapsed_ms = %d, want nil after discontinuous PCR packet", *positions[0].ElapsedMs)
	}
}

func TestCounter_SyncRecovery(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// 先頭にゴミ 50 バイト + 正常パケット 2 個
	garbage := make([]byte, 50)
	data := append(garbage, makePacket(0x100, 0)...)
	data = append(data, makePacket(0x100, 1)...)

	mustWrite(t, c, data)

	stats := c.Stats()
	if stats[0x100].Packets != 2 {
		t.Errorf("packets = %d, want 2 (should recover sync after garbage)", stats[0x100].Packets)
	}
}

func TestCounter_Totals(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: 1 drop, 1 error
	// PID 0x200: 1 scrambled
	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 5))                    // drop
	mustWrite(t, c, makePacket(0x100, 6, withTEI()))         // error
	mustWrite(t, c, makePacket(0x200, 0, withScrambling(2))) // scrambled

	if c.TotalDrops() != 1 {
		t.Errorf("TotalDrops = %d, want 1", c.TotalDrops())
	}
	if c.TotalErrors() != 1 {
		t.Errorf("TotalErrors = %d, want 1", c.TotalErrors())
	}
	if c.TotalScrambled() != 1 {
		t.Errorf("TotalScrambled = %d, want 1", c.TotalScrambled())
	}
}

func TestReadFrom(t *testing.T) {
	// 10 パケット分の入力を ReadFrom で処理
	var input bytes.Buffer
	for cc := 0; cc < 10; cc++ {
		input.Write(makePacket(0x100, cc))
	}

	var downstream bytes.Buffer
	counter, n, err := ReadFrom(&input, &downstream)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(10*packet.PacketSize) {
		t.Errorf("bytes read = %d, want %d", n, 10*packet.PacketSize)
	}

	stats := counter.Stats()
	if stats[0x100].Packets != 10 {
		t.Errorf("packets = %d, want 10", stats[0x100].Packets)
	}
	if downstream.Len() != 10*packet.PacketSize {
		t.Errorf("downstream size = %d, want %d", downstream.Len(), 10*packet.PacketSize)
	}
}

func TestCounter_WriteError(t *testing.T) {
	w := &errWriter{err: io.ErrShortWrite}
	c := NewCounter(w)

	_, err := c.Write(makePacket(0x100, 0))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("err = %v, want ErrShortWrite", err)
	}
}

type errWriter struct{ err error }

func (w *errWriter) Write([]byte) (int, error) { return 0, w.err }
