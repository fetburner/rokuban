import {
  deleteRecordingPlaybackPosition,
  putRecordingPlaybackPosition,
  putRecordingWatched,
  type KeepRange,
} from '@/api/generated'
import { putRecordingPlaybackPositionBodyPositionMsMin } from '@/api/zod'

/**
 * 再生速度は端末ごとの好みとして localStorage に残す。
 * 再開位置と視聴済みは世帯共通の事実なので API に置く。
 */

export type PlaybackPositionWrite =
  | { kind: 'delete' }
  | { kind: 'put'; positionMs: number }
  | { kind: 'watched' }

/** サーバーが受け付ける再開位置の最小値（openapi.yaml の positionMs minimum）。 */
export const PLAYBACK_POSITION_MINIMUM_MS = putRecordingPlaybackPositionBodyPositionMsMin

/** 原本の ms をカット後の ms へ写す。keep 外の位置は次の区間の先頭へ寄せる。 */
export function originalMsToCutMs(originalMs: number, keepRanges: readonly KeepRange[]): number {
  let cutOffset = 0
  for (const range of keepRanges) {
    if (originalMs < range.startMs) return cutOffset
    if (originalMs >= range.endMs) {
      cutOffset += range.endMs - range.startMs
      continue
    }
    return cutOffset + originalMs - range.startMs
  }
  return cutOffset
}

/** カット後の ms を原本の ms へ戻す。内部境界は次の keep 区間の先頭へ寄せる。 */
export function cutMsToOriginalMs(cutMs: number, keepRanges: readonly KeepRange[]): number {
  if (keepRanges.length === 0) return 0
  let cutOffset = 0
  for (let index = 0; index < keepRanges.length; index += 1) {
    const range = keepRanges[index]!
    const length = range.endMs - range.startMs
    const cutEnd = cutOffset + length
    if (cutMs < cutEnd || index === keepRanges.length - 1) {
      return range.startMs + Math.min(Math.max(cutMs - cutOffset, 0), length)
    }
    cutOffset = cutEnd
  }
  return keepRanges.at(-1)!.endMs
}

/** API の原本秒を、いま再生する video の currentTime 秒へ戻す。 */
export function playbackResumeSeconds(positionMs: number | undefined, keepRanges?: readonly KeepRange[]): number | null {
  if (positionMs === undefined || !Number.isFinite(positionMs) || positionMs <= 0) return null
  return (keepRanges === undefined ? positionMs : originalMsToCutMs(positionMs, keepRanges)) / 1000
}

/** 各動画経路の currentTime から、位置 DELETE / PUT / 視聴済みを決める。 */
export function playbackPositionWrite(
  currentTimeSeconds: number,
  durationSeconds: number,
  durationFinal: boolean,
  keepRanges?: readonly KeepRange[],
): PlaybackPositionWrite {
  if (!Number.isFinite(currentTimeSeconds)) return { kind: 'delete' }
  if (currentTimeSeconds * 1000 < PLAYBACK_POSITION_MINIMUM_MS) return { kind: 'delete' }
  if (
    durationFinal &&
    Number.isFinite(durationSeconds) &&
    durationSeconds > 0 &&
    currentTimeSeconds >= durationSeconds * 0.9
  ) {
    return { kind: 'watched' }
  }
  const currentMs = Math.floor(currentTimeSeconds * 1000)
  const positionMs = keepRanges === undefined ? currentMs : cutMsToOriginalMs(currentMs, keepRanges)
  return positionMs < PLAYBACK_POSITION_MINIMUM_MS ? { kind: 'delete' } : { kind: 'put', positionMs }
}

/** 旧 localStorage の再開位置を削除する。速度キーは別名なので残る。 */
export function clearLegacyPlaybackPositions(): void {
  const prefix = 'rokuban:playback:'
  try {
    for (let index = localStorage.length - 1; index >= 0; index -= 1) {
      const key = localStorage.key(index)
      if (key?.startsWith(prefix)) localStorage.removeItem(key)
    }
  } catch {
    // private mode 等で localStorage が使えない場合は無視
  }
}

async function sendPlaybackWrite(recordingId: number, write: PlaybackPositionWrite, keepalive: boolean): Promise<boolean> {
  try {
    if (write.kind === 'delete') await deleteRecordingPlaybackPosition(recordingId, { keepalive })
    else if (write.kind === 'watched') await putRecordingWatched(recordingId, { keepalive })
    else await putRecordingPlaybackPosition(recordingId, { positionMs: write.positionMs }, { keepalive })
    return true
  } catch {
    return false
  }
}

// 先に出した位置 PUT が後から出した watched を追い越すと、watched が消した位置行を
// PUT が作り直す。書き込みは前のものが settle するまで次を送らない。
// ponytail: 全録画で 1 本のチェーン。録画をまたいでも順序が厳しくなるだけで結果は変わらない。
// 待ちが問題になったら録画ごとのチェーンにする。
let writeTail: Promise<unknown> = Promise.resolve()

/**
 * position writes are best effort; pause/pagehide supply later retry points.
 * keepalive（pagehide）は待てないので直列化せず即送る。残差: pagehide 時に未完了の書き込みが
 * あれば keepalive が追い越しうる。窓は直前の 1 往復だけで、ページを閉じる経路では待てないので受け入れる。
 */
export function persistPlaybackPosition(
  recordingId: number,
  write: PlaybackPositionWrite,
  keepalive = false,
): Promise<boolean> {
  if (keepalive) return sendPlaybackWrite(recordingId, write, true)
  const run = writeTail.then(() => sendPlaybackWrite(recordingId, write, false))
  writeTail = run
  return run
}

const RATE_KEY = 'rokuban:playback-rate'
const CHAPTER_EDIT_RATE_KEY = 'rokuban:chapter-edit-playback-rate'

function loadPlaybackRateFrom(key: string): number {
  try {
    const raw = localStorage.getItem(key)
    if (raw === null) return 1
    const n = Number(raw)
    return Number.isFinite(n) && n > 0 ? n : 1
  } catch {
    // private mode 等で localStorage が使えない場合は既定
    return 1
  }
}

function savePlaybackRateTo(key: string, rate: number): void {
  try {
    if (!Number.isFinite(rate) || rate <= 0) return
    if (rate === 1) localStorage.removeItem(key)
    else localStorage.setItem(key, String(rate))
  } catch {
    // ignore
  }
}

/**
 * loadPlaybackRate は保存済みの再生速度を返す。無い・壊れているなら 1。
 *
 * **録画ごとではなく端末ごとに 1 つ**（キーに録画 ID を含めない）。速度は
 * 「この録画をどう見るか」ではなく「自分がどう見るか」の好みなので、録画を
 * 変えるたびに 1 倍へ戻ると毎回選び直しになる（docs/frontend/design.md §個人化）。
 * 値はブラウザ標準 controls が提供するので固定の選択肢一覧ではなく、正の有限値を
 * 有効とする。
 */
export function loadPlaybackRate(): number {
  return loadPlaybackRateFrom(RATE_KEY)
}

/** savePlaybackRate は正の有限な再生速度を保存する。既定（1 倍）はキーごと消す。 */
export function savePlaybackRate(rate: number): void {
  savePlaybackRateTo(RATE_KEY, rate)
}

/** チャプター編集の速度は通常再生と分けて端末ごとに保存する。 */
export function loadChapterEditPlaybackRate(): number {
  return loadPlaybackRateFrom(CHAPTER_EDIT_RATE_KEY)
}

/** 既定（1 倍）はキーごと消し、通常再生の速度には触れない。 */
export function saveChapterEditPlaybackRate(rate: number): void {
  savePlaybackRateTo(CHAPTER_EDIT_RATE_KEY, rate)
}

/**
 * applyPlaybackRate は速度を video に設定し、ブラウザが拒否した場合は 1 倍へ戻して保存先へ反映する。
 * 対応する速度の範囲はブラウザごとに異なるため、保存時の数値検証だけでは
 * playbackRate の代入で NotSupportedError が起きる場合がある。保存先は呼び出し側が選ぶ。
 */
export function applyPlaybackRate(
  video: HTMLVideoElement,
  rate: number,
  saveFallback: (rate: number) => void,
): number {
  try {
    video.defaultPlaybackRate = rate
    video.playbackRate = rate
    return rate
  } catch {
    // 保存済みの速度をブラウザが受け付けない場合は、標準の 1 倍へ復旧する。
    try {
      video.defaultPlaybackRate = 1
      video.playbackRate = 1
    } catch {
      // 1 倍も設定できない環境でも React effect から例外を漏らさない。
    }
    saveFallback(1)
    return 1
  }
}

/**
 * ネイティブ HLS は ENDLIST 前の playlist を live として再生する。
 * 録画再生では WebKit がこの状態で 1 倍以外だと停止するため、有限尺になるまで 1 倍にする。
 */
export function effectivePlaybackRate(
  requestedRate: number,
  isRecordingPlayback: boolean,
  isNativeHls: boolean,
  durationSeconds: number,
): number {
  if (!isRecordingPlayback) return 1
  if (isNativeHls && durationSeconds === Infinity) return 1
  return requestedRate
}

/** recordingFileURL は streamer のバイナリ配信 URL を組み立てる（OpenAPI 外）。 */
export function recordingFileURL(recordingId: number, profile?: string): string {
  const base = `/api/media/recordings/${recordingId}/file`
  if (!profile) return base
  return `${base}?profile=${encodeURIComponent(profile)}`
}

/** recordingSubtitleURL は encoded アセット隣の WebVTT サイドカー URL を組み立てる。 */
export function recordingSubtitleURL(recordingId: number, profile: string): string {
  return `${recordingFileURL(recordingId, profile)}&track=subtitles`
}
