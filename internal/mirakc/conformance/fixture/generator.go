//go:build conformance

package fixture

import (
	"context"
	"io"
	"strconv"
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

// このフィクスチャが表現する放送・サービス・番組の識別子。internal/programid.ComposeProgramID /
// ServiceID と同じ合成規則を使う側（conformance_test.go）が programId / service id を
// 逆算できるよう、値そのものをここで公開する。
const (
	NetworkID          = 32736
	TSID               = 32736
	ServiceID          = 101
	EventID            = 1001
	FollowingEventID   = 1002
	PrecedingEventID   = 1000
	ReplacementEventID = 1003
	// ServiceName / EventName は生の ASCII のまま short_event_descriptor / service_descriptor
	// に書く。ARIB の文字符号化（8 単位符号）は既定で英数字を漢字集合として解釈するため、
	// mirakc はこれを文字化けとして decode する（scan-services / collect-eits の "name" が
	// 実際に文字化けすることを確認済み）。名前のラウンドトリップは何もアサートしていないので
	// 実害は無いが、将来これを判定に使うなら ARIB 8 単位符号のエスケープが要る。
	ServiceName = "ROKUBAN-CONFORMANCE"
	EventName   = "rokuban conformance fixture program"

	// EventDuration は番組の長さ。録画が自然終了するまでテストが待つ時間の上限でもあるので、
	// 「録画中」の判定を試すのに十分かつ待ち時間が膨らまない値にする（実測して決めた値。
	// internal/mirakc/conformance/conformance_test.go の TestConformance/RecordingInProgress
	// 参照）。
	EventDuration = 30 * time.Second

	// UndefinedDuration は EIT の duration=0xFFFFFF（未定尺）を表す。
	UndefinedDuration time.Duration = -1

	// EventLeadIn は「番組が何秒前に始まったことにするか」。0 だと mirakc が EIT p/f を
	// 読んだ直後の 1 パケットだけを present と following の境界で取りこぼす可能性がある
	// ため、余裕を持たせる。
	//
	// NewConfig のコメントのとおり EventStart は呼び出しのたびに作り直されるので、EPG
	// 収集用の呼び出し（scan-services / sync-clocks / update-schedules）で読まれた
	// startAt は、実際に録画が始まる呼び出しの時点では既にいくらか古い。その古さの分だけ
	// mirakc が実際に録画する長さは EventDuration より短くなる（実測でこの古さは
	// 8〜16 秒、録画時間は 16〜24 秒。conformance_test.go の CompletedRecordStreamAndDelete
	// が観測値をログする）。
	EventLeadIn = 5 * time.Second
)

const (
	// CasePrecedingExtension は前番組が未定尺のまま延長しているケース。
	CasePrecedingExtension = "preceding-extension"
	// CaseRunningStatus は target が running_status=2 の present にいるケース。
	CaseRunningStatus = "running-status"
	// CaseFollowing は present が前番組、following が target のケース。
	CaseFollowing = "following"
	// CaseEventIDReset は target の event_id が同じ時間帯で振り直されるケース。
	CaseEventIDReset = "event-id-reset"
)

const pathologyDuration = 15 * time.Second

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
	// Case が空なら既存の正常系。値がある場合は conformance の放送病態を表す。
	Case string
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

// NewConfigForChannel は tuner command の channel 引数に対応する Config を作る。
// conformance の解放遅延テストでは、同じ fixture tuner バイナリを複数の channel 定義から
// 起動して、mirakc に別々のサービス（別々のチャンネル）として扱わせる。既存の正常系は
// 引数なしで NewConfig を使うため、service ID は従来どおり変わらない。
func NewConfigForChannel(channel string) Config {
	cfg := NewConfig()
	n, err := strconv.ParseUint(channel, 10, 16)
	if err == nil && n <= uint64(^uint16(0)-100) {
		cfg.ServiceID = uint16(n + 100)
	}
	return cfg
}

// NewConfigForCase は病態ケース用の Config を作る。
//
// 病態ケースは EPG 収集と録画で別々に起動されるチューナープロセスをまたいで
// 同じ放送を表す必要がある。そのため通常系のように「プロセス起動時からの相対時刻」
// ではなく、壁時計上の次の固定境界を使う。
func NewConfigForCase(name string) Config {
	cfg := NewConfig()
	cfg.Case = name
	return cfg
}

// caseEventStart は JST の暦値で 30 秒周期の xx:10 をイベント開始時刻にする。
// xx:10 は EPG bootstrap の完了後にも短い準備時間を確保しつつ、ケースごとの待ち時間を
// bounded にするための値である。入力・出力とも jstNow と同じ「JST 暦を UTC location
// に載せた time.Time」規約。
func caseEventStart(now time.Time) time.Time {
	start := now.Truncate(30 * time.Second).Add(10 * time.Second)
	if !start.After(now) {
		start = start.Add(30 * time.Second)
	}
	return start
}

// activeCaseEventStart は現在の放送周期がまだ継続中ならその開始時刻を返し、周期の
// 境界を過ぎていれば次の開始時刻を返す。EPG 用と録画用のプロセスが同じ周期を選べる
// ように、プロセス起動時刻ではなく現在時刻から毎回導出する。
func activeCaseEventStart(now time.Time) time.Time {
	start := now.Truncate(30 * time.Second).Add(10 * time.Second)
	if now.Before(start.Add(pathologyDuration)) {
		return start
	}
	return caseEventStart(now)
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
				present, following := currentEvents(cfg, jstNow())
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
				events := scheduleEvents(cfg, jstNow())
				sec := buildEIT(0x50, cfg.ServiceID, cfg.TSID, cfg.NetworkID, 0, 0, 0, 0x50, events)
				if err := write(packetizeSection(pidEIT, sec, &ccEIT)); err != nil {
					return err
				}
			}
		}
	}
}

// currentEvents は病態ケースの EIT[p/f] を返す。正常系は従来の相対時刻フィクスチャを
// そのまま使う。病態ケースでは、別プロセスとして起動される EPG 収集と録画が同じ壁時計
// 上のイベントを見られることが重要なので、毎回現在時刻から状態を導出する。
func currentEvents(cfg Config, now time.Time) (eitEvent, eitEvent) {
	if cfg.Case == "" {
		present := eitEvent{EventID: cfg.EventID, Start: cfg.EventStart, Duration: cfg.EventDuration, Name: cfg.EventName}
		following := eitEvent{
			EventID:  cfg.FollowingEventID,
			Start:    cfg.EventStart.Add(cfg.EventDuration),
			Duration: time.Hour,
			Name:     cfg.EventName + " (following)",
		}
		return present, following
	}

	start := activeCaseEventStart(now)
	target := eitEvent{EventID: cfg.EventID, Start: start, Duration: pathologyDuration, Name: cfg.EventName, RunningStatus: 4}
	next := eitEvent{EventID: cfg.FollowingEventID, Start: start.Add(pathologyDuration), Duration: time.Hour, Name: cfg.EventName + " (following)", RunningStatus: 1}
	previous := precedingEvent(cfg, start)

	switch cfg.Case {
	case CasePrecedingExtension:
		if now.Before(start) {
			target.RunningStatus = 1
			return previous, target
		}
		return target, next
	case CaseRunningStatus:
		if now.Before(start) {
			target.RunningStatus = 2
		}
		return target, next
	case CaseFollowing:
		target.RunningStatus = 1
		return previous, target
	case CaseEventIDReset:
		if now.Before(start) {
			target.RunningStatus = 1
			return previous, target
		}
		replacement := eitEvent{EventID: ReplacementEventID, Start: start, Duration: pathologyDuration, Name: cfg.EventName + " (replacement)", RunningStatus: 4}
		return replacement, next
	default:
		return target, next
	}
}

// precedingEvent は病態ケースの「前番組」を表す EIT イベントを返す。EIT p/f
// （currentEvents）と EIT schedule（scheduleEvents）の両方がここを通ることで、同一
// event_id（PrecedingEventID）に 2 つの尺を書いてしまう食い違いを構造的に防ぐ
// --- 尺が食い違うと mirakc の EPG マージでどちらが勝つかは未規定になる。
func precedingEvent(cfg Config, start time.Time) eitEvent {
	previous := eitEvent{EventID: PrecedingEventID, Start: start.Add(-30 * time.Second), Duration: 30 * time.Second, Name: cfg.EventName + " (preceding)", RunningStatus: 4}
	switch cfg.Case {
	case CasePrecedingExtension:
		previous.Duration = UndefinedDuration
	case CaseFollowing:
		previous.Duration = 60 * time.Second
	}
	return previous
}

// scheduleEvents は EIT schedule に載せるイベントを返す。前番組も一緒に載せることで、
// mirakc のスケジュール更新が present/following だけに依存していないことも検査できる。
func scheduleEvents(cfg Config, now time.Time) []eitEvent {
	if cfg.Case == "" {
		return []eitEvent{{EventID: cfg.EventID, Start: cfg.EventStart, Duration: cfg.EventDuration, Name: cfg.EventName}}
	}
	start := activeCaseEventStart(now)
	target := eitEvent{EventID: cfg.EventID, Start: start, Duration: pathologyDuration, Name: cfg.EventName, RunningStatus: 4}
	if cfg.Case == CaseEventIDReset && !now.Before(start) {
		return []eitEvent{{EventID: ReplacementEventID, Start: start, Duration: pathologyDuration, Name: cfg.EventName + " (replacement)", RunningStatus: 4}}
	}
	if cfg.Case == CaseRunningStatus {
		return []eitEvent{target}
	}
	return []eitEvent{precedingEvent(cfg, start), target}
}
