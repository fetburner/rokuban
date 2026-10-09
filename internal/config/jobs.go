package config

// ingest / EPG / ruler / reconciler / worker の設定。

import (
	"fmt"
	"time"
)

// IngestConfig は ingest ジョブの設定。
type IngestConfig struct {
	// Concurrency は mirakc サイトあたりの ingest 同時実行数（ingest キューの
	// MaxWorkers）。**既定値の権威はここ（defaults() の 3）である**
	// （internal/worker.defaultIngestConcurrency は config.Load を経由しない
	// 呼び出し元のための二重既定）。
	//
	// 3 は docs/recording/ingest.md §5.4 の式（`チューナー数 + 全速 pull の
	// 許容本数（1〜2）`）の下端で、2 チューナー機の「2 本同時録画 + 全速 pull 1 本」
	// にあたる。§5.4 の窓（録画数ちょうどだと全速 pull が詰まる / 広げすぎると
	// 復旧中の全速 pull がエッジの録画書き込みと競合する）の中に収まっている。
	//
	// **足りないと追従が枠待ちになる。** 枠が録画数を下回ると、録画中の追従が
	// MaxWorkers の待ち行列に入り、UI には `pending`（取り込み待ち）が続く。
	// 4 チューナー機では 5〜6 に上げる（docs/recording/ingest.md §5.4 の式のまま）。
	// **接続プールの予算はこれに自動で追随する** --- job lock の本数は
	// internal/worker.LockSlots が設定から数え、internal/db がそこから worker の
	// 予算を導出する。
	//
	// 枠待ちの間に録画が終わったジョブは、録画終了後に始まる pull（finished を観測したら
	// 最後まで全速で読む）として走る（ingest は追従と完了後 pull を同じ
	// ループで処理する。finished の record を最初から pull して commit する
	// TestIngestWorker_FullTransfer が根拠）。
	Concurrency int `yaml:"concurrency"`

	// StallTimeout は転送中の無進捗検知タイムアウト。進捗がこの時間止まると
	// 切断扱いにして Range 再開する（River の総時間タイムアウトは無効化している
	// ため、これが ingest の唯一のタイムアウト）。既定値 30 秒は defaults() が
	// 埋める（worker 側に二重の既定は無い。0 以下は validate が起動時に弾く ---
	// 0 と負値のどちらも StallReader を即発火させる。validate のコメント参照）。
	StallTimeout time.Duration `yaml:"stall_timeout"`
}

// validate は ingest 設定のうち、値の範囲で決まるものを検査する（Load 時）。
//
// Concurrency < 1 を弾くのは encode と同形である（EncodeConfig.validate）。
// 0 を「既定に寄せる」のは defaults() の役目で、defaults() を通っていない値
// （明示された 0 や負値）がここへ来る。0 を worker 側で既定に読み替えると、
// 「設定したのに効かない」と「設定を消した」が同じ結果になる。
//
// StallTimeout <= 0 を通すと、worker 側は素通りでそのまま
// time.AfterFunc(0 または負, stallCancel) に渡す（IngestWorker.Work の
// StallTimeout 参照）。time.AfterFunc は d <= 0 を「即時実行」として扱う
// （手元で `time.AfterFunc(-1*time.Second, ...)` を実行して確認済み。panic は
// しない）ので、0 でも負でも stallCancel が接続直後に即発火し、ingest が
// 打ち切られて再試行し続ける。worker 側が 0 を既定に読み替えていた（旧
// resolveStallTimeout）のをやめた分、その検証をここに移す。
func (c IngestConfig) validate() error {
	if c.Concurrency < 1 {
		return fmt.Errorf("ingest.concurrency must be >= 1, got %d", c.Concurrency)
	}
	if c.StallTimeout <= 0 {
		return fmt.Errorf("ingest.stall_timeout must be > 0, got %s", c.StallTimeout)
	}
	return nil
}

// EpgConfig は EPG プロジェクションの設定。
type EpgConfig struct {
	// SyncInterval は mirakc から EPG を全量取得する間隔。
	SyncInterval time.Duration `yaml:"sync_interval"`

	// RetentionGrace は放送終了からこの時間が経った番組をローリングウィンドウから
	// 刈り取る猶予。既定値 24 時間は defaults() が埋める（worker 側に二重の既定は
	// 無い。0 以下は validate が起動時に弾く）。
	RetentionGrace time.Duration `yaml:"retention_grace"`
}

// validate は EPG 設定のうち、値の範囲で決まるものを検査する（Load 時）。
//
// RetentionGrace <= 0 を通すと EpgSyncWorker.Work は mark.Add(-grace) をそのまま
// 使う（RetentionGrace のコメント参照）。0 は「猶予なし」（放送終了直後の番組を
// 即座に刈り取る。ローリングウィンドウの意図から外れる）、負値はさらに悪く
// mark より未来の EndAt を切る（mark.Add(-(-1h)) = mark+1h）ため、**まだ放送中の
// 番組まで EPG 射影から刈り取られる**。worker 側が <= 0 を既定に読み替えていた
// （旧 defaultEpgRetentionGrace フォールバック）のをやめた分、その検証をここに
// 移す。
func (c EpgConfig) validate() error {
	if c.RetentionGrace <= 0 {
		return fmt.Errorf("epg.retention_grace must be > 0, got %s", c.RetentionGrace)
	}
	return nil
}

// RulerConfig は ruler（ルール評価パス）の設定。
type RulerConfig struct {
	// MaxDeletesPerPass は 1 サイト・1 パスあたりの導出削除許容数（大量削除サーキット
	// ブレーカーの閾値。internal/ruler.Config.MaxDeletesPerPass、docs/recording.md
	// §3.2「大量削除サーキットブレーカー」）。超えたら削除を一切実行せず発動し、
	// 手動で再開するまで止まり続ける（ラッチ。issue #24 M2-5）。
	// 0 なら ruler 側の既定値（50）を使う。
	MaxDeletesPerPass int `yaml:"max_deletes_per_pass"`

	// RetractGrace は放送開始直前にルールから外れた予約を、このパスでは削除しない
	// 猶予（internal/ruler.Config.RetractGrace、docs/recording/ruler.md §3.1「直前
	// unmatch の猶予」）。番組表は放送直前まで書き換わるため、猶予が無いと題名の
	// 1 文字修正のような無害な変更で録り逃す経路が開く（denpa の `RULE_RETRACT_GRACE`
	// と同じ動機。「手違いで消す方が余分に録るより高い」）。
	//
	// **ポインタ**: 未設定（yaml にキーが無い）と明示的な 0 を区別する必要がある。
	// 未設定は既定 1h（defaults() が埋める）、明示的な 0 は猶予そのものを無効化する
	// （EncodeProfile.HWAccel と同じ goccy/go-yaml の nil ポインタ規約。上記コメント
	// 参照）。値型 time.Duration だと 0 が「未設定」と「無効化」のどちらにも読めて
	// 区別できない。ruler パッケージ自身は受け取った値をそのまま使うだけで、1h と
	// いう既定値は知らない（internal/ruler.defaultConfig のコメント参照）
	// --- 既定値の主体はこの config 層にある。
	RetractGrace *time.Duration `yaml:"retract_grace"`
}

// validate は ruler 設定のうち、値の範囲で決まるものを検査する（Load 時）。
func (c RulerConfig) validate() error {
	if c.RetractGrace != nil && *c.RetractGrace < 0 {
		return fmt.Errorf("ruler.retract_grace must be >= 0, got %s", *c.RetractGrace)
	}
	return nil
}

// ReconcilerConfig は reconciler（宣言的同期パス）の設定。
type ReconcilerConfig struct {
	// StartDelayGrace は開始遅延検出器（internal/reconciler.Config.StartDelayGrace、
	// docs/recording.md §3.3「開始遅延検出器」）の猶予。開始時刻からこの時間が
	// 経っても recordings.started_at が観測されない予約を「開始遅延」として
	// 検出し、slog.Error とゲージ（rokuban_reconcile_start_delayed）に出す。
	// mirakc 側の未知の不具合への保険（EPGStation#724 の実例あり）。
	// 0 なら reconciler 側の既定値（3 分）を使う。
	StartDelayGrace time.Duration `yaml:"start_delay_grace"`
}

// WorkerConfig は worker ロールの River クライアント設定。
type WorkerConfig struct {
	// PeriodicJobs はプロセス内で定期ジョブ（epg_sync / ruler_pass / reconcile_pass /
	// record_sweep）を投入するか。
	// k8s では false にし、CronJob から `rokuban enqueue` で投入する。
	// River の PeriodicJobs はリーダーに選出されたクライアントだけが投入するため、
	// worker を KEDA で 0 にスケールすると誰も投入しなくなる（docs/data.md §2
	// 「定期実行の契機はデプロイ形態に委ねる」）。
	PeriodicJobs bool `yaml:"periodic_jobs"`

	// Queues は引くキューを絞る。空なら全部。ロールを増やさずに「ruler / reconciler だけ別 Pod」を
	// 実現するための knob（docs/overview.md「ロールは『プロセスの形』を表し、
	// 『どの仕事をするか』は表さない」）。未知のキュー名は起動時エラーになる。
	Queues []string `yaml:"queues"`

	// RescueStuckJobsAfter は River の JobRescuer が running のまま残ったジョブを
	// 死んだ実行とみなすまでの時間（river.Config.RescueStuckJobsAfter）。0 なら
	// River の既定（1h）。個別の Timeout() がこれより長い kind は、その Timeout を
	// 待ってから rescue される（Timeout() が -1 の kind は rescue されない）。
	// 正で 1 分未満だと worker が起動しない（River は RescueStuckJobsAfter >= JobTimeout を
	// 要求し、rokuban は JobTimeout を設定しないので River の既定 1 分が効く）。
	// k8s で rescuer を誰が動かすかは docs/data/jobs.md §2。
	RescueStuckJobsAfter time.Duration `yaml:"rescue_stuck_jobs_after"`
}
