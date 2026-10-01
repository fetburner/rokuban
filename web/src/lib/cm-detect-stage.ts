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
    case 'setup':
    case 'probe':
    case 'chapter':
    case 'join':
    case 'parse':
    case 'save':
    case 'stopped':
      return 'CM 検出の処理が失敗しました。ロゴの枠では直せない失敗です。'
    default:
      return '失敗の種類が記録されていない古い試行です。'
  }
}
