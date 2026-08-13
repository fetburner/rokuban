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
 * interruptionQueryWindowGridMs は EPG 問い合わせ（`GET /api/sites/{site}/programs`）
 * の時間窓を丸めるグリッド幅（10 分）。
 *
 * `pages/live.tsx` は「いま」を `nowPlayingRefetchMs`（30 秒）ごとに更新する tick を
 * 持つ。窓の開始・終了を `nowMs` から素直に組むと、この 30 秒ごとの tick で
 * `useListPrograms` のクエリパラメータ（= クエリキー）が毎回変わり、react-query は
 * それを**新しいキャッシュエントリ**として扱う --- 直前のキャッシュ済み
 * `sameTypeProgramIds` は使えず、新しいキーの `data` は取得完了までの間 `undefined`
 * に戻る。この間 `sameTypeProgramIds` は空集合になり、`upcomingInterruptingReservation`
 * が該当を見つけられず、**表示中の警告が一時的に消える**（実測: jsdom で
 * 30038ms 後・実 Chromium で 28258ms 後に消失。`pages/live.test.tsx`「30 秒の
 * tick を跨いでも警告が消えない」参照。レビューでの指摘）。加えて、同種別全
 * サービス × 2 時間ぶんの EPG を 30 秒ごとに取り直すのは無駄が大きい。
 *
 * 窓の開始点を 10 分単位に切り捨てて丸めることで、この 10 分の間は
 * `interruptionQueryWindow` が返す `{ start, end }` が**値として不変**になり
 * （react-query のクエリキーはハッシュによる値比較なので、同じ値なら同じ
 * キャッシュエントリのまま）、tick のたびに `data` が失われることも、10 分の間に
 * 何度も再取得されることも無くなる。
 */
export const interruptionQueryWindowGridMs = 10 * 60 * 1000

/**
 * interruptionQueryWindow は `nowMs` から EPG 問い合わせの時間窓を組む。
 *
 * 開始点は `gridMs` 単位に切り捨てる（`interruptionQueryWindowGridMs` 参照）。
 * 終了点は「切り捨てた分のずれ（最大 `gridMs`）」を `lookaheadMs` に足すことで、
 * 常に実際の判定窓 `[nowMs, nowMs + lookaheadMs)` を包含する上位集合になる ---
 * 窓を広げる方向にしか丸めないので、`upcomingInterruptingReservation` が本来
 * 見るべき programId を見落とすことは無い（広い分は同関数側の `startMs` の
 * 範囲チェックで最終的に絞られる）。
 */
export function interruptionQueryWindow(
  nowMs: number,
  lookaheadMs: number = interruptionLookaheadMs,
  gridMs: number = interruptionQueryWindowGridMs,
): { start: string; end: string } {
  const base = Math.floor(nowMs / gridMs) * gridMs
  return {
    start: new Date(base).toISOString(),
    end: new Date(base + lookaheadMs + gridMs).toISOString(),
  }
}

/**
 * InterruptingReservationCandidate は判定に必要な予約の部分形。
 *
 * `Reservation` 全体ではなく必要なフィールドだけを要求することで、テストが
 * 生成型の全フィールドを埋めずに済む（構造的型付けにより `Reservation` を
 * そのまま渡すこともできる --- 下部の `AssertReservationIsInterruptingCandidate`
 * 参照）。
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
// 型レベルの assert（レビューでの指摘。実行されない関数を残さない。0 コスト）。
// `Reservation` が `InterruptingReservationCandidate` を満たさなくなったら
// ここが `never` になりコンパイルエラーになる。
type AssertReservationIsInterruptingCandidate =
  Reservation extends InterruptingReservationCandidate ? true : never
const _assertReservationIsInterruptingCandidate: AssertReservationIsInterruptingCandidate = true
void _assertReservationIsInterruptingCandidate
