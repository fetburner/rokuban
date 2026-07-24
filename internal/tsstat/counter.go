package tsstat

import (
	"bufio"
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
	if pkt.IsNull() {
		return
	}
	if pid < 0 || pid >= maxPID {
		return
	}

	c.seen[pid] = true
	c.stats[pid].Packets++

	if pkt.TransportErrorIndicator() {
		c.stats[pid].Errors++
		return
	}

	if !packet.ContainsPayload(pkt) {
		return
	}

	cc := int(packet.ContinuityCounter(pkt))
	tr := &c.trackers[pid]

	if packet.ContainsAdaptationField(pkt) && adaptationfield.IsDiscontinuous(pkt) {
		tr.hasSeen = false
	}

	if tr.hasSeen {
		expected := (tr.lastCC + 1) & 0x0F
		if cc == tr.lastCC {
			tr.dup++
			if tr.dup > 1 {
				c.stats[pid].Drops++
			}
		} else {
			tr.dup = 0
			if cc != expected {
				c.stats[pid].Drops++
			}
		}
	}
	tr.lastCC = cc
	tr.hasSeen = true

	if pkt.TransportScramblingControl() >= 2 {
		c.stats[pid].Scrambled++
	}
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
