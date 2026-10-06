/* Shared session lifecycle, ffmpeg execution, and input probing. */

package streamer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/metrics"
)

// liveUpstreamStartError は mirakc の stream 要求が拒否された起動失敗を表す。
// ffmpeg の起動失敗などとは、idle セッションを退避して再試行できる点が異なる。
type liveUpstreamStartError struct {
	err error
}

func (e *liveUpstreamStartError) Error() string {
	return fmt.Sprintf("requesting mirakc live stream: %v", e.err)
}

func (e *liveUpstreamStartError) Unwrap() error {
	return e.err
}

// liveMirakcReleaseWait は、退避したセッションの mirakc 接続を Close して
// `<-s.done` を待った後、1 回だけ再試行する前の待ち時間。実 mirakc
// 4.0.0-dev.0 + fixture tuner 2 本 + 録画 1 本で、旧ライブの Close から次の
// 異なる波のライブ要求が通るまでを測ったところ 2.35〜4.18 秒だった（2026-09-06、
// internal/mirakc/conformance/live_release_test.go）。5 秒にして、mirakc 側の
// 非同期な tuner プロセス終了の揺れを吸収する。ここは再試行の回数を増やすための
// backoff ではなく、退避後に 1 回だけ行う解放待ちである。
//
// **ポーリングではなく固定待ちにしているのは単純さを取った選択。** 典型的な解放は
// 2.35 秒で終わる（上記実測の最小値）が、固定 5 秒はその典型ケースで最大 2.6 秒を
// 余分に払う。予算内で 100ms 間隔のポーリングに変える案もあるが、この PR の範囲
// （issue #677 の「再試行は 1 回だけ」---反復禁止であって解放待ちの実装方式では
// ない）を広げない。判定手段は internal/mirakc/conformance/live_release_test.go
// に既にある（100ms ポーリングで解放を検出している）ので、ポーリング化するときは
// そこを使って測り直す。
//
// var にしてあるのはテストからの上書き用（playlistStartupTimeout と同じ理由 ---
// 5 秒の実待ちはテストを不必要に遅くする）。運用者向けの設定キーではない。
var liveMirakcReleaseWait = 5 * time.Second

const playlistPollInterval = 100 * time.Millisecond

type sessionKind string

type sessionKey struct {
	kind          sessionKind
	id            int64
	offsetSeconds int64
}

type sessionSource func(context.Context) (io.ReadCloser, error)

// liveSession はライブまたは追っかけ再生の 1 セッション（1 mirakc 接続 +
// 1 ffmpeg プロセス）。種別だけが資源の入口と出力ディレクトリを変え、上限・
// startup wait・idle GC・leave は同じ状態機械を通る。
//
// crash-only の唯一の例外（使い捨てのインメモリ状態）。DB には一切書かない。
type liveSession struct {
	serviceID int64
	key       sessionKey
	source    sessionSource
	dir       string // SegmentDir/site/{serviceID|chase/recordingID/offset/seconds}

	ready chan struct{} // startSession が終わったら閉じる（成功でも失敗でも）
	done  chan struct{} // ffmpeg プロセスが完全に終了したら閉じる

	startErr error // ready が閉じた後にだけ読む

	cancel context.CancelFunc

	mu         sync.Mutex
	lastAccess time.Time
}

func (s *liveSession) touch() {
	s.mu.Lock()
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

// hintLeave は離脱ヒントを反映する。idle 期限が「now + grace」になるところまで
// lastAccess を**巻き戻す**。
//
// **前へ進める方向には決して動かさない。** grace が idleTimeout 以上の設定
// （あるいは既にもっと古い lastAccess を持つセッション）でこれを無条件に代入
// すると、ヒントが**延命の道具**になる --- 「離れた」と言うだけでセッションを
// 引き延ばせてしまい、意味が反転する。巻き戻しだけを許すことで、ヒントの
// 最悪ケースは「何も起こらない」になる。
//
// この後に誰かが touch() すれば lastAccess は now に戻り、猶予も元の
// idleTimeout に戻る（他の視聴者がいる場合の自己修復。Leave の doc コメント参照）。
//
// 戻り値は**実際に期限を動かしたか**。動かさなかった（＝ヒントが no-op だった）
// ケースは 2 つあり、どちらもメトリクスでは `no_effect` として数える:
// 猶予が IdleTimeout 以上の設定（leaveGrace のコメント参照）と、連打の 2 発目
// 以降（既に詰めた期限より後ろにしか詰められない）。
func (s *liveSession) hintLeave(now time.Time, grace, idleTimeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	shortened := now.Add(grace - idleTimeout)
	if !shortened.Before(s.lastAccess) {
		return false
	}
	s.lastAccess = shortened
	return true
}

// waitReadyTouching は s の起動完了（close(s.ready)）を、**待っている間ずっと
// s を touch しながら**待つ。timeout / ctx.Done で打ち切る。
//
// **待っている客も客である**（issue #191 のレビュー指摘）。ハンドラが
// `<-s.ready` や waitForPlaylist で待っている区間は「誰も要求していない無音区間」
// に見えるが、実際にはそのセッションを待っている視聴者がそこにいる。last-access が
// 止まったままだと、その区間に届いた離脱ヒント（他人のものでも、自分のタブが
// hidden になったものでも）が idle 期限を猶予まで詰め、**起動待ちの視聴者ごと
// セッションが回収される**（実測: 起動待ち 4 秒・猶予 2 秒の構成で、ヒント送出の
// 約 2 秒後に回収され、待っていた視聴者は 504 を受け取った。
// `TestLiveStreamer_LeaveHint_DoesNotKillASessionThatIsStillStartingUp`）。
//
// **GC 側に「起動中は回収しない」という例外を作る形は採らない。** 実測した失敗は
// ready が閉じた**後**のプレイリスト待ちで起きており、「起動中」を ready で
// 判定する例外はそこを覆えない。加えて、例外は「回収されない状態」を新設する
// ので、mirakc がハングして ready が永久に閉じないときにセッションが回収不能に
// なる（チューナーを掴んだまま max_sessions を食い潰す）。ここで touch すれば、
// 真実は last-access 1 つのまま（不変条件 5 のレベルトリガー）で、待ちが
// 終われば自動的に通常の idle 判定に戻る --- 待ちは playlistStartupTimeout で
// 上限が付いているので、これで延命できるのも高々その時間である。
func waitReadyTouching(ctx context.Context, s *liveSession, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(playlistPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ready:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errStartupTimeout
		case <-ticker.C:
			s.touch()
		}
	}
}

func (s *liveSession) idleSince(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.lastAccess)
}

// stop は mirakc 接続と ffmpeg プロセスを止め、終了を待つ。ctx キャンセルで
// io.Reader からの読み取り（ffmpeg の stdin コピー）が中断し、
// exec.CommandContext の既定動作でプロセスが kill される。
func (s *liveSession) stop() {
	s.cancel()
	<-s.done
}

// sessionCount はロックを取って現在のセッション数を返す（メトリクス用）。
func (ls *LiveStreamer) sessionCount() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.sessions) + len(ls.chaseSessions)
}

func sessionKindOf(s *liveSession) sessionKind {
	switch s.key.kind {
	case chaseSessionKind, originalVODSessionKind:
		return s.key.kind
	default:
		return liveSessionKind
	}
}

func sessionIDOf(s *liveSession) int64 {
	if sessionKindOf(s) != liveSessionKind {
		return s.key.id
	}
	return s.serviceID
}

// getSessionLocked returns the session for a complete key. The caller must
// hold ls.mu. Chase offsets are part of the key so two initial positions never
// share an HLS timeline accidentally.
func (ls *LiveStreamer) getSessionLocked(key sessionKey) (*liveSession, bool) {
	if key.kind == chaseSessionKind || key.kind == originalVODSessionKind {
		if ls.chaseSessions == nil {
			return nil, false
		}
		s, ok := ls.chaseSessions[key]
		return s, ok
	}
	if ls.sessions == nil {
		return nil, false
	}
	s, ok := ls.sessions[key.id]
	return s, ok
}

func (ls *LiveStreamer) putSessionLocked(s *liveSession) {
	if sessionKindOf(s) == chaseSessionKind || sessionKindOf(s) == originalVODSessionKind {
		if ls.chaseSessions == nil {
			ls.chaseSessions = make(map[sessionKey]*liveSession)
		}
		ls.chaseSessions[s.key] = s
		return
	}
	if ls.sessions == nil {
		ls.sessions = make(map[int64]*liveSession)
	}
	ls.sessions[s.key.id] = s
}

func (ls *LiveStreamer) deleteSessionLocked(s *liveSession) {
	if sessionKindOf(s) == chaseSessionKind || sessionKindOf(s) == originalVODSessionKind {
		delete(ls.chaseSessions, s.key)
		return
	}
	delete(ls.sessions, s.key.id)
}

func (ls *LiveStreamer) setActiveSessionMetrics() {
	ls.mu.Lock()
	live := len(ls.sessions)
	chase := len(ls.chaseSessions)
	vod := 0
	for key := range ls.chaseSessions {
		if key.kind == originalVODSessionKind {
			vod++
			chase--
		}
	}
	ls.mu.Unlock()
	metrics.LiveActiveSessions.WithLabelValues(string(liveSessionKind)).Set(float64(live))
	metrics.LiveActiveSessions.WithLabelValues(string(chaseSessionKind)).Set(float64(chase))
	metrics.LiveActiveSessions.WithLabelValues(string(originalVODSessionKind)).Set(float64(vod))
}

// getOrCreateSession は serviceID のセッションを返す。無ければ作る。
//
// **同じ serviceID への同時リクエストは 1 本の ffmpeg に収束する。** マップへの
// 挿入をロック内で行い、実際の起動（mirakc 接続 + ffmpeg exec、時間がかかる）は
// ロック外で行う。後発のリクエストは ready チャネルで起動完了を待つだけで、
// 2 本目の ffmpeg を起動しない。
//
// ctx は**セッション自体の生存**（sessionCtx）ではなく、**この呼び出しがどれだけ
// 待つか**にだけ使う。呼び出し元のリクエストが切れても他の同時リクエストが待って
// いる可能性があるセッションの起動を巻き込んで中断しない --- 待つのをやめるだけ。
//
// **`<-s.ready` 待ちは playlistStartupTimeout で打ち切る（issue #286）。** mirakc
// への接続（StreamService、全体タイムアウト無し）がハングすると close(s.ready) に
// 到達せず、ctx（呼び出し元のリクエストの ctx）だけでは呼び出し元が切断するまで
// 戻らない。**この期限は呼び出し元の ctx に `context.WithTimeout` を被せる形では
// 実装しない** --- getOrCreateSessionOnce の 2 か所の select にそれぞれ
// `case <-time.After(...)` を足すだけに留める。ctx を包んで下位の sessionCtx にまで
// 渡してしまうと、この
// 呼び出しの待ちを諦めるだけのつもりが起動中のセッションそのものを巻き込んで
// 中断してしまう（sessionCtx は `context.Background()` 由来で、ctx とは独立して
// いなければならない。issue #189 の罠と同じ形）。
//
// **退避（takeIdleSessionForRetry → stop → 解放待ち）は evictMu で直列化し、
// LiveStreamer 全体で 1 本しか走らない（issue #677 のレビュー指摘）。** 同じ
// serviceID を待っている同時要求は全員が同じ起動失敗を受け取るので、ロックが
// 無いと全員が個別に退避を試み、圧力イベント 1 つに対して要求数ぶんの idle
// セッションを殺してしまう。evictMu を取った直後に `ls.sessions[serviceID]`
// を見て、**既に別の要求が退避と再試行を終えていれば**（先着の再試行が成功して
// 新しいセッションが map に入っていれば）自分は退避せずその成果に相乗りする。
// **evictMu の保持区間に getOrCreateSessionOnce（最大 playlistStartupTimeout の
// 起動待ち）を含めない** --- 含めると、無関係な別サービスへの要求まで他サービスの
// 起動待ちで足止めされる。実際の起動 I/O はロックの外で行う。
func (ls *LiveStreamer) getOrCreateSession(ctx context.Context, serviceID int64) (*liveSession, error) {
	return ls.getOrCreateSessionFor(ctx, sessionKey{kind: liveSessionKind, id: serviceID}, func(ctx context.Context) (io.ReadCloser, error) {
		return ls.mirakc.StreamService(ctx, serviceID, ls.cfg.TunerPriority)
	})
}

func (ls *LiveStreamer) getOrCreateSessionFor(ctx context.Context, key sessionKey, source sessionSource) (*liveSession, error) {
	s, err := ls.getOrCreateSessionOnceFor(ctx, key, source)
	return ls.recoverSessionStartup(ctx, key, source, s, err)
}

func (ls *LiveStreamer) recoverSessionStartup(ctx context.Context, key sessionKey, source sessionSource, s *liveSession, err error) (*liveSession, error) {
	if err == nil {
		return s, nil
	}

	reason, retryable := liveEvictionReason(err)
	if !retryable {
		return nil, err
	}

	// 上流拒否で ready が閉じても、runSession は map からの削除とディレクトリの
	// 掃除を defer で行う。自分の失敗セッションを先に完全終了させないと、再試行が
	// そのセッションを既存セッションとして拾って同じ startErr を返す。
	if s != nil {
		<-s.done
	}

	// 呼び出し元（HTTP リクエスト）が既に切れているなら、退避してまで再試行する
	// 相手がいない。退避は 5 秒強 evictMu を占有するので、無意味な退避で他の
	// 同時要求を待たせない。
	if ctx.Err() != nil {
		return nil, err
	}

	ls.evictMu.Lock()

	// 別の同時要求が既に退避と再試行を終えていたら、退避せずその成果に相乗りする
	// （相乗り経路では eviction counter を計上しない --- 実際に退避したのは
	// 先着の 1 本だけである）。
	ls.mu.Lock()
	_, alreadyRecovered := ls.getSessionLocked(key)
	ls.mu.Unlock()
	if alreadyRecovered {
		ls.evictMu.Unlock()
		return ls.getOrCreateSessionOnceFor(ctx, key, source)
	}

	victim := ls.takeIdleSessionForRetry(time.Now())
	if victim == nil {
		ls.evictMu.Unlock()
		return nil, err
	}

	slog.Info("streamer: evicting idle session before retry",
		"kind", string(sessionKindOf(victim)), "session_id", sessionIDOf(victim), "reason", reason)
	victim.stop()
	// live の runSession は自分で掃除するが、完了済み chase は EVENT playlist
	// を idle GC まで保持するため defer が掃除を意図的に省略する。退去経路では
	// map から既に外れており通常の GC / shutdown が到達できないため、ここで明示的
	// にディレクトリを解放する。live 側に対しても冪等なので共通化する。
	cleanupSessionDir(victim)
	// mirakc は HTTP body の Close と tuner プロセスの解放を同期していない。
	// stop が done まで待っても、直後の要求が容量エラーになる窓が実物で観測された。
	releaseWait := liveMirakcReleaseWait
	if sessionKindOf(victim) == originalVODSessionKind {
		// 原本 VOD はチューナーを持たないので、解放待ちは無意味（シークのたびに
		// 5 秒止まる。実バイナリで測定）。
		releaseWait = 0
	}
	select {
	case <-ctx.Done():
		// **退避は既に起きている**（victim.stop() は完了済み）。ここで諦めるのは
		// このリクエストの再試行だけ --- mirakc の失敗ではないので retry_failed
		// には混ぜず、専用の result で区別する。
		ls.evictMu.Unlock()
		metrics.LiveSessionEvictions.WithLabelValues(reason, "retry_abandoned").Inc()
		return nil, ctx.Err()
	case <-time.After(releaseWait):
	}
	// 再試行セッションは evictMu を放す前に map へ登録する。放した後に登録すると、
	// evictMu を待っていた同時要求がその間に alreadyRecovered を確認して空振りし、
	// victim も既に無いので元のエラーを返してしまう。ready 待ちは evictMu の外で行う。
	retry, retryErr := ls.startSessionOnceFor(key, source)
	ls.evictMu.Unlock()
	if ls.afterEvictRelease != nil {
		ls.afterEvictRelease()
	}
	retry, retryErr = ls.awaitSessionReady(ctx, retry, retryErr)
	if retryErr != nil && retry != nil {
		// 再試行自身が ready 後に失敗した場合も、次の要求が同じ startErr を
		// 拾わないように、そのセッションの後片付けを待ってから返す。
		<-retry.done
	}
	result := "retry_failed"
	if retryErr == nil {
		result = "retry_succeeded"
	}
	metrics.LiveSessionEvictions.WithLabelValues(reason, result).Inc()
	return retry, retryErr
}

func liveEvictionReason(err error) (string, bool) {
	if errors.Is(err, errSessionLimit) {
		return "session_limit", true
	}
	var upstreamErr *liveUpstreamStartError
	if errors.As(err, &upstreamErr) && !errors.Is(err, context.Canceled) {
		return "upstream", true
	}
	return "", false
}

func (ls *LiveStreamer) getOrCreateSessionOnceFor(ctx context.Context, key sessionKey, source sessionSource) (*liveSession, error) {
	s, err := ls.startSessionOnceFor(key, source)
	return ls.awaitSessionReady(ctx, s, err)
}

// awaitSessionReady は startSessionOnceFor が返したセッションの ready を待つ。
// startSessionOnceFor がエラーを返していたら、そのまま返す。
func (ls *LiveStreamer) awaitSessionReady(ctx context.Context, s *liveSession, err error) (*liveSession, error) {
	if err != nil {
		return nil, err
	}
	if err := waitReadyTouching(ctx, s, playlistStartupTimeout); err != nil {
		return nil, err
	}
	if s.startErr != nil {
		return s, s.startErr
	}
	ls.setActiveSessionMetrics()
	return s, nil
}

// startSessionOnceFor は key のセッションが無ければ作って map に登録し、ready は待たずに返す。
// 既にあればそれを返す。登録までを 1 回のロックで行うので、evictMu を持ったまま呼べば
// 「退避した本人の登録」が evictMu を待つ同時要求より先に見える。
func (ls *LiveStreamer) startSessionOnceFor(key sessionKey, source sessionSource) (*liveSession, error) {
	ls.mu.Lock()
	if s, ok := ls.getSessionLocked(key); ok {
		ls.mu.Unlock()
		return s, nil
	}
	if key.kind == chaseSessionKind {
		if err := ls.chaseInputCooldownLocked(key.id); err != nil {
			ls.mu.Unlock()
			return nil, err
		}
	}
	if ls.closed {
		ls.mu.Unlock()
		return nil, errShuttingDown
	}
	if len(ls.sessions)+len(ls.chaseSessions) >= ls.cfg.MaxSessions {
		ls.mu.Unlock()
		metrics.LiveSessionStartFailures.WithLabelValues("session_limit").Inc()
		return nil, errSessionLimit
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	s := &liveSession{
		serviceID:  key.id,
		key:        key,
		source:     source,
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
		lastAccess: time.Now(),
		cancel:     cancel,
	}
	if key.kind == chaseSessionKind || key.kind == originalVODSessionKind {
		// Recording sessions use recordings.id (and chase also includes its offset).
		s.serviceID = 0
	}
	ls.putSessionLocked(s)
	ls.mu.Unlock()
	ls.setActiveSessionMetrics()

	go ls.runSession(sessionCtx, s)
	return s, nil
}

// takeIdleSessionForRetry は起動失敗時に退避するセッションを 1 本選び、選択と
// map からの削除を同じロック内で行う。呼び出し側は返ったセッションの stop を
// 完了させてから再試行する。
//
// ready 前のセッションは、待っているハンドラが playlistStartupTimeout の間 touch
// し続けるため候補から除外する。waiter がいなくなって idleSince が同じ timeout を
// 超えた起動待ちだけは、mirakc を掴んだままのハングとして退避を許す。ready 済みの
// セッションは最長 segment_seconds の 2 倍より長く idle であることを要求し、その
// 中で最も古いものを選ぶ。
//
// **離脱ヒントを受けたセッションがこの規則で最古の候補になるのは
// `idle_timeout > 5 × segment_seconds + 2s` のときに限る（無条件ではない）。**
// ヒント直後の idle 時間は `idle_timeout - leaveGrace` で、これが候補の閾値
// （`2 × segment_seconds`）を上回るには `idle_timeout - (3×segment_seconds+2s) >
// 2×segment_seconds`、すなわち上記の条件が要る（`leaveGrace` の定義そのもの。
// 展開すると `idle_timeout > 5×segment_seconds + 2s`）。既定値（`idle_timeout: 30s` /
// `segment_seconds: 2s`）はこれを満たす（ヒント後 idle 22s > 閾値 4s）。満たさない
// 設定（例: `idle_timeout: 10s` / `segment_seconds: 2s` --- ヒント後 idle 2s < 閾値 4s）
// では、ヒントは退避の候補化には効かない。ただしその設定では idle GC 自体の刻みが
// 短いので（gcInterval が `idle_timeout` にも連動する）、露出は限定される ---
// `TestLiveStreamer_EvictionCandidate_PrefersLeaveHint`（成立域）と
// `TestLiveStreamer_EvictionCandidate_HintDoesNotQualifyBelowThreshold`（不成立域）が
// 両側を固定する。
func (ls *LiveStreamer) takeIdleSessionForRetry(now time.Time) *liveSession {
	threshold := ls.cfg.idleEvictionThreshold()
	ls.mu.Lock()
	var victim *liveSession
	var oldest time.Duration
	for _, s := range ls.sessions {
		idle := s.idleSince(now)
		if idle <= threshold {
			continue
		}
		if !sessionReady(s) && idle <= playlistStartupTimeout {
			continue
		}
		if victim == nil || idle > oldest {
			victim = s
			oldest = idle
		}
	}
	for _, s := range ls.chaseSessions {
		idle := s.idleSince(now)
		if idle <= threshold {
			continue
		}
		if !sessionReady(s) && idle <= playlistStartupTimeout {
			continue
		}
		if victim == nil || idle > oldest {
			victim = s
			oldest = idle
		}
	}
	if victim != nil {
		ls.deleteSessionLocked(victim)
	}
	ls.mu.Unlock()

	if victim != nil {
		ls.setActiveSessionMetrics()
	}
	return victim
}

func sessionReady(s *liveSession) bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

// runSession は 1 セッションの全生涯（mirakc 接続 → ffmpeg 起動 → 終了待ち →
// 後片付け）を担う。呼び出し元は go で起動し、s.ready / s.done で同期する。
func (ls *LiveStreamer) runSession(ctx context.Context, s *liveSession) {
	kind := sessionKindOf(s)
	keepCompletedRecordingSession := false
	inputFailed := false
	// close(s.done) は必ず最後（他の全ての後片付けの後）に行う。stop() は
	// `<-s.done` が閉じたら「片付け完了」とみなして戻るので、途中の状態
	// （map から消す前・ディレクトリを消す前）で閉じると、呼び出し側が
	// 「もう消えている」つもりで見に行った os.Stat がまだ古いディレクトリを
	// 見つけてしまう（実際にテストで踏んだ競合）。
	defer close(s.done)
	defer func() {
		ls.mu.Lock()
		if inputFailed {
			ls.recordFailedChaseInputLocked(s.key.id)
		}
		// idle GC が先にこの id を削除して新しいセッションに入れ替えていたら、
		// 新しいセッションを消さない（cur == s のときだけ削除）。
		if !keepCompletedRecordingSession {
			if cur, ok := ls.getSessionLocked(s.key); ok && cur == s {
				ls.deleteSessionLocked(s)
			}
		}
		ls.mu.Unlock()
		if !keepCompletedRecordingSession && s.dir != "" {
			cleanupSessionDir(s)
		}
		ls.setActiveSessionMetrics()
	}()

	dir := filepath.Join(ls.cfg.SegmentDir, ls.site, strconv.FormatInt(sessionIDOf(s), 10))
	switch kind {
	case chaseSessionKind:
		dir = chaseSessionDir(ls.cfg.SegmentDir, ls.site, sessionIDOf(s), s.key.offsetSeconds)
	case originalVODSessionKind:
		dir = originalVODSessionDir(ls.cfg.SegmentDir, ls.site, sessionIDOf(s), s.key.offsetSeconds)
	}
	if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
		s.startErr = fmt.Errorf("creating live segment dir: %w", err)
		metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
		close(s.ready)
		return
	}
	s.dir = dir

	body, err := s.source(ctx)
	if err != nil {
		if kind == originalVODSessionKind {
			s.startErr = err
			metrics.LiveSessionStartFailures.WithLabelValues("original_vod_error").Inc()
		} else if errors.Is(err, errChaseRecordNotReadyTimeout) {
			s.startErr = err
			metrics.LiveSessionStartFailures.WithLabelValues("record_not_ready_timeout").Inc()
		} else {
			s.startErr = &liveUpstreamStartError{err: err}
			metrics.LiveSessionStartFailures.WithLabelValues("upstream_error").Inc()
		}
		// 範囲外の offset は利用者入力の結果で、サーバーの障害ではない（416 になる）。
		level := slog.LevelError
		if errors.Is(err, errOriginalVODOffsetUnavailable) {
			level = slog.LevelInfo
		}
		slog.Log(ctx, level, "streamer: requesting session upstream",
			"kind", string(kind), "session_id", sessionIDOf(s), "err", err)
		close(s.ready)
		return
	}
	defer func() { _ = body.Close() }()

	var originalFile *os.File
	if kind == originalVODSessionKind {
		var ok bool
		originalFile, ok = body.(*os.File)
		if !ok {
			s.startErr = errors.New("original VOD source is not a seekable file")
			metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
			close(s.ready)
			return
		}
	}

	input, streamInfo, err := probeLiveSessionInput(ctx, ls.cfg.FFprobe, kind, sessionIDOf(s), body, originalFile)
	if err != nil {
		s.startErr = err
		close(s.ready)
		return
	}
	defer func() { _ = input.Close() }()
	captionInput := ls.cfg.Captions && streamInfo.hasSubtitles
	audioStreamCount := streamInfo.audioStreams

	playlistType := hlsLivePlaylist
	inputPath := "pipe:0"
	var offsetSeconds int64
	switch kind {
	case chaseSessionKind:
		playlistType = hlsEventPlaylist
	case originalVODSessionKind:
		playlistType = hlsOriginalEventPlaylist
		inputPath = originalVODFFmpegInputPath
		offsetSeconds = s.key.offsetSeconds
	}
	args := buildHLSFFmpegArgsForPlaylistType(
		ls.cfg, dir, captionInput, playlistType, inputPath, offsetSeconds, audioStreamCount,
	)
	cmd := exec.CommandContext(ctx, ls.cfg.FFmpeg, args...)
	if originalFile != nil {
		// Go maps ExtraFiles[0] to child fd 3. Passing the already-open original
		// keeps ffmpeg's seekable input alive if until_encoded unlinks its name.
		cmd.ExtraFiles = []*os.File{originalFile}
	}
	var chaseInput *chaseInputCopy
	switch {
	case originalFile != nil:
	case kind == chaseSessionKind:
		if chaseInput, s.startErr = attachChaseInput(cmd); s.startErr != nil {
			metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
			close(s.ready)
			return
		}
	default:
		cmd.Stdin = input
	}
	stderr := newCappedWriter(stderrCap)
	cmd.Stderr = stderr
	// ctx がキャンセルされてプロセスを kill した後、I/O をコピーするゴルーチン
	// （cmd.Stdin 用の内部パイプ）が終わるまで Wait は最大この時間だけ待つ。
	// ffmpeg が孫プロセスを fork していて標準入出力の fd を握ったまま残ると
	// （通常は起きないが）、Wait が無期限にブロックしうる。stop() は
	// idle GC / shutdown から呼ばれるので、ここが詰まるとチューナー解放も
	// 詰まる --- 上限を設けて必ず前に進めるようにする。
	cmd.WaitDelay = 5 * time.Second

	startErr := cmd.Start()
	chaseInput.started(startErr)
	if err := startErr; err != nil {
		s.startErr = fmt.Errorf("starting live ffmpeg: %w", err)
		metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
		close(s.ready)
		return
	}

	slog.Info("streamer: session started", "kind", string(kind), "session_id", sessionIDOf(s), "dir", dir,
		"profiles", len(ls.cfg.Profiles))
	close(s.ready)

	chaseInput.copy(input, cmd.Process.Kill)

	waitErr := cmd.Wait()
	inputErr := chaseInput.finish(body)
	inputFailed = kind == chaseSessionKind && inputErr != nil && ctx.Err() == nil
	ffmpegCompleted := ffmpegSessionCompleted(ctx, cmd, waitErr, inputErr, kind, sessionIDOf(s), stderr)
	if (kind == chaseSessionKind || kind == originalVODSessionKind) && ctx.Err() == nil && ffmpegCompleted {
		// Keep completed recording playlists and all segments until the shared idle
		// GC reclaims the session, so clients can fetch ENDLIST and seek the full VOD.
		keepCompletedRecordingSession = true
	}
}

// probeLiveSessionInput reads a finite input prefix, probes its audio/subtitle streams, and
// returns a reader that replays the prefix before the remaining live input. For an opened
// original VOD file it uses ReadAt so ffmpeg retains the seekable input at offset zero.
func probeLiveSessionInput(
	ctx context.Context,
	ffprobe string,
	kind sessionKind,
	sessionID int64,
	body io.ReadCloser,
	originalFile *os.File,
) (io.ReadCloser, liveStreamInfo, error) {
	input := body
	var prefix []byte
	var readErr error
	if originalFile != nil {
		prefix = make([]byte, liveStreamProbeBytes)
		n, err := originalFile.ReadAt(prefix, 0)
		prefix = prefix[:n]
		readErr = err
	} else {
		input, prefix, readErr = readLiveStreamPrefix(ctx, body, liveStreamProbeBytes, liveStreamProbeWait)
	}
	if ctx.Err() != nil {
		_ = input.Close()
		return nil, liveStreamInfo{}, ctx.Err()
	}
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		slog.Warn("streamer: reading probe prefix failed; using single audio ES fallback",
			"kind", string(kind), "session_id", sessionID, "err", readErr)
	}
	streamInfo, err := probeLiveStreamInfo(ctx, ffprobe, prefix)
	if err != nil {
		if ctx.Err() != nil {
			_ = input.Close()
			return nil, liveStreamInfo{}, ctx.Err()
		}
		slog.Warn("streamer: probing live stream failed; using single audio ES fallback",
			"kind", string(kind), "session_id", sessionID, "err", err)
		streamInfo.audioStreams = 1
	}
	return input, streamInfo, nil
}

// ffmpegSessionCompleted は ffmpeg が完走したか（ENDLIST を書いて正常終了したか）を判定し、
// 終わり方をログに残す。inputErr は追っかけの入力のエラー（chaseInputCopy.finish）。
func ffmpegSessionCompleted(ctx context.Context, cmd *exec.Cmd, waitErr, inputErr error, kind sessionKind, sessionID int64, stderr *cappedWriter) bool {
	// 入力のエラーで kill した（idle GC / shutdown の ctx キャンセルは除く）。ffmpeg の
	// 異常終了ではないので、その旨を 1 回だけ記録する。
	inputFailed := inputErr != nil && ctx.Err() == nil
	if inputFailed {
		slog.Error("streamer: chase input failed; killed ffmpeg so the playlist does not get ENDLIST",
			"session_id", sessionID, "err", inputErr)
		// 入力の失敗と同じころに ffmpeg が自分で落ちていたら（kill より先に終わっていて、
		// 終わり方が SIGKILL でない）、その落ち方も stderr ごと残す
		// （TestFFmpegSessionCompletedKeepsCrashBesideInputFailure）。
		if ffmpegExitedOnItsOwn(waitErr) {
			slog.Error("streamer: ffmpeg exited unexpectedly",
				"kind", string(kind), "session_id", sessionID, "err", waitErr, "stderr", strings.TrimSpace(stderr.String()))
		}
	}
	ffmpegCompleted := waitErr == nil && !inputFailed
	if waitErr != nil && ctx.Err() == nil && !inputFailed {
		if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			ffmpegCompleted = true
			// ffmpeg 自体は exit 0 で完走したが、孫プロセスが stdin/stderr の
			// fd を握ったままで WaitDelay が先に切れた（internal/worker の
			// runEncode / commandOutput と同型のハングの exit 0 版）。正常な
			// セッション終了なので運用者向けの Error にはしない。
			slog.Warn("streamer: ffmpeg exited successfully but WaitDelay expired before I/O completed",
				"kind", string(kind), "session_id", sessionID, "wait_delay", cmd.WaitDelay)
		} else {
			// ctx.Err() == nil ということは idle GC / shutdown による意図した kill ではない
			// ---ffmpeg 自身が落ちた（mirakc 側の切断、コーデックエラー等）。
			slog.Error("streamer: ffmpeg exited unexpectedly",
				"kind", string(kind), "session_id", sessionID, "err", waitErr, "stderr", strings.TrimSpace(stderr.String()))
		}
	}
	return ffmpegCompleted
}

// ffmpegExitedOnItsOwn は Wait のエラーが、こちらの kill（SIGKILL）以外での ffmpeg の
// 異常終了（0 以外の終了コードか、SIGKILL 以外のシグナル）か。
func ffmpegExitedOnItsOwn(waitErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL
}

func cleanupSessionDir(s *liveSession) {
	if s.dir == "" {
		return
	}
	if err := os.RemoveAll(s.dir); err != nil {
		slog.Warn("streamer: segment cleanup failed", "kind", string(sessionKindOf(s)),
			"session_id", sessionIDOf(s), "dir", s.dir, "err", err)
	}
}

// reapIdle は idle timeout を超えたセッションを止める。「クライアント 1 人ごとの
// 生存」ではなく**サービス単位**（docs/api.md §ライブ視聴の HLS）。
//
// **パスの完走を `LiveIdleGCLastPass` に必ず記録する**（何も回収しなかった場合を
// 含む）。docs/operations.md の「ゲージには最後に成功した時刻を対で持つ」規律
// ---LiveActiveSessions だけでは、idle GC ループ自体が死んでいて「セッション数が
// 変わっていない」のか「本当に GC 対象が無かった」のかを区別できない
// （レビューで指摘。issue #91 の受け入れ条件）。
func (ls *LiveStreamer) reapIdle() {
	ls.reapIdleAt(time.Now())
}

// reapIdleAt は reapIdle の本体。「いま」を引数で受けるのは、離脱ヒントで詰めた
// 期限の前後（now+猶予 の直前と直後）をテストが実時間を待たずに踏むため。
func (ls *LiveStreamer) reapIdleAt(now time.Time) {
	defer metrics.LiveIdleGCLastPass.SetToCurrentTime()

	ls.mu.Lock()
	for recordingID, retryAt := range ls.failedChaseInputs {
		if !now.Before(retryAt) {
			delete(ls.failedChaseInputs, recordingID)
		}
	}
	var idle []*liveSession
	for id, s := range ls.sessions {
		if s.idleSince(now) >= ls.cfg.IdleTimeout {
			idle = append(idle, s)
			// 即座にマップから外す。新しい要求が stop() の完了を待たずに
			// 別のセッションを起こせるようにする。
			delete(ls.sessions, id)
		}
	}
	for key, s := range ls.chaseSessions {
		if s.idleSince(now) >= ls.cfg.IdleTimeout {
			idle = append(idle, s)
			// 即座にマップから外す。新しい要求が stop() の完了を待たずに
			// 別のセッションを起こせるようにする。
			delete(ls.chaseSessions, key)
		}
	}
	ls.mu.Unlock()

	if len(idle) == 0 {
		return
	}

	// 並行に stop() する。直列だと 1 本の ffmpeg が kill に応答しない（ハング
	// した子プロセス等）と、他の回収可能なセッションまで足止めされる。
	var wg sync.WaitGroup
	for _, s := range idle {
		wg.Add(1)
		go func(s *liveSession) {
			defer wg.Done()
			slog.Info("streamer: session idle, stopping", "kind", string(sessionKindOf(s)), "session_id", sessionIDOf(s))
			s.stop()
			// A chase session whose ffmpeg already reached EOF keeps its EVENT
			// files until this common idle-GC path. Live cleanup is idempotent.
			cleanupSessionDir(s)
			metrics.LiveIdleGCReclaimed.Inc()
		}(s)
	}
	wg.Wait()

	ls.setActiveSessionMetrics()
}

// shutdown はプロセス停止時に呼ぶ。新規セッションの受付を止め、既存の全セッションを
// 止めて mirakc の接続を閉じる（チューナー解放）。
//
// reapIdle と同じ理由で並行に stop() する（1 本が詰まっても他のチューナー解放を
// 遅らせない。SIGTERM の drain 猶予は有限）。
func (ls *LiveStreamer) shutdown() {
	ls.mu.Lock()
	ls.closed = true
	sessions := make([]*liveSession, 0, len(ls.sessions)+len(ls.chaseSessions))
	for _, s := range ls.sessions {
		sessions = append(sessions, s)
	}
	for _, s := range ls.chaseSessions {
		sessions = append(sessions, s)
	}
	ls.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *liveSession) {
			defer wg.Done()
			s.stop()
			cleanupSessionDir(s)
		}(s)
	}
	wg.Wait()
	ls.mu.Lock()
	for _, s := range sessions {
		if cur, ok := ls.getSessionLocked(s.key); ok && cur == s {
			ls.deleteSessionLocked(s)
		}
	}
	ls.mu.Unlock()
	ls.setActiveSessionMetrics()
}

// stderrCap は ffmpeg の stderr から保持する末尾バイト数。encode.go の
// strings.Builder と違い、ライブの ffmpeg はセッションの生存中（数時間〜）ずっと
// 動くため、無制限バッファはエラーが出続けるとメモリを消費し続ける
// （レビューで指摘）。診断に十分な量だけ末尾を保持する。
const stderrCap = 8 * 1024

// cappedWriter は末尾 max バイトだけを保持する io.Writer（スレッドセーフ）。
type cappedWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newCappedWriter(max int) *cappedWriter {
	return &cappedWriter{max: max}
}

// Write は io.Writer を満たす。常に (len(p), nil) を返す（バッファへの追記は
// 失敗しない）。
func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// String は現在保持している内容を返す。
func (w *cappedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

type liveStreamPrefixPump struct {
	mu          sync.Mutex
	limit       int
	prefix      []byte
	probing     bool
	prefixReady chan struct{}
	readErr     error
}

func (p *liveStreamPrefixPump) finishProbeLocked(err error) {
	if !p.probing {
		return
	}
	p.probing = false
	p.readErr = err
	close(p.prefixReady)
}

func (p *liveStreamPrefixPump) snapshotAndStop() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishProbeLocked(nil)
	return append([]byte(nil), p.prefix...), p.readErr
}

func (p *liveStreamPrefixPump) copy(body io.Reader, writer *io.PipeWriter) {
	buf := make([]byte, 64*1024)
	for {
		n, readErr := body.Read(buf)
		remainder := buf[:n]
		p.mu.Lock()
		if p.probing && n > 0 {
			take := min(n, p.limit-len(p.prefix))
			p.prefix = append(p.prefix, buf[:take]...)
			remainder = buf[take:n]
			if len(p.prefix) == p.limit {
				p.finishProbeLocked(nil)
			}
		}
		if readErr != nil {
			p.finishProbeLocked(readErr)
		}
		p.mu.Unlock()

		if len(remainder) > 0 {
			if _, err := writer.Write(remainder); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				_ = writer.Close()
			} else {
				_ = writer.CloseWithError(readErr)
			}
			return
		}
	}
}

type liveStreamPrefixReplay struct {
	reader io.Reader
	pipe   *io.PipeReader
}

func (r *liveStreamPrefixReplay) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *liveStreamPrefixReplay) Close() error {
	return r.pipe.Close()
}

// readLiveStreamPrefix asynchronously buffers up to limit bytes for ffprobe, then
// returns a reader that replays that prefix before the rest of body. It stops
// waiting after wait even when an upstream Read is blocked, so a chase playlist
// can start before a growing recording produces more data. The pump owns body
// reads until it reaches EOF or body is closed.
func readLiveStreamPrefix(
	ctx context.Context,
	body io.ReadCloser,
	limit int,
	wait time.Duration,
) (input io.ReadCloser, prefix []byte, err error) {
	pipeReader, pipeWriter := io.Pipe()
	pump := &liveStreamPrefixPump{
		limit:       limit,
		prefix:      make([]byte, 0, limit),
		probing:     true,
		prefixReady: make(chan struct{}),
	}
	go pump.copy(body, pipeWriter)

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-pump.prefixReady:
	case <-timer.C:
	case <-ctx.Done():
	}
	prefix, err = pump.snapshotAndStop()
	input = &liveStreamPrefixReplay{
		reader: io.MultiReader(bytes.NewReader(prefix), pipeReader),
		pipe:   pipeReader,
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return input, prefix, err
}

type liveStreamInfo struct {
	audioStreams int
	hasSubtitles bool
}

// probeLiveStreamInfo は ffprobe に MPEG-TS の有限な先頭部分だけを渡し、音声 ES 数と
// 字幕の有無を調べる。アプリケーション自身は TS/PES や放送記述子を解釈しない。
func probeLiveStreamInfo(ctx context.Context, ffprobe string, prefix []byte) (liveStreamInfo, error) {
	ffprobe = ffargs.FFprobePath(ffprobe)
	probeCtx, cancel := context.WithTimeout(ctx, liveStreamProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffprobe,
		"-v", "error", "-probesize", "5M", "-analyzeduration", "3M",
		"-show_entries", "stream=codec_type", "-of", "json", "-i", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(prefix)
	out, err := cmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return liveStreamInfo{}, probeCtx.Err()
		}
		return liveStreamInfo{}, fmt.Errorf("running ffprobe: %w", err)
	}
	var result struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return liveStreamInfo{}, fmt.Errorf("decoding ffprobe stream list: %w", err)
	}
	info := liveStreamInfo{}
	for _, stream := range result.Streams {
		switch stream.CodecType {
		case "audio":
			info.audioStreams++
		case "subtitle":
			info.hasSubtitles = true
		}
	}
	if info.audioStreams == 0 {
		return liveStreamInfo{}, errors.New("ffprobe found no audio stream")
	}
	return info, nil
}
