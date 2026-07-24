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

func withDiscontinuity() packetOpt {
	return func(pkt *[packet.PacketSize]byte) {
		// adaptation_field_control = 11 (AF + payload)
		pkt[3] = (pkt[3] & 0xCF) | 0x30
		pkt[4] = 1    // adaptation_field_length = 1
		pkt[5] = 0x80 // discontinuity_indicator = 1
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

func TestCounter_AdaptationFieldOnlyNoCCCheck(t *testing.T) {
	var buf bytes.Buffer
	c := NewCounter(&buf)

	// PID 0x100: CC 0(payload), CC 5(AF only), CC 1(payload)
	// AF-only はペイロードなしなので CC チェック対象外。
	// CC 0 → CC 1 で expected 通り。
	mustWrite(t, c, makePacket(0x100, 0))
	mustWrite(t, c, makePacket(0x100, 5, withAdaptationFieldOnly()))
	mustWrite(t, c, makePacket(0x100, 1))

	stats := c.Stats()
	if stats[0x100].Drops != 0 {
		t.Errorf("drops = %d, want 0 (AF-only packet should not affect CC)", stats[0x100].Drops)
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
