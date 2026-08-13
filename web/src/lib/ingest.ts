/**
 * 原本の取り込み（ingest）状態の表示モデル（issue #212）。
 *
 * `recordings.status = finished` は **mirakc の録画完了**であって取り込み完了では
 * ない。取り込みが終わるまでブラウザ再生も事後エンコードもできないが、その時間帯
 * を表すものが `sizeBytes` の省略しか無かったため、遅い回線では止まっているのか
 * 進んでいるのか UI から判別できなかった。
 *
 * ここは `Recording.ingest`（サーバーが DB 行から毎回導出する。`openapi.yaml` の
 * `IngestProgress`）を画面に出す形へ落とすだけの純粋関数を置く --- jsdom で
 * 測れる部分（文言・数値）をコンポーネントから切り離して単体テストできるように
 * するため。
 */

import type { Recording } from '@/api/generated'

/**
 * ingestStaleAfterMs は「転送中だが停滞している」と見なすまでの無進捗時間。
 *
 * 既定のストール検知（`ingest.stall_timeout` = 30 秒。無進捗でいったん接続を
 * 切り、Range で繋ぎ直す）より長く取る --- 正常な再接続を毎回「停滞」と呼ぶと、
 * 本当に止まっているケースと区別が付かなくなる。進捗の書き込み間隔は 2 秒
 * （`internal/worker/ingest_progress.go` の `ingestProgressInterval`）なので、
 * 60 秒無進捗は再接続 1 往復では説明が付かない。
 */
export const ingestStaleAfterMs = 60_000

/**
 * ingestRefetchIntervalMs は取り込み中の録画が画面にある間の一覧・詳細の再取得
 * 間隔。
 *
 * SSE はヒントであって真実ではない（不変条件 5）ので、進捗は REST の再取得で
 * 収束させる。worker 側の書き込み間隔（2 秒）より長くする --- それより短くしても
 * 同じ値を読み直すだけになる。
 */
export const ingestRefetchIntervalMs = 5_000

/**
 * IngestDisplay は画面に出す取り込み状態。出すものが無ければ `undefined`
 * （`ingestDisplay` の戻り値）。
 *
 * - `pending`: 取り込み待ち / 再試行待ち
 * - `transferring`: 転送中。`stale` が真なら進捗が止まっている
 * - `originalDeleted`: 取り込みは完了したが原本が今は無い（削除された）
 */
export type IngestDisplay =
  | { kind: 'pending' }
  | {
      kind: 'transferring'
      writtenBytes: number
      expectedBytes?: number
      /** 0〜100 の整数。分母が無い / 0 のときは undefined。 */
      percent?: number
      stale: boolean
    }
  | { kind: 'originalDeleted' }

/**
 * ingestDisplay は録画 1 件の取り込み状態を表示モデルに落とす。出すものが無い
 * ときは `undefined` を返す。
 *
 * 出さないケース:
 *
 * - `ingest` が無い（API が古い）。**推測で埋めない**
 * - `status = 'recording'`: 取り込みがまだ始まっていないのは正常であって
 *   「待っている」ことを知らせる情報ではない。録画中の全行に「取り込み待ち」が
 *   並ぶのは、何も言っていないのと同じ
 * - `committed` かつ原本がある（`sizeBytes` あり）: 正常な完了形なので黙る
 * - `unknown`: mirakc record の観測すら無い。言えることが無い
 *
 * `originalDeleted`（`committed` かつ `sizeBytes` 無し）だけは `status` に
 * 関わらず返す --- これが **「まだ取り込めていない」と「取り込んだ後に消した」を
 * 区別する**唯一の材料で、区別しないと未 ingest の録画に「削除済み」と読める
 * 表示が出る（issue #211）。
 *
 * `nowMs` を引数で受け取るのは、停滞判定を時計に依存させずテストできるようにする
 * ため。
 */
export function ingestDisplay(recording: Recording, nowMs: number): IngestDisplay | undefined {
  const ingest = recording.ingest
  if (ingest === undefined) return undefined

  if (ingest.state === 'committed') {
    return recording.sizeBytes === undefined ? { kind: 'originalDeleted' } : undefined
  }
  if (recording.status === 'recording') return undefined

  if (ingest.state === 'transferring') {
    const writtenBytes = ingest.writtenBytes ?? 0
    const expectedBytes = ingest.expectedBytes
    // 分母が 0 / 未指定のときに Infinity や NaN を作らない。100 で頭打ちに
    // するのは、record_sync.content_length が転送開始時点の観測なので
    // written がそれを僅かに超えることがあるため（超過を 103% と出しても
    // 何も伝わらない）。
    const percent =
      expectedBytes !== undefined && expectedBytes > 0
        ? Math.min(100, Math.floor((writtenBytes / expectedBytes) * 100))
        : undefined

    const observedMs = ingest.observedAt === undefined ? NaN : Date.parse(ingest.observedAt)
    const stale = Number.isNaN(observedMs) ? false : nowMs - observedMs > ingestStaleAfterMs

    return { kind: 'transferring', writtenBytes, expectedBytes, percent, stale }
  }
  if (ingest.state === 'pending') return { kind: 'pending' }
  return undefined
}

/**
 * isIngestInFlight は「この録画の取り込みがまだ終わっていない」か。一覧・詳細を
 * 定期再取得するかどうかの判定に使う（終わっている画面をポーリングし続けない）。
 *
 * `status = 'recording'` の録画も真にする --- 録画が終われば取り込みが始まり、
 * そのとき画面が自動で追随してほしい（`ingestDisplay` が録画中に何も出さない
 * のは表示の判断であって、再取得を止める理由ではない）。
 */
export function isIngestInFlight(recording: Recording): boolean {
  if (recording.status === 'recording') return true
  const state = recording.ingest?.state
  return state === 'transferring' || state === 'pending'
}
