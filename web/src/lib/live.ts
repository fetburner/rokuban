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
 * mirakcServiceIdMagic は Mirakurun / mirakc が service id を合成する基数。
 * `internal/mirakc/ids.go` の idMagicNumber（100_000）と同じ。
 */
const mirakcServiceIdMagic = 100_000

/**
 * mirakcServiceId は SI の networkId / serviceId から Mirakurun 合成 service id を作る。
 *
 * mirakc の `GET /api/services/{id}/stream` が要求するのは SI の serviceId ではなく
 * この合成値（issue #208）。streamer は URL の数字をそのまま mirakc に渡す契約
 * （docs/api.md §ライブ視聴の HLS）なので、合成は URL を組み立てる側の責務。
 */
export function mirakcServiceId(networkId: number, serviceId: number): number {
  return networkId * mirakcServiceIdMagic + serviceId
}

/**
 * livePlaylistURL はストリーマーが配るプレイリスト URL を組み立てる（OpenAPI 外。
 * [docs/api.md](../../../docs/api.md) §ライブ視聴の HLS）。
 *
 * パスの serviceId は EPG の SI serviceId ではなく、mirakc が受け付ける合成 id
 * （`mirakcServiceId`）。渡す前にここで合成する（issue #208）。
 */
export function livePlaylistURL(
  site: string,
  networkId: number,
  serviceId: number,
  profile?: string,
): string {
  const id = mirakcServiceId(networkId, serviceId)
  const base = `/api/sites/${encodeURIComponent(site)}/services/${id}/live/playlist.m3u8`
  return profile ? `${base}?profile=${encodeURIComponent(profile)}` : base
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
 * pickInitialServiceId は `serviceId` 検索パラメータから初期選択チャンネルを決める。
 *
 * 指定があり、かつ現在のサービス一覧に存在するならそれを使う。無効な指定
 * （存在しない id・未指定）のときは番組を持つ先頭のサービスへフォールバックする ---
 * マルチ編成のないサブサービス（`hasPrograms: false`）を既定にしても、今放送中の
 * 番組を出せず「いま放送中」欄が常に空になる。番組を持つサービスが 1 つも無ければ
 * 先頭のサービスを使う。サービス自体が 1 件も無ければ undefined（まだ取得できて
 * いない、または EPG プロジェクションが空）。
 */
export function pickInitialServiceId(
  services: readonly Service[],
  requestedServiceId: number | undefined,
): number | undefined {
  if (
    requestedServiceId !== undefined &&
    services.some((s) => s.serviceId === requestedServiceId)
  ) {
    return requestedServiceId
  }
  return (services.find((s) => s.hasPrograms) ?? services[0])?.serviceId
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
