/**
 * ライブ視聴（M4-4）の純関数群。
 *
 * URL 組み立て・エラー分類・初期チャンネル選択は DOM にも hls.js にも依存しないので
 * ここに集約してテストする。実再生（`<video>` / hls.js の初期化）は jsdom で測れない
 * ため `components/live-player.tsx` 側にとどめ、状態遷移の判定だけをここに置く
 * （CLAUDE.md「jsdom が測れないものは実装より先に判定手段を作る」の対称形 ---
 * ここでは判定できる部分とできない部分を先に切り分けている）。
 */

import type { LiveProfileSummary, Service } from '@/api/generated'

/**
 * livePlaylistURL はストリーマーが配るプレイリスト URL を組み立てる（OpenAPI 外。
 * [docs/api.md](../../../docs/api.md) §ライブ視聴の HLS）。
 *
 * パスの `networkId` / `serviceId` は **SI の値そのもの** ---
 * `GET /api/sites/{site}/services` が返すのと同じ id 空間である。mirakc が要求する
 * 合成 id（`networkId * 100000 + serviceId`）への変換は streamer 側で行う
 * （`internal/streamer/live.go` の `resolveRequest`）。合成をここで行っていた形
 * （issue #208）は、mirakc の id 規則を TypeScript にも複製したうえで、URL の
 * `services/{...}` が一覧 API と違う id を指す状態を作っていた（issue #217）。
 */
export function livePlaylistURL(
  site: string,
  networkId: number,
  serviceId: number,
  profile?: string,
): string {
  const base =
    `/api/sites/${encodeURIComponent(site)}` +
    `/networks/${networkId}/services/${serviceId}/live/playlist.m3u8`
  return profile ? `${base}?profile=${encodeURIComponent(profile)}` : base
}

/**
 * validLiveProfile は `?profile=` の要求値を一覧に照らして検証し、使える名前だけを返す。
 *
 * **未知の名前をそのまま流してはならない。** streamer は `?profile=` が空なら既定
 * （`live.profiles` の先頭）に落とすが、**未知の名前は 400**（`unknown live profile`。
 * `internal/streamer/live.go` の `Playlist`）を返す。綴り違いの共有リンク・古い
 * ブックマークをエラー画面にしないため、フロントが先に落として既定へ倒す。
 *
 * 一覧は実行時に来るデータなので、この検査は `validateSearch` では書けない
 * （あちらは同期・クエリ文字列だけを見る）。`?site=` と同じ分担である ---
 * 形は `validateSearch`、実在の判定は一覧を読める場所。
 */
export function validLiveProfile(
  profiles: readonly LiveProfileSummary[],
  requested: string | undefined,
): string | undefined {
  if (requested === undefined) return undefined
  return profiles.some((p) => p.name === requested) ? requested : undefined
}

/**
 * liveProfileLabel は画質セレクタに出す 1 件分の表示名。
 *
 * 名前は設定者が付けた文字列（`h264` 等）で、それだけでは画質として読めないことが
 * あるため、`height`（= 実際の出力高）を添える。**0 を「0p」と書かない** ---
 * 0 は「スケールしない」（元の解像度）の表現である。
 */
export function liveProfileLabel(profile: LiveProfileSummary): string {
  return profile.height !== undefined && profile.height > 0
    ? `${profile.name}（${profile.height}p）`
    : profile.name
}

/**
 * readSubtitleVisibility は字幕トラックの表示状態を読む（issue #869 の画質切替）。
 *
 * **トラックが 1 本も無いときは `null`（不明）を返す。** 「トラックはあるが全て
 * 切ってある」（`false`）と「まだトラックが無い」を潰すと、切替時に
 * 「字幕は切ってあった」と誤読して、既定（表示）を勝手に切ってしまう ---
 * 再生前の切替（probe 失敗からの再試行など）でこれが起きる。
 *
 * 引数を `HTMLMediaElement` ではなくトラックの配列にするのは、jsdom が
 * `TextTrackList` を実装しておらず（`video.textTracks` は常に空）、
 * 判定を純関数としてテストできるようにするため。
 */
export function readSubtitleVisibility(
  tracks: readonly { mode: string }[],
): boolean | null {
  if (tracks.length === 0) return null
  return tracks.some((t) => t.mode === 'showing')
}

/**
 * liveStallTimeoutMs は「映像が進んでいない」と見なすまでの猶予（ミリ秒。
 * 停滞時の自動降格。issue #871。テストから参照するので export する）。
 *
 * **ネイティブ HLS 経路の `stalled` / `waiting` の猶予と、hls.js 経路の
 * 「`currentTime` が進まない」の猶予で同じ値を使う。** どちらも同じ事象
 * （この回線でこのプロファイルのセグメントが間に合っていない）を別の信号で
 * 見ているだけなので、別々の値を置くと片方だけ調整されて食い違う。
 *
 * 12 秒にしたのは、WebKit が `stalled` を出すのがデータ途絶から 3 秒後
 * （HTML 仕様の「3 秒以上データが来ない」規定。実測でも 3.6 秒）で、
 * streamer 側のセグメント長が 2 秒（`internal/streamer/live.go` の
 * `-hls_time 2`）だから --- 正常なら 3 セグメント以上落ちないと到達しない。
 */
export const liveStallTimeoutMs = 12_000

/**
 * ProgressWatch は「映像が進んでいるか」の観測状態（issue #871）。
 *
 * `progressedAtMs` は最後に `currentTime` が動いたと判定した時刻で、
 * `currentTime` はそのときの値である。**「進んでいない時間」は持たない** ---
 * 毎回 `nowMs - progressedAtMs` で作り直せるので `stalledForMs` が導出する。
 */
export type ProgressWatch = {
  progressedAtMs: number
  currentTime: number
}

/**
 * nextProgressWatch は観測を 1 回反映した次の状態を返す。
 *
 * **`currentTime` が違えば進んだと見なす（`!==`。大小は見ない）。** ライブ同期点の
 * 補正で `currentTime` は後退しうるし、「進んだ」の判定に `>` を使うと後退の後に
 * 通常どおり再生が続いても無進捗の時計が止まらない（誤って停滞と読む）。
 *
 * `watch` が `null`（観測の基準が無い）なら、この観測を基準にして返す。
 */
export function nextProgressWatch(
  watch: ProgressWatch | null,
  nowMs: number,
  currentTime: number,
): ProgressWatch {
  if (watch === null || currentTime !== watch.currentTime) {
    return { progressedAtMs: nowMs, currentTime }
  }
  return watch
}

/**
 * stalledForMs は最後に映像が進んでからの経過（ミリ秒）。基準が無ければ 0。
 *
 * 判定する側が `liveStallTimeoutMs` と比べる（この関数は閾値を持たない）。
 */
export function stalledForMs(watch: ProgressWatch | null, nowMs: number): number {
  return watch === null ? 0 : nowMs - watch.progressedAtMs
}

/**
 * StallTracker は停滞の観測状態（issue #871）。
 *
 * `done` は「一度停滞と判定して、呼び出し側が下げられなかった」ことを表す ---
 * 現行の hls.js 経路に停滞の失敗経路は無いので、下げられないと分かった後に
 * 同じ判定を毎秒繰り返しても何も変わらない。
 */
export type StallTracker = { watch: ProgressWatch | null; done: boolean }

/**
 * StallHandling は停滞の後に呼び出し側が返す扱い。
 *
 * `true` は降格を引き取った、`false` はこの再生では下げない、`'wait'` は
 * 実行時のプロファイル一覧がまだ無く判断材料が足りない、を表す。
 */
export type StallHandling = boolean | 'wait'

/** createStallTracker は観測の初期状態を返す（`observeStall` の入力）。 */
export function createStallTracker(): StallTracker {
  return { watch: null, done: false }
}

/**
 * observeStall は観測を 1 回反映し、**停滞と見なすかどうか**を返す。
 * `true` を返したら呼び出し側が下げるかどうかを決める（この関数は下げない）。
 *
 * **DOM を読まない。** 判定に要るのは「一時停止しているか」「タブが見えているか」
 * 「いまの再生位置」「いまの時刻」だけなので、呼び出し側（`components/live-player.tsx`）
 * が読んで `sample` として渡す。こうすると閾値（`liveStallTimeoutMs`）を含む
 * 判定の本体がここに来て、実時間を使わずにテストできる
 * （jsdom のタイマーで 12 秒を作る必要が無い）。
 *
 * **`paused` の間と非表示タブでは数えず、基準を捨てる（`watch = null`）。**
 * `<video>` に `autoPlay` は無いので、再生を押す前は `currentTime` が進まないのが
 * 正常である。非表示タブでは `setInterval` が間引かれるので、捨てないと復帰した
 * 瞬間の巨大な差分で誤発火する。
 */
export function observeStall(
  tracker: StallTracker,
  sample: { paused: boolean; hidden: boolean; currentTime: number },
  nowMs: number,
): boolean {
  if (tracker.done) return false
  if (sample.paused || sample.hidden) {
    tracker.watch = null
    return false
  }
  tracker.watch = nextProgressWatch(tracker.watch, nowMs, sample.currentTime)
  if (stalledForMs(tracker.watch, nowMs) < liveStallTimeoutMs) return false
  tracker.done = true
  return true
}

/**
 * effectiveProfileHeight は帯域の重さを比べるための高さを返す。
 *
 * **`height` が 0（または省略）なら `Infinity`** --- 0 は「スケールしない」
 * = 元の解像度 = **最も重い**段である（`liveProfileLabel` が 0 に「0p」を
 * 書かないのと同じ意味の読み替え）。
 */
function effectiveProfileHeight(profile: LiveProfileSummary): number {
  return profile.height !== undefined && profile.height > 0
    ? profile.height
    : Number.POSITIVE_INFINITY
}

/**
 * nextLowerProfile は停滞時に自動で下げる次の 1 段を選ぶ（issue #871）。
 * 下げ先が無ければ `undefined`。
 *
 * **設定順（一覧 API の配列順）で前方へ走査し、実効高さが現在より小さい最初の段を
 * 返す。** 「配列は後ろほど軽い」という新しい契約は置かない --- 置くと
 * `[sd 480, hd 720]` の構成（先頭が既定で最も軽い）で停滞時に**重い方へ上げて**
 * しまう。`height` は「上がってしまう段」への拒否権としてだけ使う。
 *
 * 帰結（`config.example.yml` の形を含む）:
 *
 * | 一覧 | 選択 |
 * |---|---|
 * | `[hd 720, sd 480]`（`current` 未指定 = 先頭） | `sd` |
 * | `[sd 480, hd 720]` | 下げない（軽い段が後ろに無い） |
 * | `[h264 0, h264_720 720]` | `h264_720` |
 * | `[h264 720, h264_vaapi 720]` | 下げない（同高さを飛ばして後ろにも無い） |
 *
 * **`height` は帯域の完全な代理ではない**（同じ高さでも crf / codec で重さが違う）。
 * それでも拒否権としてなら誤りが「下げない」側に倒れるので、測っていない数値を
 * 装飾として出すより安全である。
 *
 * `current` が一覧に無いときは `undefined`（判断材料が無いので下げない）。
 * `current` を省略したときは先頭（= サーバー側の既定。`?profile=` を省略したときの
 * 意味。`pages/live.tsx` の既定（一覧の先頭）と同じ解決）。
 */
export function nextLowerProfile(
  profiles: readonly LiveProfileSummary[],
  current: string | undefined,
): string | undefined {
  const index = current === undefined ? 0 : profiles.findIndex((p) => p.name === current)
  if (index < 0 || profiles[index] === undefined) return undefined
  const currentHeight = effectiveProfileHeight(profiles[index])
  for (let i = index + 1; i < profiles.length; i++) {
    const candidate = profiles[i]
    if (candidate !== undefined && effectiveProfileHeight(candidate) < currentHeight) {
      return candidate.name
    }
  }
  return undefined
}

/**
 * isMasterPlaylist は probe が読んだプレイリスト本文が **master playlist か**を判定する
 * （issue #871）。
 *
 * **`live.captions: true` のデプロイでは自動降格を動かしてはならない。** そのとき
 * `Playlist` ハンドラは `?profile=` に関わらず master playlist（`playlist.m3u8`）を
 * 返すので（`internal/streamer/live.go`）、降格は**何も下げないのに「下げました」と
 * 表示する**ことになる。master の中では hls.js / ネイティブが自前で variant を選ぶ。
 *
 * 判定材料を **API ではなく本文**にするのは、本文が権威だからである ---
 * api ロールと streamer ロールに別の config を配る構成では、`GET /api/live-profiles`
 * の一覧（api の config の写し）が streamer の実際の出力と食い違いうる。
 *
 * 見るのは `#EXT-X-STREAM-INF` の有無だけである（master は variant ごとに 1 行持ち、
 * variant playlist 自身は持たない）。
 */
export function isMasterPlaylist(body: string): boolean {
  return body.includes('#EXT-X-STREAM-INF')
}

/**
 * liveLeaveURL は「このチャンネルを見るのをやめた」というヒントの宛先。
 *
 * プレイリスト / セグメントと同じ `(site, networkId, serviceId)` の固定深さ
 * （セッション ID は URL にもクッキーにも持たない）。id は `livePlaylistURL` と
 * 同じく **SI の値そのもの**を置く（合成は streamer 側。issue #217）。
 */
export function liveLeaveURL(site: string, networkId: number, serviceId: number): string {
  return (
    `/api/sites/${encodeURIComponent(site)}` +
    `/networks/${networkId}/services/${serviceId}/live/leave`
  )
}

/** chasePlaylistURL は録画中の追っかけ再生 EVENT playlist の URL を組み立てる。 */
export function chasePlaylistURL(
  site: string,
  recordingId: number,
  profile?: string,
  offsetSeconds?: number,
): string {
  const offset =
    Number.isSafeInteger(offsetSeconds) && offsetSeconds !== undefined && offsetSeconds > 0
      ? `/offset/${offsetSeconds}`
      : ''
  const base =
    `/api/sites/${encodeURIComponent(site)}/recordings/${recordingId}/chase` +
    `${offset}/playlist.m3u8`
  return profile ? `${base}?profile=${encodeURIComponent(profile)}` : base
}

/** chaseLeaveURL は追っかけ再生セッションへの離脱ヒントの宛先。 */
export function chaseLeaveURL(site: string, recordingId: number, offsetSeconds?: number): string {
  const offset =
    Number.isSafeInteger(offsetSeconds) && offsetSeconds !== undefined && offsetSeconds > 0
      ? `/offset/${offsetSeconds}`
      : ''
  return `/api/sites/${encodeURIComponent(site)}/recordings/${recordingId}/chase${offset}/leave`
}

/**
 * sendLiveLeaveHint は離脱のヒントを 1 回送る（失敗は無視する）。
 *
 * **これは停止命令ではない。** サーバー側はこれを受けてもセッションを止めず、
 * idle 期限を短い猶予まで詰めるだけ --- 同じチャンネルを見ている別の視聴者が
 * いれば、その人の次のセグメント要求が期限を元に戻す（`internal/streamer/live.go`
 * の `Leave`）。したがって「送れなかった」も「余計に送った」も壊れない：前者は
 * 従来どおり `live.idle_timeout` で回収され、後者は自分の次の要求が期限を戻す。
 *
 * **`navigator.sendBeacon` を優先する。** ページ離脱の瞬間（`pagehide` /
 * `visibilitychange`）に投げる必要があり、その時点の `fetch` はドキュメントの
 * 破棄で中断されうる。`sendBeacon` はブラウザが送信を引き取るのでこの窓を持たない
 * （POST しか出せないので、サーバー側もこの口を POST にしてある）。無い環境
 * （jsdom・古いブラウザ）では `keepalive: true` の `fetch` に落とす。
 */
export function sendLiveLeaveHint(site: string, networkId: number, serviceId: number): void {
  const url = liveLeaveURL(site, networkId, serviceId)
  if (typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
    // 本文は無い（宛先の URL が全ての情報を持つ）。戻り値の false（キュー拒否）は
    // 無視する --- 送れなくても idle GC が従来どおり回収する
    navigator.sendBeacon(url)
    return
  }
  void fetch(url, { method: 'POST', keepalive: true }).catch(() => {
    // 離脱時の失敗はユーザーに見せる意味がない（見せる画面がもう無い）
  })
}

/** sendChaseLeaveHint は sendLiveLeaveHint と同じ fire-and-forget 契約で追っかけを離れる。 */
export function sendChaseLeaveHint(site: string, recordingId: number, offsetSeconds?: number): void {
  const url = chaseLeaveURL(site, recordingId, offsetSeconds)
  if (typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
    navigator.sendBeacon(url)
    return
  }
  void fetch(url, { method: 'POST', keepalive: true }).catch(() => {
    // 離脱時の失敗は idle GC に任せる
  })
}

/**
 * livePlaylistMimeType は streamer がプレイリストに付ける Content-Type
 * （`internal/streamer/live.go`）。
 */
const livePlaylistMimeType = 'application/vnd.apple.mpegurl'

/**
 * liveSegmentMimeType は streamer がセグメントに付ける Content-Type
 * （`internal/streamer/live.go`。ffmpeg の HLS マルチプレクサが吐く MPEG-2 TS）。
 */
const liveSegmentMimeType = 'video/mp2t'

/**
 * supportsNativeHls は `<video>` が **streamer が実際に配るもの**をネイティブに
 * 再生できるかを判定する。プレイリストの MIME とセグメントの MIME の両方を問う。
 *
 * **プレイリストの MIME だけでは Safari と Chrome を区別できない。** Playwright の
 * 3 エンジンで実測した値（`web/e2e/live.mjs` の⑥が同じことを実ブラウザで固定する）:
 *
 * | canPlayType の引数 | WebKit 605.1.15 | Chromium 151 | Chrome 151 | Firefox 153 |
 * |---|---|---|---|---|
 * | `application/vnd.apple.mpegurl` | `maybe` | `maybe` | `maybe` | `''` |
 * | `application/x-mpegURL` | `maybe` | `maybe` | `maybe` | `''` |
 * | 上記 + `; codecs="avc1.42E01E,mp4a.40.2"` | `probably` | `probably` | `probably` | `''` |
 * | **`video/mp2t`** | **`maybe`** | **`''`** | **`''`** | **`''`** |
 *
 * つまり m3u8 の MIME に対する戻り値を決めているのは **codecs パラメータの有無で
 * あってエンジンの違いではない**（HTML 仕様は「codecs を許す type について、それが
 * 無いなら `probably` を返すべきでない」と定めており、3 エンジンともそれに従って
 * いるだけ）。`'probably'` のみを対応と見なす形（レビュー #190 の 1 回目の修正）は
 * **どの実ブラウザでも false を返し、ネイティブ分岐が一度も成立しない**。逆に
 * `'maybe'` も対応と見なす形（初版）は Chrome を誤ってネイティブ分岐へ送る。
 * m3u8 の MIME をどう読んでも、この 2 つのどちらかにしかならない。
 *
 * **見分けているのはセグメントの container である。** Chromium / Firefox の
 * `<video>` は MPEG-2 TS を demux できない（hls.js が TS を fMP4 へ remux してから
 * MSE に載せるのはこのため）が、WebKit はできる。streamer が配るセグメントは
 * `video/mp2t` そのものなので、この問いは「このブラウザは我々が配るものを
 * そのまま再生できるか」という能力そのものへの問いになっている --- エンジンの
 * 同定でも、m3u8 という拡張子への態度でもない。
 *
 * `canPlayType` を注入で受け取るのは、実際の `HTMLVideoElement.canPlayType` は
 * jsdom で常に `''` を返す（未実装）ため、テストから振る舞いを差し替えられるように
 * するため。
 */
export function supportsNativeHls(canPlayType: (type: string) => string): boolean {
  return canPlayType(livePlaylistMimeType) !== '' && canPlayType(liveSegmentMimeType) !== ''
}

/**
 * claimsHlsPlaylistSupport は `<video>` が HLS プレイリストの MIME に何らかの
 * 支持を表明するかだけを見る（`supportsNativeHls` より弱い問い）。
 *
 * **これ単独をネイティブ分岐の条件にしてはならない**（Chrome も真になる。上記の表）。
 * 使ってよいのは `Hls.isSupported()` が false と分かった後の**最後の砦**としてだけ
 * である --- MSE も ManagedMediaSource も無いブラウザは hls.js では絶対に再生
 * できないので、そこで `<video>` に直接渡して駄目でも失うものが無い。逆に
 * `supportsNativeHls` が（例えば iOS の `video/mp2t` に対する戻り値が macOS と
 * 違って）取りこぼした場合に、ネイティブで完璧に再生できる端末へ
 * 「このブラウザは HLS に対応していません」と表示してしまう事故を防ぐ。
 */
export function claimsHlsPlaylistSupport(canPlayType: (type: string) => string): boolean {
  return canPlayType(livePlaylistMimeType) !== ''
}

/**
 * pickInitialService は `?service=<Service.id>` から初期選択チャンネル（`Service`）を
 * 決める。
 *
 * `requestedId` と `requestedSite` に一致するサービスがあればそれを使う。
 * site が未指定なら従来どおり id の一致だけを見る。一致しない（未指定・無効な id・
 * 一覧に無い id/site）ときは番組を持つ先頭のサービスへフォールバックする
 * --- マルチ編成のないサブサービス（`hasPrograms: false`）を既定にしても、今放送中の
 * 番組を出せず「いま放送中」欄が常に空になる。番組を持つサービスが 1 つも無ければ
 * 先頭のサービスを使う。サービス自体が 1 件も無ければ undefined（まだ取得できて
 * いない、または EPG プロジェクションが空）。
 */
export function pickInitialService<S extends Service & { site?: string }>(
  services: readonly S[],
  requestedId: number | undefined,
  requestedSite?: string,
): S | undefined {
  if (requestedId !== undefined) {
    const exact = services.find(
      (s) => s.id === requestedId && (requestedSite === undefined || s.site === requestedSite),
    )
    if (exact !== undefined) return exact
  }
  return services.find((s) => s.hasPrograms) ?? services[0]
}

/**
 * currentProgramWindow は「いま放送中」を取得するための時間窓を返す。
 *
 * 零幅の窓（`start === end`）は EPG の重なり判定（`start_at < end AND end_at > start`）
 * に対する境界ケースを避けるため、`windowMs`（既定 60 秒）ぶんの幅を持たせる。幅を
 * 持たせても、いま放送中の番組（開始が `nowMs` 以前・終了が `nowMs` より後）は
 * `end_at > start` かつ `start_at < end` を常に満たすので取得結果は変わらない。
 */
export function currentProgramWindow(
  nowMs: number,
  windowMs = 60_000,
): { start: string; end: string } {
  return {
    start: new Date(nowMs).toISOString(),
    end: new Date(nowMs + windowMs).toISOString(),
  }
}

/** LiveLoadError はプレイリスト読み込み失敗の分類。 */
export type LiveLoadError =
  // streamer に到達できない（fetch 自体が reject）。ハイブリッド構成では
  // 自宅側が落ちているときに起きる正常状態（docs/overview.md §サーバーレスデプロイ）
  | { kind: 'unreachable' }
  // 503。同時セッション上限 / チューナー枯渇 / シャットダウン中のいずれか。
  // 本文（プレーンテキスト）はそのまま運ぶ
  | { kind: 'capacity'; message: string }
  // 想定外のステータス
  | { kind: 'other'; status: number; message: string }

/**
 * classifyLiveLoadError はプレイリスト取得の結果をエラー種別に分類する。
 *
 * 503 はすべて `capacity` に落とす --- 本文でセッション上限 / チューナー枯渇 /
 * シャットダウン中を区別できるが、いずれも「今は無理なので後で試す」という同じ
 * 対応を要求するので、UI 側の分岐は 1 つで足りる。本文は必ずそのまま運ぶ
 * （docs/frontend.md「エラーの本文も UI まで運ぶ」）。
 */
export function classifyLiveLoadError(
  result: { kind: 'network' } | { kind: 'http'; status: number; body: string },
): LiveLoadError {
  if (result.kind === 'network') return { kind: 'unreachable' }
  if (result.status === 503) return { kind: 'capacity', message: result.body.trim() }
  return { kind: 'other', status: result.status, message: result.body.trim() }
}

/**
 * LiveDiagnostics は再生経路から読み取った遅延・バッファの計器値（issue #476）。
 *
 * `latencySec` は hls.js 経由（`source: 'hls'`）でのみ埋まる。ネイティブ HLS
 * （Safari）はライブ同期点（`liveSyncPosition`）を持たないため、hls.js の
 * `latency` に相当する値を取得できない --- `source: 'native'` のときは
 * 呼び出し側（`components/live-player.tsx`）が常に `null` を渡す
 * （「測れないものを出さない」。`docs/frontend/live.md` §フロントエンド実装）。
 */
export type LiveDiagnostics = {
  source: 'hls' | 'native'
  latencySec: number | null
  bufferSec: number | null
}

/** liveDiagnosticsMissingLabel はまだ値が定まっていないときの表示。 */
const liveDiagnosticsMissingLabel = '—'

/**
 * missingOr は欠損値（`null` / `NaN`）なら `liveDiagnosticsMissingLabel` を、
 * そうでなければ `format` の結果を返す。
 *
 * 呼び出し側（`components/live-player.tsx` の `readHlsDiagnostics` /
 * `readNativeDiagnostics`）が既に欠損を `null` に正規化して渡す前提だが、
 * `NaN` もここで弾く --- `hls.latency` はライブ同期点が決まる前は `NaN` では
 * なく `0` を返す（`LatencyController.get latency()` が `this._latency || 0`。
 * `node_modules/hls.js` 1.7.1 で確認済み）ため呼び出し側が `0` を欠損として
 * 弾いているが、ここでの `NaN` チェックはそれをすり抜けた場合の保険。
 */
function missingOr(value: number | null, format: (n: number) => string): string {
  return value === null || !Number.isFinite(value) ? liveDiagnosticsMissingLabel : format(value)
}

/**
 * formatLiveDiagnostics は計器 1 行ぶんの表示文字列。
 *
 * ネイティブ経路（`source: 'native'`）は「先読み」だけを返す ---
 * 「放送から」を欠損表示（`—`）で出すことすらしない。measured でない値の
 * プレースホルダを置くこと自体が「そのうち測れる」という誤った期待を作るため
 * （issue #476 の含むもの 2「測れないものを出さない」）。
 */
export function formatLiveDiagnostics(diagnostics: LiveDiagnostics): string {
  const buffer = `先読み${missingOr(diagnostics.bufferSec, (n) => `${Math.round(n)}秒`)}`
  if (diagnostics.source === 'native') return buffer
  const latency = `放送から${missingOr(diagnostics.latencySec, (n) => `約${Math.round(n)}秒`)}`
  return `${latency} / ${buffer}`
}

/**
 * LivePlaylistProbeResult は probeLivePlaylist の結果。
 *
 * 成功時は `masterPlaylist` も返す（issue #871）。`live.captions: true` の
 * デプロイでは `?profile=` が何も選ばないので、自動降格を止める判断が要る
 * （`isMasterPlaylist`）。
 */
export type LivePlaylistProbeResult =
  | { ok: true; masterPlaylist: boolean }
  | { ok: false; error: LiveLoadError }

/**
 * probeLivePlaylist はプレイリスト URL への GET を 1 回行い、実際に再生を試す前に
 * 到達可能性とステータスを確認する。
 *
 * ネイティブ `<video>` も hls.js も、読み込み失敗時に HTTP ステータスや本文を取り出す
 * 手段を持たない（`<video>` の `error` イベントは status を運ばない。hls.js の
 * エラーイベントも本文までは持たない）。両方の再生経路で同じエラー表示を出すため、
 * 実際の再生前に `fetch` で 1 回取得し、成功したときだけ `<video>` / hls.js に URL を
 * 渡す。この GET 自体もセグメント要求と同じ経路（`internal/streamer` のアプリ配信）を
 * 通るので、idle GC の last-access 更新にも自然に乗る。
 *
 * **成功時は本文も読む**（issue #871）。読むのは master playlist かどうかの判定だけで
 * （`isMasterPlaylist`）、そのために要求を増やしはしない（同じ 1 回の GET の本文を
 * 読む）。プレイリストは数 KB なので、`response.ok` のときだけ読む費用は無視できる。
 *
 * **代償として、200 を返したまま本文が終わらない応答では probe が返らない**
 * （base はヘッダの時点で再生層へ進んでいた）。probe が返らないと `<video>` /
 * hls.js のセットアップに入らず、`stalled` の検出も再読み込み表示も働かないので、
 * 「読み込み中…」のまま止まる。**実配線では起きない** --- streamer は
 * `Content-Length` を付けて有限長を書き切る（`internal/streamer/live.go` の
 * `Playlist`）。ここを塞ぐなら本文の読み取りに上限（時間かバイト数）を設けることになる。
 */
export async function probeLivePlaylist(
  url: string,
  signal?: AbortSignal,
): Promise<LivePlaylistProbeResult> {
  let response: Response
  try {
    response = await fetch(url, { signal })
  } catch (err) {
    // 呼び出し側が明示的に中断した場合（チャンネル切り替え・破棄）はそのまま
    // 再 throw する --- 呼び出し側は `cancelled` フラグで結果を捨てるので、
    // ここで「到達できない」と誤分類してもいずれ捨てられるが、意図的な中断を
    // ネットワーク障害と同じ形で返すのは紛らわしい。中断は呼び出し側の
    // AbortController が起点なので、その意図をそのまま伝える
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    return { ok: false, error: classifyLiveLoadError({ kind: 'network' }) }
  }
  if (response.ok) {
    // 本文が読めなくても成功として扱う（既定 = master ではない）。読めない理由が
    // 転送中の切断なら、メディア層の失敗として `watchNativeMedia` /
    // hls.js の ERROR が拾う --- ここで probe を失敗にすると、両経路に共通の
    // エラー表示が「本文が読めなかった」という別の原因を語ることになる
    const body = await response.text().catch(() => '')
    return { ok: true, masterPlaylist: isMasterPlaylist(body) }
  }
  const body = await response.text().catch(() => '')
  return {
    ok: false,
    error: classifyLiveLoadError({ kind: 'http', status: response.status, body }),
  }
}
