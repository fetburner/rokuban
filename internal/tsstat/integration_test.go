package tsstat

import (
	"bufio"
	"io"
	"os"
	"testing"

	"github.com/Comcast/gots/v3/packet"
)

// 既知の良品 TS ファイルに対する差分テスト（issue #6 の差分テスト戦略）。
//
// ROKUBAN_TEST_TS_FILE に「別実装（EPGStation 等）で drop / error / scrambled が
// いずれも 0 と確認済み」の .m2ts を指すと有効になる。ファイルは巨大かつ
// 著作物なのでリポジトリには置かない。
//
//	ROKUBAN_TEST_TS_FILE=/path/to/clean.m2ts go test ./internal/tsstat/ -run TestIntegration
//
// このテストが押さえるのは**誤検知がないこと**。実放送には重複パケット・
// discontinuity_indicator・payload なしパケットが実際に現れるため、
// これらの規約の扱いを間違えていれば clean なファイルで drop が出る。
//
// 検知漏れの側は既知の良品を意図的に壊して確かめる（下記の mutation テスト）。
// ファイルの内容は一切読み出さず、集計値のみを扱う。

// mutationPackets は改変テストで読むパケット数。
// 誤検知の検証は全長で行うが、検知の検証は先頭だけで十分なので短く抑える。
const mutationPackets = 400_000

func tsFixture(t *testing.T) string {
	t.Helper()
	path := os.Getenv("ROKUBAN_TEST_TS_FILE")
	if path == "" {
		t.Skip("ROKUBAN_TEST_TS_FILE not set")
	}
	return path
}

// mutation は index 番目（対象 PID 内での通番）のパケットをどう扱うかを決める。
// false を返すとそのパケットを落とす（= 欠落を再現する）。
type mutation func(occurrence int, pkt []byte) bool

// feedFile はファイル先頭の mutationPackets パケットを Counter に流す。
// targetPID が 0 以上なら、その PID のパケットにだけ mutate を適用する。
func feedFile(t *testing.T, path string, targetPID int, mutate mutation) *Counter {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	c := NewCounter(io.Discard)
	r := bufio.NewReaderSize(f, 1<<20)
	buf := make([]byte, packet.PacketSize)

	occurrence := 0
	for i := 0; i < mutationPackets; i++ {
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		if mutate != nil {
			var pkt packet.Packet
			copy(pkt[:], buf)
			if packet.Pid(&pkt) == targetPID {
				keep := mutate(occurrence, buf)
				occurrence++
				if !keep {
					continue
				}
			}
		}
		if _, err := c.Write(buf); err != nil {
			t.Fatalf("writing packet %d: %v", i, err)
		}
	}
	return c
}

// busiestPID はパケット数が最も多い PID を返す（通常は映像）。
func busiestPID(stats map[int]PIDStat) int {
	best, bestPackets := -1, int64(-1)
	for pid, s := range stats {
		if s.Packets > bestPackets {
			best, bestPackets = pid, s.Packets
		}
	}
	return best
}

// 既知の良品を全長流して drop / error / scrambled がいずれも 0 になること。
func TestIntegration_CleanFileHasNoAnomalies(t *testing.T) {
	path := tsFixture(t)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}

	// ReadFrom は 32KB 単位で流すためパケット境界をまたぐ。
	// 32768 / 188 = 174.4 なので毎チャンクで端数が出て、
	// Counter のバッファリング経路も同時に検証される。
	c, n, err := ReadFrom(f, io.Discard)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != info.Size() {
		t.Errorf("read %d bytes, want %d", n, info.Size())
	}

	stats := c.Stats()
	var packets int64
	for _, s := range stats {
		packets += s.Packets
	}
	t.Logf("%d bytes / %d packets / %d PIDs（NULL パケットは統計対象外）",
		n, packets, len(stats))
	t.Logf("totals: drops=%d errors=%d scrambled=%d",
		c.TotalDrops(), c.TotalErrors(), c.TotalScrambled())

	// PID 別に出すことで、どの PID で誤検知したかが分かるようにする
	for pid, s := range stats {
		if s.Drops != 0 || s.Errors != 0 || s.Scrambled != 0 {
			t.Errorf("PID 0x%04x: drops=%d errors=%d scrambled=%d, want all 0（誤検知）",
				pid, s.Drops, s.Errors, s.Scrambled)
		}
	}

	if packets > n/int64(packet.PacketSize) {
		t.Errorf("packets=%d exceeds the number of packets in the file (%d)",
			packets, n/int64(packet.PacketSize))
	}
}

// 1 パケット落とすと、その PID にちょうど 1 drop が出ること（検知漏れの検証）。
func TestIntegration_SingleDroppedPacketIsDetected(t *testing.T) {
	path := tsFixture(t)

	base := feedFile(t, path, -1, nil)
	target := busiestPID(base.Stats())
	if target < 0 {
		t.Fatal("no PID observed in the fixture prefix")
	}
	if base.TotalDrops() != 0 {
		t.Fatalf("prefix is not clean: drops=%d", base.TotalDrops())
	}
	t.Logf("target PID 0x%04x（先頭 %d パケット中 %d パケット）",
		target, mutationPackets, base.Stats()[target].Packets)

	// 対象 PID の 1000 番目のパケットだけを落とす
	const victim = 1000
	got := feedFile(t, path, target, func(occurrence int, _ []byte) bool {
		return occurrence != victim
	})

	stats := got.Stats()
	if stats[target].Drops != 1 {
		t.Errorf("PID 0x%04x drops = %d, want 1", target, stats[target].Drops)
	}
	for pid, s := range stats {
		if pid != target && s.Drops != 0 {
			t.Errorf("PID 0x%04x drops = %d, want 0（対象外の PID に波及）", pid, s.Drops)
		}
	}
	if got.TotalErrors() != 0 || got.TotalScrambled() != 0 {
		t.Errorf("errors=%d scrambled=%d, want 0", got.TotalErrors(), got.TotalScrambled())
	}
}

// 連続欠落は 1 drop として数えられること。
// あわせて continuity_counter が 4 ビットであることの限界を記録する。
func TestIntegration_ConsecutiveDropsAndCounterWrap(t *testing.T) {
	path := tsFixture(t)

	base := feedFile(t, path, -1, nil)
	target := busiestPID(base.Stats())

	tests := []struct {
		name      string
		missing   int
		wantDrops int64
	}{
		{"3 パケット欠落", 3, 1},
		// 15 個欠落すると次のパケットの CC が直前と同じ値になり、CC だけでは
		// 規格が許す重複と区別できない。payload を比較して欠落と判定する。
		{"15 パケット欠落（CC が直前と一致するが payload が違う）", 15, 1},
		// CC は 4 ビットなので 16 の倍数の欠落は CC が期待値と完全に一致し、
		// payload を見ても「正常な次のパケット」と区別できない。原理的な限界で、
		// 他実装（tspacketchk / EPGStation 含む）も同じ。
		{"16 パケット欠落（CC 一周のため原理的に検知不能）", 16, 0},
		{"17 パケット欠落", 17, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const from = 1000
			got := feedFile(t, path, target, func(occurrence int, _ []byte) bool {
				return occurrence < from || occurrence >= from+tt.missing
			})
			if drops := got.Stats()[target].Drops; drops != tt.wantDrops {
				t.Errorf("PID 0x%04x drops = %d, want %d", target, drops, tt.wantDrops)
			}
		})
	}
}

// TEI が立ったパケットは error に数えられること。
func TestIntegration_TransportErrorIndicatorIsCounted(t *testing.T) {
	path := tsFixture(t)

	base := feedFile(t, path, -1, nil)
	target := busiestPID(base.Stats())

	got := feedFile(t, path, target, func(occurrence int, pkt []byte) bool {
		if occurrence == 1000 {
			pkt[1] |= 0x80 // transport_error_indicator
		}
		return true
	})

	stats := got.Stats()
	if stats[target].Errors != 1 {
		t.Errorf("PID 0x%04x errors = %d, want 1", target, stats[target].Errors)
	}
	for pid, s := range stats {
		if pid != target && s.Errors != 0 {
			t.Errorf("PID 0x%04x errors = %d, want 0", pid, s.Errors)
		}
	}
	// TEI パケットの CC は信用できないので継続性の判定から外し、次のパケットで
	// 基準を取り直す。1 つの破損を error と drop で二重に数えない。
	// tspacketchk は TEI 時も CC を更新するため、直後のパケットで drop も 1 数える。
	// ここは意図的な差。
	if stats[target].Drops != 0 {
		t.Errorf("PID 0x%04x drops = %d, want 0（TEI は error のみで数える）",
			target, stats[target].Drops)
	}
}

// scrambling_control が立ったパケットは scrambled に数えられること。
// B-CAS デコード後のファイルでは 0 であるべきなので、
// これが検知できることは bcas_anomaly（M1-5-2）の前提になる。
func TestIntegration_ScramblingControlIsCounted(t *testing.T) {
	path := tsFixture(t)

	base := feedFile(t, path, -1, nil)
	target := busiestPID(base.Stats())
	if base.TotalScrambled() != 0 {
		t.Fatalf("prefix is not descrambled: scrambled=%d", base.TotalScrambled())
	}

	got := feedFile(t, path, target, func(occurrence int, pkt []byte) bool {
		if occurrence == 1000 {
			pkt[3] |= 0x80 // transport_scrambling_control = '10'
		}
		return true
	})

	if scrambled := got.Stats()[target].Scrambled; scrambled != 1 {
		t.Errorf("PID 0x%04x scrambled = %d, want 1", target, scrambled)
	}
	for pid, s := range got.Stats() {
		if pid != target && s.Scrambled != 0 {
			t.Errorf("PID 0x%04x scrambled = %d, want 0", pid, s.Scrambled)
		}
	}
}
