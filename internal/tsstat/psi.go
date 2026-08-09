package tsstat

import (
	"bytes"
	"fmt"
	"log/slog"
	"slices"

	"github.com/Comcast/gots/v3"
	"github.com/Comcast/gots/v3/packet"
	"github.com/Comcast/gots/v3/psi"
)

// PID 種別。drop_stats.pid_type に入る値の権威はここにある
// （列に CHECK は無い。circuit_breakers.name と同じ理由）。
//
// 分類は PAT / PMT のセクション再構成と ES ループの stream_type までで行う。
// 記述子は一切読まないため、字幕と文字スーパー（ARIB ではどちらも
// stream_type = 0x06）は区別せずどちらも PIDTypeOther になる
// （docs/recording.md §1「例外の境界」）。
const (
	// PIDTypeVideo は PMT の stream_type が映像だった ES。
	PIDTypeVideo = "video"
	// PIDTypeAudio は PMT の stream_type が音声だった ES。
	PIDTypeAudio = "audio"
	// PIDTypeOther は PMT に載っているが映像でも音声でもない ES。
	// 字幕・文字スーパー・データ放送はすべてここに入る（区別には記述子が必要）。
	PIDTypeOther = "other"

	// PIDTypePAT は Program Association Table（固定 PID 0x0000）。
	PIDTypePAT = "pat"
	// PIDTypeCAT は Conditional Access Table（固定 PID 0x0001）。
	PIDTypeCAT = "cat"
	// PIDTypeNIT は Network Information Table（固定 PID 0x0010）。
	PIDTypeNIT = "nit"
	// PIDTypeSDT は Service Description Table（固定 PID 0x0011）。
	PIDTypeSDT = "sdt"
	// PIDTypeEIT は Event Information Table（固定 PID 0x0012）。
	PIDTypeEIT = "eit"
	// PIDTypeTOT は Time Offset Table（固定 PID 0x0014）。
	PIDTypeTOT = "tot"
	// PIDTypePMT は PAT が PMT の在り処として指していた PID。
	// 固定ではないので PAT の解析が必要（PMT 自体の解析は不要）。
	PIDTypePMT = "pmt"
)

// fixedPIDTypes は解析なしで名前が決まる PID。
// PSI/SI の固定 PID は規格で決まっているので静的表で足りる。
var fixedPIDTypes = map[int]string{
	0x0000: PIDTypePAT,
	0x0001: PIDTypeCAT,
	0x0010: PIDTypeNIT,
	0x0011: PIDTypeSDT,
	0x0012: PIDTypeEIT,
	0x0014: PIDTypeTOT,
}

const (
	patPID = 0x0000

	tableIDPAT = 0x00
	tableIDPMT = 0x02

	// stuffingByte はセクションの代わりに詰められるバイト。table_id には現れない。
	stuffingByte = 0xFF

	// section_length は 12 ビットだが上位 2 ビットは 0 なので最大 1021。
	maxSectionLength = 1021
	// minSectionLength は受け付ける section_length の下限。
	// PSI の共通ヘッダの後に拡張ヘッダ 5 バイト + CRC 4 バイトが必ず付くので 9 未満は
	// 構文として成立しない。PAT は最低 12（プログラム 0 個）、PMT は最低 13 なので
	// 12 を共通の下限にする。
	minSectionLength = 12

	// crcLen はセクション末尾の CRC_32 の長さ。
	crcLen = 4

	// minPMTSectionLen は PMT セクション（table_id から CRC まで）の最小長。
	// program_number 2 + version 1 + section_number 1 + last 1 + PCR_PID 2 +
	// program_info_length 2 + CRC 4 に共通ヘッダ 3 を足した 16。
	minPMTSectionLen = 16
	// minPATSectionLen は PAT セクションの最小長（プログラム 0 個）。
	minPATSectionLen = 15
)

// parsePAT / parsePMT は gots の解析関数への間接参照。
//
// 差し替え可能にしているのは、解析が panic した場合に統計採取が継続することを
// テストで検証するため。gots は section_length が実バッファより長い入力で
// スライス範囲外の panic を起こす（psi.NewPMT が `sectionBytes[0:3+tableLength]`
// を無検査で切る）。sectionReassembler は section_length 分のバイトが揃うまで
// 何も渡さないのでこの経路には入らないが、その不変条件が壊れたときに ingest まで
// 落ちないよう recover を置いてある。合成 TS では踏めないのでテストは関数を
// 差し替えて panic 経路そのものを確認する。
var (
	parsePAT = psi.NewPAT
	parsePMT = psi.NewPMT
)

// sectionReassembler は 1 つの PID の PSI セクションを TS パケットの payload から復元する。
//
// payload_unit_start_indicator と pointer_field でセクションの先頭を割り出し、
// section_length 分のバイトが揃うまで蓄積する。
type sectionReassembler struct {
	// buf は収集中のセクション（table_id から始まる）。
	buf []byte
	// want は完成に必要な長さ（3 + section_length）。0 は未確定。
	want int
	// active は収集中かどうか。PUSI を見るまでは、パケットの途中から
	// 始まるセクションを取り込まないために false。
	active bool
}

func (r *sectionReassembler) reset() {
	r.buf = r.buf[:0]
	r.want = 0
	r.active = false
}

// push は 1 パケットの payload を与える。セクションが完成するたびに emit を呼ぶ。
// emit に渡すバイト列は次の push まで有効（呼び出し側は保持しない）。
func (r *sectionReassembler) push(payload []byte, pusi bool, emit func(section []byte)) {
	if !pusi {
		r.add(payload, emit)
		return
	}

	if len(payload) == 0 {
		r.reset()
		return
	}

	// pointer_field の分だけ直前のセクションの続きが入っている。
	// これを捨てると PMT 更新のたびに 1 セクション落とすことになる。
	ptr := int(payload[0])
	rest := payload[1:]
	if ptr > len(rest) {
		r.reset()
		return
	}
	if ptr > 0 {
		r.add(rest[:ptr], emit)
		rest = rest[ptr:]
	}

	r.reset()
	if len(rest) == 0 || rest[0] == stuffingByte {
		return
	}
	r.active = true
	r.add(rest, emit)
}

func (r *sectionReassembler) add(b []byte, emit func(section []byte)) {
	if !r.active || len(b) == 0 {
		return
	}
	r.buf = append(r.buf, b...)

	if r.want == 0 {
		if len(r.buf) < 3 {
			return
		}
		sectionLength := int(r.buf[1]&0x03)<<8 | int(r.buf[2])
		if sectionLength < minSectionLength || sectionLength > maxSectionLength {
			r.reset()
			return
		}
		r.want = 3 + sectionLength
	}

	if len(r.buf) >= r.want {
		emit(r.buf[:r.want])
		r.reset()
	}
}

// classifier は PAT / PMT を追って PID → 種別の対応を作る。
//
// PAT（固定 PID 0x0000）から PMT の PID 集合を得て、その PMT の ES ループから
// elementary_PID と stream_type を読む。記述子は読まない。
type classifier struct {
	types [maxPID]string
	// sections は監視対象 PID のセクション収集器。nil = 監視対象外。
	// 全パケットで引くので map ではなく配列にしてある。
	sections [maxPID]*sectionReassembler
	// pmtPIDs は現在監視している PMT の PID。PAT を見るたびに作り直す。
	pmtPIDs []int
	changes int64
}

func (c *classifier) init() {
	for pid, t := range fixedPIDTypes {
		c.types[pid] = t
	}
	c.sections[patPID] = &sectionReassembler{}
}

// observe は 1 パケットの payload を PSI 監視対象の PID にだけ通す。
//
// 規格が許す重複パケット（同一 CC・同一 payload）も素通しする。重複した
// 継続パケットはセクションを壊すが CRC 検証で捨てられ、次の周期の PAT/PMT で
// 拾い直すので、レベルトリガーとして収束する。
func (c *classifier) observe(pid int, payload []byte, pusi bool) {
	r := c.sections[pid]
	if r == nil {
		return
	}
	r.push(payload, pusi, func(section []byte) {
		c.onSection(pid, section)
	})
}

// onSection は完成したセクションを CRC 検証して PAT / PMT として解釈する。
func (c *classifier) onSection(pid int, section []byte) {
	if !validCRC(section) {
		return
	}
	switch tableID := section[0]; {
	case pid == patPID && tableID == tableIDPAT:
		c.onPAT(section)
	case pid != patPID && tableID == tableIDPMT:
		c.onPMT(pid, section)
	}
}

// validCRC はセクション末尾の CRC_32 を検証する。
// 放送波は壊れうるので、壊れたセクションを解析器に渡さないための最初の門。
func validCRC(section []byte) bool {
	if len(section) <= crcLen {
		return false
	}
	body := section[:len(section)-crcLen]
	return bytes.Equal(gots.ComputeCRC(body), section[len(section)-crcLen:])
}

func (c *classifier) onPAT(section []byte) {
	if len(section) < minPATSectionLen {
		return
	}
	pat, err := safeParsePAT(withPointerField(section))
	if err != nil {
		slog.Debug("tsstat: PAT parse failed, keeping previous classification", "err", err)
		return
	}

	// PAT が真実。PMT PID の集合は毎回作り直して PID の再割り当てに追従する。
	//
	// ただし引き続き載っている PID の収集器は使い回す。PAT は PMT より高頻度に
	// 流れるので、PAT を見るたびに収集器を作り直すと複数パケットに跨る PMT が
	// 永久に完成しない（PAT が PMT のパケットの間に割り込む）。
	next := make([]int, 0, len(c.pmtPIDs))
	for _, pmtPID := range pat.ProgramMap() {
		// PID 0 は PAT 自身なので PMT ではありえない。壊れた PAT に釣られて
		// PAT の収集器を差し替えないよう弾く。
		if pmtPID <= patPID || pmtPID >= maxPID {
			continue
		}
		c.setType(pmtPID, PIDTypePMT)
		if c.sections[pmtPID] == nil {
			c.sections[pmtPID] = &sectionReassembler{}
		}
		next = append(next, pmtPID)
	}
	for _, pid := range c.pmtPIDs {
		if !slices.Contains(next, pid) {
			c.sections[pid] = nil
		}
	}
	c.pmtPIDs = next
}

func (c *classifier) onPMT(pid int, section []byte) {
	if len(section) < minPMTSectionLen {
		return
	}
	pmt, err := safeParsePMT(withPointerField(section))
	if err != nil {
		slog.Debug("tsstat: PMT parse failed, keeping previous classification",
			"pid", pid, "err", err)
		return
	}

	for _, es := range pmt.ElementaryStreams() {
		// 記述子は読まない（Descriptors() を呼ばない）。stream_type だけで分類する。
		t := PIDTypeOther
		switch {
		case es.IsVideoContent():
			t = PIDTypeVideo
		case es.IsAudioContent():
			t = PIDTypeAudio
		}
		c.setType(es.ElementaryPid(), t)
	}
}

// setType は PID の種別を更新する。PMT は録画中に更新されうるので、
// 同じ PID の分類が変わったら最後に見たものを採用し、変化を数える。
func (c *classifier) setType(pid int, t string) {
	if pid < 0 || pid >= maxPID {
		return
	}
	// 固定 PID の名前は放送内容で上書きしない。壊れた PAT が 0x0000 を
	// PMT PID として指していても pat のままにする。
	if _, fixed := fixedPIDTypes[pid]; fixed {
		return
	}
	prev := c.types[pid]
	if prev == t {
		return
	}
	c.types[pid] = t
	if prev != "" {
		c.changes++
		slog.Debug("tsstat: PID type reclassified", "pid", pid, "from", prev, "to", t)
	}
}

// withPointerField は table_id から始まるセクションを gots が期待する形
// （pointer_field 付きの payload）に整える。
//
// gots の PAT 解析は pointer_field を読み飛ばさずプログラムループを固定位置から
// 読むので、pointer_field は 0 に正規化しておく必要がある。
// また psi.NewPAT は長さ 188 の入力を TS パケットとして扱う特別扱いがあるため、
// ちょうど 188 になる場合はスタッフィングを 1 バイト足して避ける。
func withPointerField(section []byte) []byte {
	buf := make([]byte, 0, len(section)+2)
	buf = append(buf, 0x00)
	buf = append(buf, section...)
	if len(buf) == packet.PacketSize {
		buf = append(buf, stuffingByte)
	}
	return buf
}

func safeParsePAT(b []byte) (pat psi.PAT, err error) {
	defer func() {
		if r := recover(); r != nil {
			pat, err = nil, fmt.Errorf("panic in PAT parser: %v", r)
		}
	}()
	return parsePAT(b)
}

// safeParsePMT は psi.NewPMT の panic を error に変える。
//
// PSI 解析の失敗を ingest の失敗にしない（docs/recording.md §1）。長さ検査と
// CRC 検証を通ったセクションでも、解析器が想定しない構造なら panic しうるので、
// 転送そのものを落とさないために最後の門を置く（parsePMT のコメント参照）。
func safeParsePMT(b []byte) (pmt psi.PMT, err error) {
	defer func() {
		if r := recover(); r != nil {
			pmt, err = nil, fmt.Errorf("panic in PMT parser: %v", r)
		}
	}()
	return parsePMT(b)
}
