package tsstat

import (
	"bufio"
	"bytes"
	"io"

	"github.com/Comcast/gots/v3/packet"
	"github.com/Comcast/gots/v3/packet/adaptationfield"
)

const maxPID = 8192

// maxDropPositionsPerPID は 1 PID あたりに保持するドロップ位置の上限。
// 30 分の録画でも API 応答を有界にしつつ、UI で「真の件数のうち何件を
// 保存したか」を示せる 100 件を採る。PID ごとに上限を持たせるのは、映像 PID
// の大量のイベントが EIT など別 PID の位置を押し出さないためである。
const maxDropPositionsPerPID = 100

// PIDStat は 1 つの PID の統計。
type PIDStat struct {
	Packets   int64
	Drops     int64
	Errors    int64
	Scrambled int64
	// Positions はこの PID で観測したドロップ位置。上限は PID ごとに
	// maxDropPositionsPerPID 件で、Drops そのものは上限なく数える。
	Positions []DropPosition

	// Type は PID の種別（PIDTypeVideo などの定数のいずれか）。
	// 分類できなかった PID では空文字。空文字を「未分類」という値として
	// 永続化してはならない（drop_stats.pid_type は NULL にする）。
	Type string
}

// DropPosition は 1 件のドロップを観測した位置。
// ByteOffset は原本 TS の先頭からのバイト位置で、ElapsedMs は最初に観測した
// PCR を録画開始とみなした経過時間。PCR をまだ観測していない、または PCR の
// 逆行 / discontinuity を観測して時計を無効にした場合は ElapsedMs が nil になる。
type DropPosition struct {
	ByteOffset int64
	ElapsedMs  *int64
}

type pidTracker struct {
	lastCC  int
	hasSeen bool
	dup     int

	// lastPayload は直前のパケットの payload。CC が直前と同じパケットが来たとき、
	// 規格が許す重複なのか欠落なのかを見分けるために使う（processPacket 参照）。
	lastPayload []byte
}

// pcrTracker は全 PID から観測した PCR を録画内の相対時間へ変換する。
//
// PMT の PCR_PID を読む方法もあるが、gots の psi.PMT にアクセサが無く、PMT の
// 再構成に時計の有無を依存させると、壊れた PSI の録画で位置まで失う。実際に
// PCR を載せているパケットを素直に採用し、最初の PCR を原点にする。複数 PID が
// PCR を持つ場合も、観測順で最後の値を採る。PCR base は 33 ビットで約 26.5 時間
// で一周し、discontinuity でも戻りうるため、逆行を検出した時点で以後の経過時刻を
// 無効にする（嘘の位置を出さないことを優先する）。
type pcrTracker struct {
	firstBase uint64
	lastBase  uint64
	hasValue  bool
	invalid   bool
}

func (t *pcrTracker) observe(base uint64, discontinuous bool) {
	if !t.hasValue {
		t.firstBase = base
		t.lastBase = base
		t.hasValue = true
		return
	}
	if discontinuous || base < t.lastBase {
		t.invalid = true
	}
	t.lastBase = base
}

func (t *pcrTracker) markDiscontinuous() {
	if t.hasValue {
		t.invalid = true
	}
}

func (t *pcrTracker) elapsedMS() *int64 {
	if !t.hasValue || t.invalid {
		return nil
	}
	elapsed := int64((t.lastBase - t.firstBase) * 1000 / 90000)
	return &elapsed
}

// Counter は TS ストリームをパケット単位でパースし、
// PID 別の統計を収集する io.Writer。
// 書き込まれたバイトは下流の io.Writer にそのまま透過する。
type Counter struct {
	w   io.Writer
	buf []byte
	// bytesReceived は次の Write のバイト位置を決めるための累積値。
	// c.buf に残った未処理バイトはこの値に含まれるので、feed は
	// bytesReceived - len(c.buf) をバッファ先頭の原本位置として使う。
	bytesReceived int64
	trackers      [maxPID]pidTracker
	stats         [maxPID]PIDStat
	seen          [maxPID]bool
	psi           classifier
	pcr           pcrTracker
}

// NewCounter は下流 w へ透過しつつ TS 統計を収集する Counter を返す。
func NewCounter(w io.Writer) *Counter {
	c := &Counter{w: w}
	c.psi.init()
	return c
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
	// data は今回新たに受け取った領域、c.buf は前回からの残り。先に
	// 新しい領域を累積しておくと、バッファを連結した後の dataOffset を
	// 「今回の累積値 - 既存バッファ長」で求められる。
	dataOffset := c.bytesReceived - int64(len(c.buf))
	c.bytesReceived += int64(len(data))
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
				dataOffset += int64(off)
				continue
			}
		}
		var pkt packet.Packet
		copy(pkt[:], data[:packet.PacketSize])
		c.processPacket(&pkt, dataOffset)
		data = data[packet.PacketSize:]
		dataOffset += packet.PacketSize
	}

	if len(data) > 0 {
		if cap(c.buf) >= len(data) {
			c.buf = c.buf[:len(data)]
			copy(c.buf, data)
		} else {
			c.buf = bytes.Clone(data)
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

func (c *Counter) processPacket(pkt *packet.Packet, byteOffset int64) {
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

	hasAdaptation := packet.ContainsAdaptationField(pkt)
	discontinuous := hasAdaptation && adaptationfield.Length(pkt) > 0 && adaptationfield.IsDiscontinuous(pkt)
	if discontinuous {
		// PCR が同じ adaptation field に無くても、その discontinuity より後の
		// PCR を同じ時計として扱える保証はない。
		c.pcr.markDiscontinuous()
	}
	// gots の HasPCR / PCR は adaptation-field の長さを検査しない。長さ 7 は
	// flags 1 バイト + PCR 6 バイトを含む最小の正しい長さなので、自分で確認して
	// から呼ぶ（壊れた放送波で payload を PCR と誤読しない）。
	if hasAdaptation && adaptationfield.Length(pkt) >= 7 && adaptationfield.HasPCR(pkt) {
		if raw, err := adaptationfield.PCR(pkt); err == nil {
			c.pcr.observe(pcrBase(raw), discontinuous)
		}
	}

	// discontinuity_indicator が立っていれば CC の不連続は正常なので基準を取り直す。
	if discontinuous {
		tr.hasSeen = false
	}

	cc := int(packet.ContinuityCounter(pkt))

	// payload のないパケット（adaptation_field_control が 00 / 10）では
	// CC は増えない。増えていたら間に payload 付きパケットが欠落している。
	if !packet.ContainsPayload(pkt) {
		if tr.hasSeen && cc != tr.lastCC {
			c.recordDrop(pid, byteOffset)
		}
		tr.lastCC = cc
		tr.hasSeen = true
		return
	}

	payload, err := packet.Payload(pkt)
	if err != nil {
		payload = nil
	} else {
		// PSI の観測は伝送エラーのないパケットに限る（上で TEI は return 済み）。
		// 監視対象は PAT の PID と PAT が指した PMT の PID だけなので、
		// 大多数のパケットでは配列 1 回引いて終わる。
		c.psi.observe(pid, payload, packet.PayloadUnitStartIndicator(pkt))
	}

	if tr.hasSeen {
		switch cc {
		case tr.lastCC:
			// 規格は重複パケットを 1 回まで許すが、その payload はビット単位で同一。
			// payload が違うなら重複ではなく、CC が一周する数（16n-1 個）の欠落。
			// CC だけを見ていると欠落を重複として飲み込んでしまう。
			if !bytes.Equal(payload, tr.lastPayload) {
				tr.dup = 0
				c.recordDrop(pid, byteOffset)
			} else {
				tr.dup++
				if tr.dup > 1 {
					c.recordDrop(pid, byteOffset)
				}
			}
		case (tr.lastCC + 1) & 0x0F:
			// 期待どおりの連続
			tr.dup = 0
		default:
			tr.dup = 0
			c.recordDrop(pid, byteOffset)
		}
	}

	tr.lastCC = cc
	tr.hasSeen = true
	tr.lastPayload = append(tr.lastPayload[:0], payload...)
}

// pcrBase は adaptationfield.PCR が返す生の 6 バイトから 33 ビットの PCR base
// だけを取り出す。PCR extension は経過時間のミリ秒精度には使わない --- issue の
// 方針どおり 90kHz の base を使う。
func pcrBase(raw []byte) uint64 {
	return uint64(raw[0])<<25 |
		uint64(raw[1])<<17 |
		uint64(raw[2])<<9 |
		uint64(raw[3])<<1 |
		uint64(raw[4]>>7)
}

// recordDrop は真の Drops を必ず増やし、まだ上限に達していなければ観測位置も
// 保存する。位置の保持を上限で打ち切っても Drops は畳み込まず、API が
// 「214 件中 100 件」のように真の件数と保存件数を区別できるようにする。
func (c *Counter) recordDrop(pid int, byteOffset int64) {
	s := &c.stats[pid]
	s.Drops++
	if len(s.Positions) >= maxDropPositionsPerPID {
		return
	}
	s.Positions = append(s.Positions, DropPosition{
		ByteOffset: byteOffset,
		ElapsedMs:  c.pcr.elapsedMS(),
	})
}

// Stats は観測された全 PID の統計を返す。
// パケットが 0 の PID は含まない。
func (c *Counter) Stats() map[int]PIDStat {
	result := make(map[int]PIDStat)
	for pid := 0; pid < maxPID; pid++ {
		if c.seen[pid] {
			s := c.stats[pid]
			s.Type = c.psi.types[pid]
			s.Positions = append([]DropPosition(nil), s.Positions...)
			result[pid] = s
		}
	}
	return result
}

// TypeChanges は PID の種別が途中で変わった回数を返す。
//
// PMT は録画中に更新されうる（version 更新、番組の境目での PID 再割り当て）。
// 分類は最後に見たものを採用するので、変化があったこと自体はこのカウンタでしか
// 分からない。未分類から分類への最初の確定は変化に数えない。
func (c *Counter) TypeChanges() int64 {
	return c.psi.changes
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
