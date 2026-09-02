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
 * epgWindowDays は 7 日（1 週間）への正規化に使う固定の分母。**「観測スパンの
 * 正確な日数」ではなく、EPG プロジェクションの前方保持日数の目安
 * （`docs/data/projections.md`「EPG テーブルは放送済み番組を刈り取るローリング
 * ウィンドウ（8 日 + 猶予）」）だけを近似の分母として使っている。**
 *
 * ルールの時間帯条件は曜日単位（1 週間周期）で書く（`RuleTimeWindow.weekdays`）ため、
 * 8 日という半端な単位のまま件数・時間を見せると「多い/少ない」の直感的な判断が
 * しにくい。7 日（1 週間）あたりに正規化することで、曜日条件と同じ周期の単位に揃える。
 *
 * **この正規化は過大にも過小にも振れる近似であり、方向の保証は無い。** 検索結果の
 * 実際の観測スパンは「未来 8 日」だけではない --- `epg_programs` の刈り取り
 * （`PruneEpgPrograms`、`internal/db/queries/epg.sql`）は
 * `end_at < 基準時刻 - retention_grace` で、`retention_grace` の既定値は 24 時間
 * （`internal/worker/epg.go` の `defaultEpgRetentionGrace`。設定キー
 * `epg.retention_grace`（`config.example.yml`）で変更可能、フロントからは見えない）。
 * つまり**放送済みの番組が最大で `retention_grace` ぶん残る**。加えて
 * `internal/rulequery/compile.go` には `now()` を基準に未来だけへ絞る述語は無く
 * （`start_at` を暦上の範囲で絞るのは `PeriodStartAt` / `PeriodEndAt` の 2 箇所だけ
 * --- 時間帯条件が使う `startAtJST` も `start_at` を読むが、曜日と JST の壁時計の
 * 時刻に落とす式で暦上の範囲は絞らない。どちらも検索条件で明示しない限り効かない）、
 * `internal/api/search.go` も足していない。したがって条件を指定しない
 * 検索の観測スパンは「過去 `retention_grace`（既定 24h）+ 未来 ~8 日」で
 * **8 日より長くなりうる**（既定値なら約 9 日）。観測件数が多いぶん `totalCount` が
 * 実際の週あたりの値より大きくなり、8 日固定の分母で 7 日換算すると**過大に**出る
 * （例: 毎日 1 回の帯番組で未来 8 回 + 昨夜 1 回残っていると `totalCount = 9`、
 * `9 * 7/8 = 7.875` で「約 8 件」と出るが真値は週 7 件）。
 *
 * 逆に**過小にも振れる**: EPG が前方に丸 8 日ぶん入っているとは限らない。番組表が
 * 上限に使う `limitMs`（`pages/programs.tsx` の `dayOrigin(selectableDays, nowMs)`。
 * `selectableDays` は 8）は「8 日先の 0 時」（`docs/frontend/programs.md`
 * 「EPG のローリングウィンドウの終端」）なので、そこから逆算した「今からの残り日数」
 * は 7〜8 日の間で揺れる（今が何時かで決まる）。これは番組表 UI が自分で決めている
 * 上限であって EPG の前方保持量の観測ではなく、検索はこの上限を一切適用しない ---
 * あくまで「丸 8 日とは限らない」ことの目安として引いている。**実際に前方何日ぶん
 * 入っているかは測っていない（未検証）。**
 *
 * **このどちらの方向にも振れることを前提に「見込み」と呼んでおり、
 * 「多めには出ない」のような一方向の保証は書かない**（一度も真でなかった記述は
 * 古い記述より悪い --- CLAUDE.md「測っていない挙動を断言しない」）。
 *
 * 実際の観測スパンを厳密に測るには「今」・窓の終端・`retention_grace` を突き合わせる
 * 追加の問い合わせが要り、値札 1 つのために増やすコストに見合わないため、ここでは
 * 固定値 8 のままにしている。
 */
export const epgWindowDays = 8

/** ruleCostWeekDays は正規化先の単位（1 週間 = 7 日）。 */
export const ruleCostWeekDays = 7

/**
 * RuleCostSample は値札の入力。
 *
 * `totalCount` は検索結果（`{site, programId}` の配列）の全件数 --- 検索 API
 * （`POST /api/programs/search`、`internal/api/search.go`）は
 * `rulequery.MatchPrograms` の結果を `LIMIT` なしでそのまま返すため、ページングも
 * 上位 N 件打ち切りも無く、返る配列の長さがそのまま母数になる（実際にコードを
 * 確認済み。`internal/rulequery/query.go` の SQL に LIMIT は無い）。同一放送が
 * 複数 site でマッチすると複数行になるので、`totalCount` は番組数ではなく
 * 行（ruler が作る予約の見込み数）を数えている。
 *
 * `loadedDurationsMs` は番組ごとの `durationMs`。検索 API は `{site, programId}` しか
 * 返さないため、`GET /api/sites/{site}/programs/{programId}` で個別に取得できた
 * 分だけがここに入る
 * （`pages/search.tsx` が結果一覧の表示のために取得している分をそのまま再利用する
 * ので、値札のために追加のリクエストは発生しない。実測は `pages/search.tsx` の
 * `RuleCostSummary` のコメントを参照）。全件に届いていないとき
 * （`loadedDurationsMs.length < totalCount`）は平均から外挿する --- 黙って
 * 読み込み済みの合計だけを見せると実際より小さく見えるため。
 *
 * **このサンプルは無作為抽出ではない。** `loadedDurationsMs` の由来は結果の
 * `programId` 昇順の先頭 N 件（`internal/rulequery/query.go` の
 * `ORDER BY p.program_id, p.site`。programId が第 1 ソートキーなので昇順の性質は変わらない）で、
 * `programId` はネットワーク・サービス順に固まる
 * （Mirakurun 互換の合成規則 `(networkId*100000 + serviceId)*100000 + eventId`。
 * `internal/mirakc/ids.go` の `ComposeProgramID` / `SplitProgramID` と同じ式で、
 * mirakc 固有の合成規則への依存は Go 側のこの 1 箇所に閉じている --- ここでは
 * 分解はせず、「昇順に並べるとチャンネル順に固まる」ことの根拠として引くだけ）。
 * 複数チャンネルに
 * 跨がるルール（例: 30 分番組の多い GR と 120 分番組の多い BS が両方マッチする）では、
 * 先頭 N 件が特定チャンネルに偏り、平均尺が全体の平均から外れた標本になりうる。
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
 * （読み込み済みの一部でありうる。無作為抽出ではないことは `RuleCostSample` の
 * コメントの通り）の平均を `totalCount` に外挿する --- 全件の詳細を取得し直すと
 * 数百件規模のルールでリクエストが数百本に膨らむため（`pages/search.tsx` の
 * `pageSize` のコメントと同じ理由）、既に画面が持っている分だけを使う。
 *
 * 7 日への正規化係数（`ruleCostWeekDays / epgWindowDays`）が過大にも過小にも
 * 振れる近似であることは `epgWindowDays` のコメントの通り。
 */
export function estimateRuleCost(sample: RuleCostSample): RuleCostEstimate {
  const { totalCount, loadedDurationsMs } = sample
  const factor = ruleCostWeekDays / epgWindowDays
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
