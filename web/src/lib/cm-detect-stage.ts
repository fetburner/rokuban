/**
 * cmDetectStageMessage は CM 検出の内部工程を利用者向けの一文へ変換する。
 *
 * stage は worker が観測した値で、空・未知の値は古い試行や将来の値を壊れた
 * 生ログとして表示しないため、一般的な失敗へ畳み込む。
 */
export function cmDetectStageMessage(stage: string | null | undefined): string {
  switch (stage) {
    case 'logo':
      return 'ロゴを見つけられず、CM を検出できませんでした。'
    case 'area':
      return '教えた枠が録画の解像度と合わないため、枠を使えませんでした。'
    case 'resolution':
      return '覚えたロゴは別の解像度の録画から作られたため、この録画には使えませんでした。'
    case 'match':
      return '覚えたロゴがこの録画にほとんど映っておらず、CM を検出できませんでした。局のロゴが変わった可能性があります。'
    case 'setup':
    case 'probe':
    case 'chapter':
    case 'join':
    case 'parse':
    case 'save':
    case 'stopped':
      return 'CM 検出の処理が失敗しました。ロゴの枠では直せない失敗です。'
    case 'adopt':
      return 'この局はロゴの採用待ちです。局の画面で候補を確かめて採用してください。'
    default:
      return '失敗の種類が記録されていない古い試行です。'
  }
}

/**
 * isStationFixableCMStage は、解消する操作が局の CM ロゴ画面にしか無い失敗段階かを返す。
 *
 * 段階ごとの解消操作は次のとおり。
 * - `logo`: 局で枠を教え、候補を採用する。
 * - `area`: 局で枠を教え直す（枠と解像度が合わない古い試行の意味）。
 * - `adopt`: 局で候補を採用する。
 * - `match`: 局でロゴを忘れる、または枠を教え直して候補を採用する。
 * - `resolution`: 局でその解像度の録画から枠を教えて採用する。
 *
 * それ以外（`setup` / `probe` / `chapter` など、null と未知を含む）は録画詳細の再検出で直す。
 * `state === 'failed'` の判定は呼び出し側の責任で、この関数は stage だけを見る。
 */
export function isStationFixableCMStage(stage: string | null | undefined): boolean {
  return stage === 'logo' || stage === 'area' || stage === 'match' || stage === 'resolution' || stage === 'adopt'
}
