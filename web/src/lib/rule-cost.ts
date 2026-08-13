/**
 * ルールを保存する前の値札（件数・時間の見込み）を検索結果から導出する純関数。
 *
 * **値札は警告ではない。** しきい値で色を変えたり保存を止めたりしない --- 多いか
 * 少ないかの判断はユーザーのもの（issue #237）。GB 換算もやらない --- ビットレートの
 * 実測の出所が未決で、この関数のスコープはそれに依存しない件数と時間だけに切る。
 *
 * React に依存しないのは `lib/program-search.ts` と同じ理由。7 日換算の係数や
 * サンプルからの外挿は、UI 越しに検証すると桁や係数の取り違えが見えにくい。
 */

/**
 * epgWindowDays は EPG のローリングウィンドウが「今日から何日先まで」保持される
 * 想定日数。番組表は「共通の『EPG のローリングウィンドウの終端』（8 日先の 0 時）」
 * （docs/frontend/programs.md）を上限に使っており、EPG プロジェクション自体も
 * 「8 日 + 猶予」（docs/data/projections.md）で刈り取られる。検索結果はこの窓の
 * 範囲内でしか出ないため、検索結果の実測はおおむね「今日から 8 日分」の観測である。
 *
 * ルールの時間帯条件は曜日単位（1 週間周期）で書く（`RuleTimeWindow.weekdays`）ため、
 * 8 日という半端な単位のまま件数・時間を見せると「多い/少ない」の直感的な判断が
 * しにくい。7 日（1 週間）あたりに正規化することで、曜日条件と同じ周期の単位に揃える。
 *
 * 「今」から窓の終端までの実際の残り日数は時刻に応じて 7〜8 日の間で揺れるが、
 * ここでは実際の残り日数を測らず、EPG が保証する上限（8 日）を固定の分母として使う
 * --- 実際の残り日数を測るには「今」と窓の終端（サービスごとに EPG 受信状況で
 * 微妙に前後する）を別途問い合わせる必要があり、値札 1 つのために追加の API 呼び出しを
 * 増やすコストに見合わない。揺れの分だけ 7 日換算が実際の平均より少なめに出ることは
 * あっても、多めに出ることはない（保証された上限を分母に使っているため）。
 *
 * 検索条件に `periodStartAt` / `periodEndAt` で明示的な期間を指定した場合も、この
 * 8 日基準をそのまま使う。期間を絞った検索は既に「期間を指定したまま保存すると
 * 恒久的な期間制限になる」という別の注意書き（`pages/search.tsx` の `hasPeriod`）を
 * 持っているので、値札側でさらに期間の実際の幅を分母にする特別扱いを足すと
 * 「なぜこの数字だけ基準が違うのか」を説明する負担が増える。期間指定時の値札の精度は
 * 元々「見込み」の範囲内として扱う。
 */
export const epgWindowDays = 8

/** ruleCostWeekDays は正規化先の単位（1 週間 = 7 日）。 */
export const ruleCostWeekDays = 7

/**
 * RuleCostSample は値札の入力。
 *
 * `totalCount` は検索結果（`programId` の配列）の全件数 --- 検索 API
 * （`POST /api/sites/{site}/programs/search`、`internal/api/search.go`）は
 * `rulequery.MatchProgramIDs` の結果を `LIMIT` なしでそのまま返すため、ページングも
 * 上位 N 件打ち切りも無く、返る配列の長さがそのまま母数になる（実際にコードを
 * 確認済み。`internal/rulequery/query.go` の SQL に LIMIT は無い）。
 *
 * `loadedDurationsMs` は番組ごとの `durationMs`。検索 API 自体は `programId` しか
 * 返さないため、`GET /api/programs/{id}` で個別に取得できた分だけがここに入る
 * （`pages/search.tsx` が結果一覧の表示のために取得している分をそのまま再利用する
 * ので、値札のために追加のリクエストは発生しない）。全件に届いていないとき
 * （`loadedDurationsMs.length < totalCount`）は平均から外挿する --- 黙って
 * 読み込み済みの合計だけを見せると実際より小さく見えるため。
 */
export type RuleCostSample = {
  totalCount: number
  loadedDurationsMs: number[]
}

/** RuleCostEstimate は `estimateRuleCost` の出力。 */
export type RuleCostEstimate = {
  /** 検索結果の総件数（母数。ページングされていない全件） */
  totalCount: number
  /** 7 日あたりの見込み件数 */
  countPerWeek: number
  /**
   * 7 日あたりの見込み時間（ms）。
   *
   * `totalCount` が 0 のときだけ確定した `0`。`totalCount > 0` で
   * `loadedDurationsMs` がまだ 1 件も無いとき（読み込み中）は `undefined` ---
   * 「まだ算出できていない」と「算出した結果が 0」を同じ値で表さない
   * （`/search` の「未検索と 0 件を混同しない」規律と同じ精神）。
   */
  durationMsPerWeek: number | undefined
  /** 時間の見積もりに使ったサンプル件数（`loadedDurationsMs.length`） */
  sampleSize: number
  /** サンプルが全件に届いていない（＝時間は外挿である）かどうか */
  isSampled: boolean
}

/**
 * estimateRuleCost は検索結果から「7 日あたり約 N 番組 / 合計約 H 時間」を導出する。
 *
 * 件数は `totalCount` から厳密に計算できる（母数が全件であることは
 * `RuleCostSample` のコメントの通り確認済み）。時間は `loadedDurationsMs`
 * （読み込み済みの一部でありうる）の平均を `totalCount` に外挿する --- 全件の
 * 詳細を取得し直すと数百件規模のルールでリクエストが数百本に膨らむため
 * （`pages/search.tsx` の `pageSize` のコメントと同じ理由）、既に画面が持っている
 * 分だけを使う。
 */
export function estimateRuleCost(
  sample: RuleCostSample,
  windowDays: number = epgWindowDays,
): RuleCostEstimate {
  const { totalCount, loadedDurationsMs } = sample
  const factor = ruleCostWeekDays / windowDays
  const sampleSize = loadedDurationsMs.length
  const isSampled = sampleSize < totalCount

  const countPerWeek = totalCount * factor

  const durationMsPerWeek =
    totalCount === 0
      ? 0
      : sampleSize === 0
        ? undefined
        : (loadedDurationsMs.reduce((sum, ms) => sum + ms, 0) / sampleSize) * totalCount * factor

  return { totalCount, countPerWeek, durationMsPerWeek, sampleSize, isSampled }
}
