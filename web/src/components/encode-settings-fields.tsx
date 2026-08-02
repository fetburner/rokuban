import { useEffect, useState } from 'react'

import {
  useListEncodeProfiles,
  type EncodeProfileSummary,
  type ProgramOverridesInput,
  type ReservationOverrides,
} from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { Button } from '@/components/ui/button'
import { Field, Select } from '@/components/ui/field'
import {
  encodeSettingsError,
  encodeSettingsValueFromOverrides,
  hasEncodeOverride,
  keepOriginalLabel,
  sameEncodeSettingsValue,
  toggleProfile,
  type EncodeSettingsValue,
  type KeepOriginal,
} from '@/lib/encode-settings'
import { cn } from '@/lib/utils'

export type { EncodeSettingsValue }

type EncodeSettingsFieldsProps = {
  value: EncodeSettingsValue
  onChange: (next: EncodeSettingsValue) => void
  /**
   * 追加の説明文（予約 overrides では「ルールに戻す」など）。
   * 省略時は原本削除の注意だけを出す。
   */
  note?: string
  /** 無効化（保存中など）。 */
  disabled?: boolean
  className?: string
}

/**
 * EncodeSettingsFields は encodeProfiles 複数選択 + keepOriginal の編集欄。
 *
 * プロファイル一覧は `GET /api/encode-profiles` から取る（設定に無い名前を
 * 自由入力させない）。`until_encoded` かつプロファイル空はクライアントでも止め、
 * 理由をボタン横に出せるよう `encodeSettingsError` と対になる。
 *
 * 原本削除後は再エンコードできない、という注意を常に出す（issue #68）。
 */
export function EncodeSettingsFields({
  value,
  onChange,
  note,
  disabled,
  className,
}: EncodeSettingsFieldsProps) {
  const profilesQuery = useListEncodeProfiles()
  const profiles = unwrap(profilesQuery.data) ?? []
  const error = encodeSettingsError(value.keepOriginal, value.encodeProfiles)

  return (
    <div className={cn('flex flex-col gap-3', className)}>
      {/* 複数チェックボックスを包むので Field（label）は使わない。
          label の入れ子は accessible name が壊れ、テストも実機の読み上げも壊れる。 */}
      <div className="flex flex-col gap-1 text-xs text-muted-foreground">
        <span>エンコードプロファイル</span>
        <ProfileMultiSelect
          profiles={profiles}
          selected={value.encodeProfiles}
          disabled={disabled || profilesQuery.isPending}
          isError={profilesQuery.isError}
          onToggle={(name) =>
            onChange({
              ...value,
              encodeProfiles: toggleProfile(value.encodeProfiles, name),
            })
          }
        />
      </div>

      <Field label="原本の保持">
        <Select
          value={value.keepOriginal}
          disabled={disabled}
          onChange={(e) =>
            onChange({
              ...value,
              keepOriginal: e.target.value as KeepOriginal,
            })
          }
        >
          <option value="always">{keepOriginalLabel('always')}</option>
          <option value="until_encoded">{keepOriginalLabel('until_encoded')}</option>
        </Select>
      </Field>

      <p className="text-xs text-muted-foreground">
        原本を削除したあとは再エンコードできません。エンコードが完了するまで原本は残ります。
        {note !== undefined && <> {note}</>}
      </p>

      {error !== undefined && (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}

/**
 * EncodeOverridesEditor は 1 番組ぶんの overrides（encodeProfiles / keepOriginal）
 * を編集して `PATCH .../overrides` に送る。
 *
 * `Reservation` 型には依存しない（`(site, programId)` は呼び出し側が
 * `onSave` の中で知っていればよく、ここでは overrides の現在値だけを受け取る）
 * ---
 * 予約詳細画面（既存の予約）だけでなく、番組表からの「予約」導線
 * （まだ予約行が無い番組）でも同じ編集フォームを使うため（issue #132）。
 *
 * `overrides` が未設定（`undefined` / `null`）なら「上書きなし」の表示・
 * 初期値になる。
 */
export function EncodeOverridesEditor({
  overrides,
  isPending,
  onSave,
}: {
  overrides: ReservationOverrides | null | undefined
  isPending: boolean
  onSave: (body: ProgramOverridesInput) => void
}) {
  const fromOverrides = encodeSettingsValueFromOverrides(overrides)
  const [value, setValue] = useState<EncodeSettingsValue>(fromOverrides)
  // サーバー側の overrides が変わったらフォームを同期する（保存後の invalidate など）。
  useEffect(() => {
    setValue(encodeSettingsValueFromOverrides(overrides))
  }, [overrides])

  const error = encodeSettingsError(value.keepOriginal, value.encodeProfiles)
  const dirty = !sameEncodeSettingsValue(value, fromOverrides)
  const currentlyOverridden = hasEncodeOverride(overrides)

  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border p-3">
      <p className="text-xs text-muted-foreground">
        現在の上書き:{' '}
        {currentlyOverridden
          ? `${keepOriginalLabel(fromOverrides.keepOriginal)} / ${
              fromOverrides.encodeProfiles.length === 0
                ? 'プロファイルなし'
                : fromOverrides.encodeProfiles.join(', ')
            }`
          : 'なし（ルールまたは既定）'}
      </p>
      <EncodeSettingsFields
        value={value}
        onChange={setValue}
        disabled={isPending}
        note="この予約だけを上書きします。"
      />
      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          size="lg"
          disabled={!dirty || error !== undefined || isPending}
          onClick={() =>
            onSave({
              keepOriginal: value.keepOriginal,
              encodeProfiles: value.encodeProfiles,
            })
          }
        >
          {isPending ? '保存中…' : '上書きを保存'}
        </Button>
        {currentlyOverridden && (
          <Button
            type="button"
            variant="outline"
            size="lg"
            disabled={isPending}
            onClick={() =>
              onSave({
                reset: ['keepOriginal', 'encodeProfiles'],
              })
            }
          >
            ルールに戻す
          </Button>
        )}
      </div>
    </div>
  )
}

function ProfileMultiSelect({
  profiles,
  selected,
  disabled,
  isError,
  onToggle,
}: {
  profiles: EncodeProfileSummary[]
  selected: string[]
  disabled?: boolean
  isError: boolean
  onToggle: (name: string) => void
}) {
  if (isError) {
    return (
      <p className="text-xs text-destructive">プロファイル一覧の取得に失敗しました</p>
    )
  }
  if (profiles.length === 0) {
    return (
      <p className="text-xs text-muted-foreground">
        設定にエンコードプロファイルがありません（config.encode.profiles）
      </p>
    )
  }

  return (
    <ul
      role="group"
      aria-label="エンコードプロファイル"
      className="flex flex-col gap-1 rounded-lg border border-border p-2"
    >
      {profiles.map((p) => {
        const checked = selected.includes(p.name)
        return (
          <li key={p.name}>
            <label
              className={cn(
                'flex min-h-9 cursor-pointer items-center gap-2 rounded-md px-2 text-sm text-foreground',
                'hover:bg-muted/60',
                disabled && 'pointer-events-none opacity-50',
              )}
            >
              <input
                type="checkbox"
                className="size-4 accent-primary"
                checked={checked}
                disabled={disabled}
                onChange={() => onToggle(p.name)}
              />
              <span className="min-w-0 flex-1 truncate">{p.name}</span>
              {p.container !== undefined && (
                <span className="shrink-0 text-xs text-muted-foreground">{p.container}</span>
              )}
            </label>
          </li>
        )
      })}
    </ul>
  )
}
