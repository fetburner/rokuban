package tsstat

import (
	"bufio"
	"bytes"
	"io"

	"github.com/Comcast/gots/v3/packet"
	"github.com/Comcast/gots/v3/packet/adaptationfield"
)

const maxPID = 8192

// PIDStat は 1 つの PID の統計。
type PIDStat struct {
	Packets   int64
	Drops     int64
	Errors    int64
	Scrambled int64
}

type pidTracker struct {
	lastCC  int
	hasSeen bool
	dup     int

	// lastPayload は直前のパケットの payload。CC が直前と同じパケットが来たとき、
	// 規格が許す重複なのか欠落なのかを見分けるために使う（processPacket 参照）。
	lastPayload []byte
}

// Counter は TS ストリームをパケット単位でパースし、
// PID 別の統計を収集する io.Writer。
// 書き込まれたバイトは下流の io.Writer にそのまま透過する。
type Counter struct {
	w        io.Writer
	buf      []byte
	trackers [maxPID]pidTracker
	stats    [maxPID]PIDStat
	seen     [maxPID]bool
}

// NewCounter は下流 w へ透過しつつ TS 統計を収集する Counter を返す。
func NewCounter(w io.Writer) *Counter {
	return &Counter{w: w}
}

// Write は io.Writer を満たす。
// 書き込まれたバイトをパケット境界で分割して統計を更新し、
// 元のバイト列をそのまま下流に書き込む。
func (c *Counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.feed(p[:n])
	}
	return n, err
}

func (c *Counter) feed(data []byte) {
	if len(c.buf) > 0 {
		c.buf = append(c.buf, data...)
		data = c.buf
	}

	for len(data) >= packet.PacketSize {
		if data[0] != packet.SyncByte {
			if off := findSync(data); off < 0 {
				c.buf = c.buf[:0]
				return
			} else {
				data = data[off:]
				continue
			}
		}
		var pkt packet.Packet
		copy(pkt[:], data[:packet.PacketSize])
		c.processPacket(&pkt)
		data = data[packet.PacketSize:]
	}

	if len(data) > 0 {
		if cap(c.buf) >= len(data) {
			c.buf = c.buf[:len(data)]
			copy(c.buf, data)
		} else {
			c.buf = append([]byte(nil), data...)
		}
	} else {
		c.buf = c.buf[:0]
	}
}

func findSync(data []byte) int {
	for i, b := range data {
		if b == packet.SyncByte {
			return i
		}
	}
	return -1
}

func (c *Counter) processPacket(pkt *packet.Packet) {
	pid := packet.Pid(pkt)
	// NULL パケットの continuity_counter は意味を持たないので統計から外す。
	if pkt.IsNull() {
		return
	}
	if pid < 0 || pid >= maxPID {
		return
	}

	c.seen[pid] = true
	c.stats[pid].Packets++

	// transport_scrambling_control は 2 ビットで、00 以外はすべてスクランブル
	// または未定義値。B-CAS 障害の検知が目的なので 0 以外を異常として数える。
	if pkt.TransportScramblingControl() != 0 {
		c.stats[pid].Scrambled++
	}

	tr := &c.trackers[pid]

	if pkt.TransportErrorIndicator() {
		c.stats[pid].Errors++
		// 伝送エラーのあったパケットの CC は信用できないので、継続性の判定から外して
		// 次のパケットで基準を取り直す。1 つの破損を drop と error で二重に数えない。
		tr.hasSeen = false
		return
	}

	// discontinuity_indicator が立っていれば CC の不連続は正常なので基準を取り直す。
	if packet.ContainsAdaptationField(pkt) && adaptationfield.IsDiscontinuous(pkt) {
		tr.hasSeen = false
	}

	cc := int(packet.ContinuityCounter(pkt))

	// payload のないパケット（adaptation_field_control が 00 / 10）では
	// CC は増えない。増えていたら間に payload 付きパケットが欠落している。
	if !packet.ContainsPayload(pkt) {
		if tr.hasSeen && cc != tr.lastCC {
			c.stats[pid].Drops++
		}
		tr.lastCC = cc
		tr.hasSeen = true
		return
	}

	payload, err := packet.Payload(pkt)
	if err != nil {
		payload = nil
	}

	if tr.hasSeen {
		switch cc {
		case tr.lastCC:
			// 規格は重複パケットを 1 回まで許すが、その payload はビット単位で同一。
			// payload が違うなら重複ではなく、CC が一周する数（16n-1 個）の欠落。
			// CC だけを見ていると欠落を重複として飲み込んでしまう。
			if !bytes.Equal(payload, tr.lastPayload) {
				tr.dup = 0
				c.stats[pid].Drops++
			} else {
				tr.dup++
				if tr.dup > 1 {
					c.stats[pid].Drops++
				}
			}
		case (tr.lastCC + 1) & 0x0F:
			// 期待どおりの連続
			tr.dup = 0
		default:
			tr.dup = 0
			c.stats[pid].Drops++
		}
	}

	tr.lastCC = cc
	tr.hasSeen = true
	tr.lastPayload = append(tr.lastPayload[:0], payload...)
}

// Stats は観測された全 PID の統計を返す。
// パケットが 0 の PID は含まない。
func (c *Counter) Stats() map[int]PIDStat {
	result := make(map[int]PIDStat)
	for pid := 0; pid < maxPID; pid++ {
		if c.seen[pid] {
			result[pid] = c.stats[pid]
		}
	}
	return result
}

// TotalDrops は全 PID のドロップ合計を返す。
func (c *Counter) TotalDrops() int64 {
	var total int64
	for pid := 0; pid < maxPID; pid++ {
		total += c.stats[pid].Drops
	}
	return total
}

// TotalErrors は全 PID のエラー合計を返す。
func (c *Counter) TotalErrors() int64 {
	var total int64
	for pid := 0; pid < maxPID; pid++ {
		total += c.stats[pid].Errors
	}
	return total
}

// TotalScrambled は全 PID のスクランブル合計を返す。
func (c *Counter) TotalScrambled() int64 {
	var total int64
	for pid := 0; pid < maxPID; pid++ {
		total += c.stats[pid].Scrambled
	}
	return total
}

// ReadFrom は io.Reader から読み取りつつ統計を収集する便利メソッド。
// 下流 Writer への書き込みと統計収集を同時に行う。
func ReadFrom(r io.Reader, w io.Writer) (*Counter, int64, error) {
	c := NewCounter(w)
	br := bufio.NewReaderSize(r, 32*1024)
	n, err := io.Copy(c, br)
	return c, n, err
}
