/**
 * 録画予約による中断予測（M7-2, issue #235）。
 *
 * mirakc の優先度調停では録画が勝つため、視聴中に同じチャンネル種別の録画予約が
 * 始まるとチューナーを取られて視聴側が中断されうる。Rokuban は予約（desired state）
 * を持っているので、視聴開始前に「この後中断されうるか」を知らせられる ---
 * EPGStation・KonomiTV には構造的にできない表示（issue #235 の「解くべき問題」）。
 *
 * ## 下界主義（docs/data.md §6.5 と同じ規律）
 *
 * - **「中断されます」と断言しない。** チューナーに余裕があれば中断されないが、
 *   余裕があるとも言えない（見えない消費者 --- 並走 EPGStation・他のライブ視聴
 *   セッション・mirakc の `excluded_channels` --- がいる）。文言は「不足すると
 *   中断されます」という条件付きにする
 * - **「録画予約はありません = 安全に見られます」を出さない。** 肯定的な文言を
 *   一切持たない --- 沈黙は保証ではない（`lib/capacity.ts` の同じ規律を参照）
 *
 * ## skip の除外（サーバーの需要計算と同じ規則）
 *
 * `effective.skip` が true の予約は reconciler が mirakc に同期しないためチューナーを
 * 消費しない（`internal/capacity/load.go` の `demandFromRow` --- `eff.IsSkipped()` が
 * true の行は容量の需要から除外される）。API が返す `Reservation.skip` はまさに
 * この `effective.skip` なので、フロント側もこの値で同じ除外を行う。
 */

import type { Reservation } from '@/api/generated'
import { formatTime } from '@/lib/format'

/**
 * interruptionLookaheadMs は「近い将来」として警告する先読みの時間窓（2 時間）。
 *
 * 視聴を選ぶ／始める瞬間の判断材料として出す表示なので、窓は「これから見始める
 * 1 回の視聴」がカバーする範囲に合わせる。1 番組（30 分〜1 時間）を見ている間に
 * 次の番組の録画が競合し得ることまでは見せたいが、24 時間先の録画予約まで
 * 警告すると「今まさに見るかどうかの判断」には関係の薄い予約まで出てノイズになる。
 * 2 時間なら典型的な 1〜2 番組分の録画開始が視野に入り、かつ「近い将来」と
 * 呼べる範囲に収まる。
 */
export const interruptionLookaheadMs = 2 * 60 * 60 * 1000

/**
 * InterruptingReservationCandidate は判定に必要な予約の部分形。
 *
 * `Reservation` 全体ではなく必要なフィールドだけを要求することで、テストが
 * 生成型の全フィールドを埋めずに済む（構造的型付けにより `Reservation` を
 * そのまま渡すこともできる --- 下部の `_typeCheck` 参照）。
 */
export type InterruptingReservationCandidate = {
  site: string
  programId: number
  skip: boolean
  startAt: string
}

/**
 * upcomingInterruptingReservation は「近い将来に同じチャンネル種別で始まる録画予約」を
 * 1 件返す（無ければ null）。
 *
 * 一致条件（すべて満たす予約のうち、最も早く始まるものを返す）:
 *
 * 1. `!skip`（effective.skip が false --- 録画されない予約は需要でない）
 * 2. `site` が視聴対象の site と一致する（`programId` は site スコープの値であり、
 *    別サイトの同じ番号の番組と取り違えない。docs/schema.md §1 の設計原則）
 * 3. `programId` が `sameTypeProgramIds`（呼び出し側が視聴対象と同じチャンネル種別の
 *    サービスに絞って引いた EPG の programId 集合）に含まれる --- チャンネル種別
 *    そのものは予約が持たないため、EPG 側の join を経由する
 * 4. `startAt` が半開区間 `[nowMs, nowMs + lookaheadMs)` に入る --- すでに始まった
 *    予約（`startAt < nowMs`）は対象にしない。「この後中断されうるか」という
 *    視聴開始前の予告が目的で、すでに始まっている予約についてはその瞬間がもう
 *    過ぎている
 *
 * 複数の予約が一致するときは最も早く始まるものを 1 件返す --- 視聴者が次に
 * 気にすべき境目はそれだけなので、一覧化はしない（バッジ／文言 1 個に対応する）。
 */
export function upcomingInterruptingReservation<T extends InterruptingReservationCandidate>(
  reservations: readonly T[],
  site: string,
  sameTypeProgramIds: ReadonlySet<number>,
  nowMs: number,
  lookaheadMs: number = interruptionLookaheadMs,
): T | null {
  let earliest: T | null = null
  let earliestStartMs = Number.POSITIVE_INFINITY

  for (const reservation of reservations) {
    if (reservation.skip) continue
    if (reservation.site !== site) continue
    if (!sameTypeProgramIds.has(reservation.programId)) continue

    const startMs = new Date(reservation.startAt).getTime()
    if (startMs < nowMs || startMs >= nowMs + lookaheadMs) continue

    if (startMs < earliestStartMs) {
      earliestStartMs = startMs
      earliest = reservation
    }
  }

  return earliest
}

/**
 * interruptionWarningMessage は文言を組む（「19:00 から録画予約があります。
 * チューナーが不足すると視聴は中断されます」）。
 *
 * **「不足すると中断されます」という条件付きにする**（issue #235 の「罠」）。
 * 「中断されます」と断言しない --- チューナーに余裕があれば中断されないが、
 * 余裕があるとも言えない（下界主義。docs/data.md §6.5 / `lib/capacity.ts` と
 * 同じ規律）。
 */
export function interruptionWarningMessage(reservation: { startAt: string }): string {
  return `${formatTime(reservation.startAt)} から録画予約があります。チューナーが不足すると視聴は中断されます`
}

// 構造的型付けで `Reservation`（生成型）をそのまま渡せることを保証するための
// コンパイル時チェック。実行はされない（`pnpm build` の型検査でのみ効く）。
function _typeCheck(reservations: readonly Reservation[], site: string, ids: ReadonlySet<number>) {
  upcomingInterruptingReservation(reservations, site, ids, Date.now())
}
void _typeCheck
