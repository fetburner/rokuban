/** 録画一覧とシリーズ一覧が共有する表示形式。 */
export type RecordingView = 'list' | 'card'

/**
 * RECORDING_VIEW_KEY は録画・シリーズ一覧で共有する表示形式の localStorage キー。
 *
 * **URL ではなく端末に持つ**（`tab` や絞り込みと違う扱い）。表示形式は共有
 * リンクの宛先ではなく、その端末で見やすい形の好みだから
 * （docs/frontend/design.md §個人化）。`components/app-shell.tsx` の
 * サイドバー畳みと同じ `rokuban:<関心事>:...` の命名。
 */
export const RECORDING_VIEW_KEY = 'rokuban:recordings:view'

/** 保存済みの表示形式を読む。無い/読めない場合はリスト表示にする。 */
export function loadRecordingView(): RecordingView {
  try {
    return localStorage.getItem(RECORDING_VIEW_KEY) === 'card' ? 'card' : 'list'
  } catch {
    return 'list'
  }
}

/** 表示形式を保存する。保存できなくても現在の画面の切替は成立させる。 */
export function saveRecordingView(view: RecordingView): void {
  try {
    localStorage.setItem(RECORDING_VIEW_KEY, view)
  } catch {
    // private mode などでは次回起動時に既定のリスト表示へ戻る。
  }
}
