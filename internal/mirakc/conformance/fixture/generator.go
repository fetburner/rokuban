//go:build conformance

package fixture

import (
	"context"
	"io"
	"time"
)

// PID の割り当て。0x0000/0x0010/0x0011/0x0012/0x0014 は規格上の固定 PID、0x0100 以降は
// 任意（他と衝突しなければよい）。
const (
	pidPAT   = 0x0000
	pidNIT   = 0x0010
	pidSDT   = 0x0011
	pidEIT   = 0x0012
	pidTOT   = 0x0014
	pidPMT   = 0x0100
	pidVideo = 0x0101 // PCR もここに乗せる（PMT.pcr_pid と一致させる）
	pidAudio = 0x0102
)

// このフィクスチャが表現する放送・サービス・番組の識別子。internal/mirakc.ComposeProgramID /
// ServiceID と同じ合成規則を使う側（conformance_test.go）が programId / service id を
// 逆算できるよう、値そのものをここで公開する。
const (
	NetworkID        = 32736
	TSID             = 32736
	ServiceID        = 101
	EventID          = 1001
	FollowingEventID = 1002
	ServiceName      = "ROKUBAN-CONFORMANCE"
	EventName        = "rokuban conformance fixture program"

	// EventDuration は番組の長さ。録画が自然終了するまでテストが待つ時間の上限でもあるので、
	// 「録画中」の判定を試すのに十分かつ待ち時間が膨らまない値にする（実測して決めた値。
	// internal/mirakc/conformance/conformance_test.go の TestConformance/RecordingInProgress
	// 参照）。
	EventDuration = 30 * time.Second

	// EventLeadIn は「番組が何秒前に始まったことにするか」。0 だと mirakc が EIT p/f を
	// 読んだ直後の 1 パケットだけを present と following の境界で取りこぼす可能性がある
	// ため、余裕を持たせる。
	EventLeadIn = 5 * time.Second
)

// Config はフィクスチャが表現する放送 1 波ぶんのパラメータ。EventStart / EventStart +
// EventDuration の時刻は jstNow() と同じ規約（JST の暦値をそのまま保持する time.Time）で
// 渡す。
type Config struct {
	NetworkID        uint16
	TSID             uint16
	ServiceID        uint16
	EventID          uint16
	FollowingEventID uint16
	ServiceName      string
	EventName        string
	EventStart       time.Time
	EventDuration    time.Duration
}

// NewConfig は「呼び出された瞬間の EventLeadIn 秒前に始まり EventDuration 秒続く」番組を
// 表す Config を作る。
//
// **mirakc は tuners[].command を呼び出しのたびに新しいプロセスとして起動する**
// （EPG 収集ジョブの短時間の呼び出しと、実際の録画のための呼び出しは別プロセス）。
// 番組の開始時刻を「呼び出された瞬間からの相対時刻」にすることで、cron の間隔や EPG
// 収集にかかった時間に関係なく、どの呼び出しも「たった今始まった番組」として振る舞う。
// 結果として、実際の録画が始まるタイミングがいつであっても、その回の filter-program は
// 番組をすぐに現在放送中と判定できる。
func NewConfig() Config {
	start := jstNow().Add(-EventLeadIn)
	return Config{
		NetworkID:        NetworkID,
		TSID:             TSID,
		ServiceID:        ServiceID,
		EventID:          EventID,
		FollowingEventID: FollowingEventID,
		ServiceName:      ServiceName,
		EventName:        EventName,
		EventStart:       start,
		EventDuration:    EventDuration,
	}
}

// tickInterval は PCR を含む多重化ループの刻み。
const tickInterval = 20 * time.Millisecond

type due struct {
	interval time.Duration
	next     time.Time
}

func (d *due) fire(t time.Time) bool {
	if t.Before(d.next) {
		return false
	}
	d.next = t.Add(d.interval)
	return true
}

// Run は cfg が表す放送を w へ実時間で流し続ける。ctx がキャンセルされるか w への書き込みが
// 失敗するまで戻らない（呼び出し側はこの両方を「正常終了」として扱ってよい。mirakc が
// チューナーを閉じるときの標準的な止め方だからである）。
//
// TOT と PCR は送信のたびに現在時刻から作り直す。固定値にすると、mirakc-arib の
// sync-clocks（TOT と PCR を突き合わせる）や filter-program（同じ突き合わせを録画中も
// 続ける）が壁時計とのずれを検知して失敗しうる。
func Run(ctx context.Context, w io.Writer, cfg Config) error {
	var ccPAT, ccPMT, ccSDT, ccNIT, ccTOT, ccEIT uint8

	now := time.Now()
	nextPAT := due{100 * time.Millisecond, now}
	nextPMT := due{100 * time.Millisecond, now}
	nextSDT := due{500 * time.Millisecond, now}
	nextNIT := due{2 * time.Second, now}
	nextTOT := due{time.Second, now}
	nextEITPF := due{500 * time.Millisecond, now}
	nextEITSchedule := due{2 * time.Second, now}

	streams := []pmtStream{
		{StreamType: 0x02, PID: pidVideo}, // MPEG-2 video（ES の中身は送らない。PMT が宣言するだけで足りる）
		{StreamType: 0x0F, PID: pidAudio}, // AAC
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	write := func(b []byte) error {
		_, err := w.Write(b)
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-ticker.C:
			pcr := pcrTicksFromTime(t)
			if err := write(pcrPacket(pidVideo, pcr)); err != nil {
				return err
			}
			if nextPAT.fire(t) {
				if err := write(packetizeSection(pidPAT, buildPAT(cfg.TSID, cfg.ServiceID, pidPMT, pidNIT), &ccPAT)); err != nil {
					return err
				}
			}
			if nextPMT.fire(t) {
				if err := write(packetizeSection(pidPMT, buildPMT(cfg.ServiceID, pidVideo, streams), &ccPMT)); err != nil {
					return err
				}
			}
			if nextSDT.fire(t) {
				if err := write(packetizeSection(pidSDT, buildSDT(cfg.TSID, cfg.NetworkID, cfg.ServiceID, 0x01, cfg.ServiceName), &ccSDT)); err != nil {
					return err
				}
			}
			if nextNIT.fire(t) {
				if err := write(packetizeSection(pidNIT, buildNIT(cfg.NetworkID, cfg.TSID, cfg.NetworkID), &ccNIT)); err != nil {
					return err
				}
			}
			if nextTOT.fire(t) {
				if err := write(packetizeSection(pidTOT, buildTOT(jstNow()), &ccTOT)); err != nil {
					return err
				}
			}
			if nextEITPF.fire(t) {
				present := eitEvent{EventID: cfg.EventID, Start: cfg.EventStart, Duration: cfg.EventDuration, Name: cfg.EventName}
				following := eitEvent{
					EventID:  cfg.FollowingEventID,
					Start:    cfg.EventStart.Add(cfg.EventDuration),
					Duration: time.Hour,
					Name:     cfg.EventName + " (following)",
				}
				sec0 := buildEIT(0x4E, cfg.ServiceID, cfg.TSID, cfg.NetworkID, 0, 1, 1, 0x4E, []eitEvent{present})
				sec1 := buildEIT(0x4E, cfg.ServiceID, cfg.TSID, cfg.NetworkID, 1, 1, 1, 0x4E, []eitEvent{following})
				if err := write(packetizeSection(pidEIT, sec0, &ccEIT)); err != nil {
					return err
				}
				if err := write(packetizeSection(pidEIT, sec1, &ccEIT)); err != nil {
					return err
				}
			}
			if nextEITSchedule.fire(t) {
				ev := eitEvent{EventID: cfg.EventID, Start: cfg.EventStart, Duration: cfg.EventDuration, Name: cfg.EventName}
				sec := buildEIT(0x50, cfg.ServiceID, cfg.TSID, cfg.NetworkID, 0, 0, 0, 0x50, []eitEvent{ev})
				if err := write(packetizeSection(pidEIT, sec, &ccEIT)); err != nil {
					return err
				}
			}
		}
	}
}
