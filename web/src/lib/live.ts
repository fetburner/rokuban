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
 */
export function livePlaylistURL(site: string, serviceId: number, profile?: string): string {
  const base = `/api/sites/${encodeURIComponent(site)}/services/${serviceId}/live/playlist.m3u8`
  return profile ? `${base}?profile=${encodeURIComponent(profile)}` : base
}

/**
 * supportsNativeHls は `<video>` がネイティブに HLS を再生できるかを判定する（Safari）。
 *
 * `canPlayType` を注入で受け取るのは、実際の `HTMLVideoElement.canPlayType` は
 * jsdom で常に `''` を返す（未実装）ため、テストから振る舞いを差し替えられるように
 * するため。
 */
export function supportsNativeHls(canPlayType: (type: string) => string): boolean {
  const result = canPlayType('application/vnd.apple.mpegurl')
  return result === 'probably' || result === 'maybe'
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
export async function probeLivePlaylist(url: string): Promise<LivePlaylistProbeResult> {
  let response: Response
  try {
    response = await fetch(url)
  } catch {
    return { ok: false, error: classifyLiveLoadError({ kind: 'network' }) }
  }
  if (response.ok) return { ok: true }
  const body = await response.text().catch(() => '')
  return {
    ok: false,
    error: classifyLiveLoadError({ kind: 'http', status: response.status, body }),
  }
}
