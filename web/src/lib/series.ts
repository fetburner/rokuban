import { LabelRuleInputKey, type LabelRule, type ProgramSearchMatch, type Recording } from '@/api/generated'

/**
 * isPlayableRecording は「再生できる行」か（`GET /api/recordings` の要素の形から
 * 判定する）。原本の `sizeBytes` があるか、active な encoded 派生物があるか。
 *
 * **ごみ箱の録画では `encodedAssets` が省略される**（プレイヤーを出さないので
 * 射影を落とす。`internal/api/recordings.go`）ので、この判定はごみ箱の行を
 * 「再生できない」と読む。ハブも次のエピソードも生きている録画しか扱わない
 * （`trash` を付けない）ので、実際にはその枝には入らない。
 */
export function isPlayableRecording(recording: Recording): boolean {
  return recording.sizeBytes !== undefined || (recording.encodedAssets?.length ?? 0) > 0
}

/**
 * nextEpisode は「次のエピソード」を選ぶ。
 *
 * 規則（docs/frontend/recordings.md）:
 *
 *   1. 起点より後に放送された回のうち、`program_start_at` が最も早いもの
 *   2. **再生できる行に限る**（{@link isPlayableRecording}）
 *   3. 同じ `program_start_at` の候補が複数あれば、起点と同じ site を優先し、
 *      次に id の小さい方
 *
 * 2 が要るのは、開始時刻がずれて supersede されなかった failed 行が finished 行と
 * 並んで残るためである（仕様）。その行を「次」にすると、押した先の再生が 404 に
 * なる（メディア配信は原本が無い行を配れない）。
 *
 * **`recordings` は起点の時刻以降を昇順で引いたページ**を渡す
 * （`order=asc&from=<起点の startAt>`）。カーソルが起点の時刻から始まるので、
 * 「起点より後の最初の行」は通常その先頭のページに入る。再生できない行が起点の後に
 * ページサイズ以上続くとページからはみ出し、その場合は「次のエピソード」が出ない（既知の限界）。
 */
export function nextEpisode(
  recordings: readonly Recording[],
  origin: Recording,
): Recording | undefined {
  const originStartMs = Date.parse(origin.startAt)
  const later = recordings.filter(
    (recording) =>
      Date.parse(recording.startAt) > originStartMs && isPlayableRecording(recording),
  )
  if (later.length === 0) return undefined

  const earliestStartMs = Math.min(...later.map((recording) => Date.parse(recording.startAt)))
  const candidates = later.filter(
    (recording) => Date.parse(recording.startAt) === earliestStartMs,
  )
  // 起点と同じ site を優先し、次に id の小さい方。**入力の配列を並べ替えない**
  // （呼び出し側の一覧の並びを壊さない）。
  return [...candidates].sort((a, b) => {
    const aSameSite = a.site === origin.site ? 0 : 1
    const bSameSite = b.site === origin.site ? 0 : 1
    if (aSameSite !== bSameSite) return aSameSite - bSameSite
    return a.id - b.id
  })[0]
}

/**
 * UpcomingRow はハブの「次回」の 1 行。**同じ放送は同じ行にまとめる**
 * （EPG は site ごとの射影なので、N 拠点の同じ放送が N 行で返る）。
 */
export type UpcomingRow = {
  networkId: number
  serviceId: number
  startAt: string
  name: string
  durationMs: number
  isFree: boolean
  /** この放送を持つ site（チップで出す。表示だけに使う）。 */
  sites: string[]
  /** まとめる前の programId。同じ放送は全サイトで同じ値を持つ。 */
  programIds: number[]
}

/**
 * collapseUpcoming は「次回」を `(networkId, serviceId, startAt)` で 1 行にまとめる。
 *
 * **録画の一覧では同じ放送を畳まない**（そこでは 1 行 = 1 録画が契約で、ドロップ
 * 統計で選び分ける運用では 2 行並ぶのが正しい）。畳むのはここだけである。
 *
 * まとめた行の並びは放送開始の昇順（同じ時刻は networkId / serviceId の順）。
 * サーバーは programId の昇順で返すが、同じ放送の N 行は同じ時刻なので順序に
 * 意味が無い。
 */
export function collapseUpcoming(matches: readonly ProgramSearchMatch[]): UpcomingRow[] {
  const byKey = new Map<string, UpcomingRow>()
  for (const match of matches) {
    const key = `${match.networkId}:${match.serviceId}:${match.startAt}`
    const existing = byKey.get(key)
    if (existing === undefined) {
      byKey.set(key, {
        networkId: match.networkId,
        serviceId: match.serviceId,
        startAt: match.startAt,
        name: match.name,
        durationMs: match.durationMs,
        isFree: match.isFree,
        sites: [match.site],
        programIds: [match.programId],
      })
      continue
    }
    if (!existing.sites.includes(match.site)) existing.sites.push(match.site)
    if (!existing.programIds.includes(match.programId)) existing.programIds.push(match.programId)
  }
  return [...byKey.values()].sort((a, b) => {
    const diff = Date.parse(a.startAt) - Date.parse(b.startAt)
    if (diff !== 0) return diff
    if (a.networkId !== b.networkId) return a.networkId - b.networkId
    return a.serviceId - b.serviceId
  })
}

/**
 * seriesLabelRules はシリーズ軸の分類ルールを、サーバーの評価順
 * （`priority DESC, id ASC`。docs/data/series.md）に並べて返す。
 * `/series` の「手動」札とハブの実効シリーズ判定が同じ絞り込みを使う。
 */
export function seriesLabelRules(rules: readonly LabelRule[]): LabelRule[] {
  return rules
    .filter((rule) => (rule.key ?? LabelRuleInputKey.series) === LabelRuleInputKey.series)
    .sort((a, b) => (b.priority ?? 0) - (a.priority ?? 0) || a.id - b.id)
}

/**
 * findManualSeriesRule は value の棚を指定している分類ルールを返す。
 *
 * **同じ値を指すルールが複数あるとき「勝った」ルールはクライアントでは決められない**
 * （勝敗は録画ごとにキーワードが当たるかで決まり、当たり判定はサーバーだけが持つ）。
 * ここでは評価順の先頭を返す近似で、キーワードの初期値にだけ使う。
 */
export function findManualSeriesRule(
  rules: readonly LabelRule[],
  value: string | undefined,
): LabelRule | undefined {
  if (value === undefined) return undefined
  return seriesLabelRules(rules).find((rule) => rule.valueKey === value)
}
