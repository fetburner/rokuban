/**
 * parsePositiveIntId は URL などの生の値を「正の安全整数の識別子」としてパースする
 * 共有ヘルパー（issue #275）。`rules.id`（`bigint` PK、Go 側 `int64`）や `/live` の
 * `serviceId` のように、シーケンス/SI 由来で 1 以上しか存在しない識別子に共通する形。
 *
 * 空文字列・空白のみの文字列は先に弾く --- `Number('') === 0` で「id 0」という
 * 具体的な値に化ける。欠落は「無し」であるべき（`lib/programs-search.ts` の
 * `parseAt` と同じ理由）。
 *
 * `Number.isFinite` と `Number.isInteger` だけでなく `Number.isSafeInteger` を使う ---
 * `Number.MAX_SAFE_INTEGER`（`9007199254740991`）を超える値は JS の `number` で
 * 正確に表せず、黙って別の値に丸まる（実測: `9007199254740993` は `Number()` の
 * 時点で既に `9007199254740992` になる）。丸まった値は利用者が書いた id ではなく
 * 「別の id を指す値」なので、黙って引くより落とす。`1e30` も
 * `Number.isSafeInteger(1e30) === false` なので同じ経路で落ちる。指数表記そのものは
 * 禁止しない --- `1e3` は数値として一意なので通す（`parseAt` と同じ流儀。文字列形の
 * 門（`/^\d+$/` 等）を足すと `+5` や前後空白まで落ちてしまう一方、指数表記を拒む
 * 理由にはならない）。
 *
 * `n > 0` も見る --- 対象の識別子はシーケンス由来の PK や SI の値空間で 1 以上
 * しか存在しない。`0` や負値はどう転んでも何も同定しないので、`GET .../0` を
 * 投げて 404 をもらう往復ではなく「指定なし」に落とす方が正しい。
 */
export function parsePositiveIntId(raw: unknown): number | undefined {
  if (typeof raw === 'string' && raw.trim() === '') return undefined
  const n = typeof raw === 'number' ? raw : typeof raw === 'string' ? Number(raw) : NaN
  return Number.isSafeInteger(n) && n > 0 ? n : undefined
}
