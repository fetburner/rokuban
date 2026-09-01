/**
 * ライブ視聴（M4-4）の純関数群。
 *
 * URL 組み立て・エラー分類・初期チャンネル選択は DOM にも hls.js にも依存しないので
 * ここに集約してテストする。実再生（`<video>` / hls.js の初期化）は jsdom で測れない
 * ため `components/live-player.tsx` 側にとどめ、状態遷移の判定だけをここに置く
 * （CLAUDE.md「jsdom が測れないものは実装より先に判定手段を作る」の対称形 ---
 * ここでは判定できる部分とできない部分を先に切り分けている）。
 */

import type { Service } from '@/api/generated'

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
 * `requestedId` に一致するサービスがあればそれを使う。一致しない（未指定・
 * 無効な id・一覧に無い id）ときは番組を持つ先頭のサービスへフォールバックする
 * --- マルチ編成のないサブサービス（`hasPrograms: false`）を既定にしても、今放送中の
 * 番組を出せず「いま放送中」欄が常に空になる。番組を持つサービスが 1 つも無ければ
 * 先頭のサービスを使う。サービス自体が 1 件も無ければ undefined（まだ取得できて
 * いない、または EPG プロジェクションが空）。
 */
export function pickInitialService(
  services: readonly Service[],
  requestedId: number | undefined,
): Service | undefined {
  if (requestedId !== undefined) {
    const exact = services.find((s) => s.id === requestedId)
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
 * `node_modules/hls.js` 1.6.17 で確認済み）ため呼び出し側が `0` を欠損として
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

/** LivePlaylistProbeResult は probeLivePlaylist の結果。 */
export type LivePlaylistProbeResult = { ok: true } | { ok: false; error: LiveLoadError }

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
  if (response.ok) return { ok: true }
  const body = await response.text().catch(() => '')
  return {
    ok: false,
    error: classifyLiveLoadError({ kind: 'http', status: response.status, body }),
  }
}
