package mirakc

// mirakc の recording schedule が取りうる state。docs/schema.md §4 のとおり
// mirakc 側の enum は追加に追従するため schedule_sync.state に CHECK 制約は
// 付けず text のまま持つが、reconciler が「安全に触ってよい状態」を
// allowlist で判定する必要があるため、既知の値のうち参照する必要があるものを
// ここに定数化する（docs/recording.md §3.2）。
const (
	// ScheduleStateScheduled は録画開始前で、まだ実際の放送への追従に入っていない
	// 状態。reconciler が予約オプションの差分反映（DELETE→POST の再作成）を
	// 許すのはこの状態のときだけ。
	//
	// 他の状態（tracking / recording / rescheduling / finished / failed、および
	// 将来 mirakc が追加しうる未知の値）はすべて再作成の対象外とする
	// （blocklist ではなく allowlist にしているのは、mirakc が状態を増やした
	// ときに安全側 = 触らない側に倒れるため）。
	ScheduleStateScheduled = "scheduled"

	// ScheduleStateTracking は EPG 上の開始/終了予定時刻ではなく、実際の放送の
	// PSI/SI を見て開始/終了を追跡している状態。
	ScheduleStateTracking = "tracking"

	// ScheduleStateRecording は録画を実行中の状態。
	ScheduleStateRecording = "recording"

	// ScheduleStateRescheduling は延長等で開始/終了時刻の再追従を行っている状態。
	ScheduleStateRescheduling = "rescheduling"

	// ScheduleStateFinished は録画が正常に終了した状態。
	ScheduleStateFinished = "finished"

	// ScheduleStateFailed は録画が失敗として終了した状態。
	ScheduleStateFailed = "failed"
)
