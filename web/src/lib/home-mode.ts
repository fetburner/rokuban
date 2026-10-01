export type HomeMode = 'watch' | 'ops'

export const HOME_MODE_STORAGE_KEY = 'rokuban:home:mode'

/** 未知の URL / storage 値は設定されていないものとして扱う。 */
export function parseHomeMode(value: unknown): HomeMode | undefined {
  return value === 'watch' || value === 'ops' ? value : undefined
}

/** URL > 端末の好み > 初回既定（見る）の順でホームのモードを決める。 */
export function resolveHomeMode(urlMode: unknown, storedMode: unknown): HomeMode {
  return parseHomeMode(urlMode) ?? parseHomeMode(storedMode) ?? 'watch'
}

/** 読み込みのたびに保存値を検証する。Storage が使えない環境は未設定扱い。 */
export function readHomeModePreference(): HomeMode | undefined {
  try {
    return parseHomeMode(window.localStorage.getItem(HOME_MODE_STORAGE_KEY))
  } catch {
    return undefined
  }
}

/** 保存に失敗しても URL 上の選択と画面操作は成立させる。 */
export function saveHomeModePreference(mode: HomeMode): void {
  try {
    window.localStorage.setItem(HOME_MODE_STORAGE_KEY, mode)
  } catch {
    // private mode や無効化された storage は、保存されないだけに縮退する。
  }
}
