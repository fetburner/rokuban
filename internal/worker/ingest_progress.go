package worker

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ingestProgressInterval は転送中に recording_ingest_progress を書き直す最短
// 間隔（issue #212）。
//
// 秒オーダーにしているのは、この観測の用途が「止まっているのか進んでいるのか」
// の判別だけだから。ingest の同時実行数は mirakc サイト単位で 1〜2 にキャップ
// されている（docs/recording/ingest.md §5.4）ので、この頻度でも DB への書き込みは
// 高々 1 秒あたり 1 行程度にしかならない。
//
// 上限は既定のストール検知（config.ingest.stall_timeout の既定値 30 秒）より
// 十分短くする ---
// ストール検知が働いて接続を切り直すより前に「進んでいない」が観測できないと、
// UI は停滞をストール検知の後追いでしか知れない。
const ingestProgressInterval = 2 * time.Second

// ingestProgressReporter は転送中の書き込みバイト数を
// recording_ingest_progress に写す（issue #212）。
//
// **書き込みの失敗で ingest を落とさない。** この表は進捗の可視化のためだけに
// あり、コミット（= media_assets 行。不変条件 3）とは独立している。ここで
// エラーを返すと「進捗が書けなかった」という表示の都合で、転送済みのバイトを
// 捨てて最初からやり直すことになる。
//
// 呼び出し元（IngestWorker.Work）は start で転送開始を記録してから、
// progressWriter 経由で継続的に report する。行の存在そのものが「転送中」の
// 主張なので（不変条件 10）、0 バイトの start にも意味がある。
type ingestProgressReporter struct {
	pool        *pgxpool.Pool
	recordingID int64
	// expectedBytes は進捗の分母。Work 開始時は record_sync.content_length
	// （watcher の観測）で初期化し、追従中は observeProgress が GetRecord の
	// content.length で更新する（固定すると録画中に 100% で止まる。issue #425）。
	// mirakc が length を返していなければ nil のままにする（でっち上げた分母を
	// 置かない）。
	expectedBytes *int64
	interval      time.Duration
	log           *slog.Logger

	// lastAt は直近に進捗を DB へ書こうとした時刻。ゼロ値は「まだ進捗を
	// 1 度も書いていない」。転送開始を表す start の書き込みは進捗ではないので
	// この時計を進めない。
	lastAt time.Time
}

// start は転送開始を記録する。再開時は既存 temp の replay 済みバイト数を渡す。
// これは進捗の観測ではないため、report の間引き時計を進めない。
// 引数を省略した場合は新規 temp の 0 バイトとして扱う。
func (r *ingestProgressReporter) start(ctx context.Context, initial ...int64) {
	written := int64(0)
	if len(initial) > 0 {
		written = initial[0]
	}
	r.write(ctx, written)
}

// report は written バイト書けたことを記録する。前回の進捗書き込みから interval
// 未満しか経っていなければ何もしない。
//
// io.Copy の 1 バッファ（32KiB）ごとに呼ばれる想定なので、間引きはここで行う。
func (r *ingestProgressReporter) report(ctx context.Context, written int64) {
	now := time.Now()
	if !r.lastAt.IsZero() && now.Sub(r.lastAt) < r.interval {
		return
	}
	r.lastAt = now
	r.write(ctx, written)
}

// flush は間隔を無視して最新の written バイトを記録し、その時刻から次の
// report の間引き間隔を数え直す。
func (r *ingestProgressReporter) flush(ctx context.Context, written int64) {
	r.lastAt = time.Now()
	r.write(ctx, written)
}

// write は進捗行を書き直す。失敗はログだけに残し、ingest 本体を失敗させない。
func (r *ingestProgressReporter) write(ctx context.Context, written int64) {
	// ctx が既に死んでいるなら書きに行かない（ジョブのキャンセル時に無駄な
	// クエリを投げない）。
	if ctx.Err() != nil {
		return
	}
	if err := sqlcgen.New(r.pool).UpsertRecordingIngestProgress(ctx, sqlcgen.UpsertRecordingIngestProgressParams{
		RecordingID:   r.recordingID,
		WrittenBytes:  written,
		ExpectedBytes: r.expectedBytes,
	}); err != nil {
		// 進捗が書けないこと自体は転送の失敗ではない。ログだけ残して続ける。
		r.log.Warn("ingest: failed to record transfer progress",
			"recording_id", r.recordingID, "written_bytes", written, "err", err)
	}
}

// progressWriter は下位 Writer への書き込みバイト数を数え、書けたぶんだけ
// onWrite に通知する io.Writer。
//
// written は**このジョブが扱う temp の先頭からの累計**で、ジョブ内リトライ
// （層 1、Range 再開）だけでなくプロセス死後の replay（層 2）も跨いで積み上がる。
// 中身が悪いと確定して temp を消した次の試行だけは 0 から始まる
// （docs/recording/ingest.md §5.3）。
type progressWriter struct {
	w       io.Writer
	written int64
	onWrite func(written int64)
}

// Write は下位 Writer に書いてから、書けた累計を onWrite に渡す。
//
// 通知は下位の書き込みが**成功したぶんだけ**（n バイト）で行う。エラー時にも
// n > 0 なら通知するのは、その n バイトは実際にファイルに書けているため
// （io.Copy と同じ規約）。
func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.written += int64(n)
		p.onWrite(p.written)
	}
	return n, err
}
