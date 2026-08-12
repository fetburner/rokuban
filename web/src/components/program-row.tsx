import { Link } from '@tanstack/react-router'
import { ChevronDown, Loader2 } from 'lucide-react'
import { useState } from 'react'

import { useGetProgram, type ProgramListItem, type ProgramOverridesInput } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EncodeSettingsFields } from '@/components/encode-settings-fields'
import { ProgramOverlapWarning } from '@/components/program-overlap-warning'
import { Button } from '@/components/ui/button'
import { useLiveEnabled } from '@/lib/capabilities'
import {
  defaultEncodeSettingsValue,
  encodeSettingsError,
  encodeSettingsOverridesBody,
  sameEncodeSettingsValue,
  type EncodeSettingsValue,
} from '@/lib/encode-settings'
import { formatDuration, formatTime, isAiring } from '@/lib/format'
import { useCurrentSite } from '@/lib/site'
import { cn } from '@/lib/utils'

/**
 * ProgramRow は番組リストの 1 行。
 *
 * 行本体のタップで詳細を展開し、予約ボタンは右端に固定幅で分離する。
 * スクロール中に予約ボタンへ誤って触れないようタップ領域を分けている。
 *
 * 展開すると（まだ予約されていない番組に限り）encodeProfiles / keepOriginal
 * を指定する欄も出す（issue #132）。「予約」ボタンは、この欄が既定から
 * 変わっていれば `onReserve` に overrides の PATCH ボディも渡す ---
 * 既定のままなら渡さない（意味の無い override 行を作らない。CLAUDE.md
 * 不変条件 10）。
 *
 * 展開パネル（`ProgramDetail` とこの欄）はチェックボックス・セレクトを
 * 持つ対話的な要素を含みうるので、行全体の開閉を切り替える `<button>` の
 * 外（兄弟要素）に置く --- `<button>` の中に `<input>`/`<select>` を置くと
 * 無効な HTML になり、それらへのクリックが展開トグルにもバブリングして
 * 意図しない開閉を起こす。
 *
 * 展開領域には外向きの導線（「ライブで見る」「予約の詳細」）も置く
 * （issue #229）。行本体 = 展開 / 右端 44px = 予約、というタップ予算
 * （docs/frontend/reservations.md §予約はワンタップ）に触れないよう、
 * 折りたたみ行ではなく展開領域側に置く。
 */
export function ProgramRow({
  program,
  serviceName,
  reserved,
  pending,
  onReserve,
  onCancel,
}: {
  program: ProgramListItem
  serviceName?: string
  reserved: boolean
  pending: boolean
  onReserve: (overrides?: ProgramOverridesInput) => void
  onCancel: () => void
}) {
  const site = useCurrentSite()
  const liveEnabled = useLiveEnabled()
  const [expanded, setExpanded] = useState(false)
  // 展開して初めて出る欄で、開かなければ既定値のまま
  // （= 「予約」を押しても overrides の PATCH は飛ばない）。
  const [encodeValue, setEncodeValue] = useState<EncodeSettingsValue>(defaultEncodeSettingsValue())

  const encodeError = encodeSettingsError(encodeValue.keepOriginal, encodeValue.encodeProfiles)
  const encodeDirty = !sameEncodeSettingsValue(encodeValue, defaultEncodeSettingsValue())
  const reserveBlocked = pending || (encodeDirty && encodeError !== undefined)

  const handleReserve = () => {
    if (reserveBlocked) return
    onReserve(encodeSettingsOverridesBody(encodeValue, defaultEncodeSettingsValue()))
  }

  const detailId = `program-row-detail-${program.programId}`

  // now の評価タイミングは「展開されて描画される瞬間（＋その後の再レンダー）」で
  // 足りるとし、専用の tick タイマーは持たない。
  //
  // 理由: このリンクは番組 ID を運ばず `serviceId`（チャンネル）だけを渡すので、
  // 境界を挟んで多少ズレても遷移先を誤ることはない --- 遷移先の /live 画面が
  // 自前で「いま何が流れているか」を再取得して表示するので、真実はそちら側に
  // ある（issue #229 の指示どおり）。行を展開したまま番組終了をまたいでも、
  // リンクの消える/現れるタイミングが数十秒ずれるだけ。pages/live.tsx の
  // `nowMs`（30 秒間隔の tick）は「いま放送中の番組名」という常時表示のために
  // 要るが、ここは真偽 1 個の出し分けで代償が小さいため見送る。
  const showLiveLink = liveEnabled && isAiring(program.startAt, program.endAt)

  return (
    <div className="flex flex-col border-b border-border">
      <div className="flex items-stretch">
        <button
          type="button"
          aria-expanded={expanded}
          aria-controls={detailId}
          onClick={() => setExpanded((v) => !v)}
          className="flex min-h-14 min-w-0 flex-1 items-center gap-3 px-4 py-2.5 text-left hover:bg-muted/50"
        >
          <div className="w-11 shrink-0 text-sm">
            {/* 放送中の行は**色を使わず**太さで立てる。理由は 2 つあり、どちらも
                docs/frontend/design.md にある: (1) 旧 `text-primary` は地の墨と
                同値になったので、そのままでは「いま」が立たない。(2) タリーレッドに
                すると、チャンネル数ぶんの行が同時に赤くなって信号として機能しない
                （リストの ON AIR は希少ではない）。線と札で示す「いま」は
                番組表グリッド側が持つ */}
            <div
              // e2e（web/e2e/design.mjs）が「この要素に信号色が付いていないこと」を
              // 測る。クラス名でセレクタを組むと、そのユーティリティクラスが
              // 別の要素へ移っただけで**別の要素を測ったまま通る**
              data-testid="program-row-time"
              className={cn(isAiring(program.startAt, program.endAt) && 'font-medium')}
            >
              {formatTime(program.startAt)}
            </div>
          </div>
          <div className="min-w-0 flex-1">
            <div className="truncate text-sm">{program.name}</div>
            <div className="flex items-center gap-2 text-xs text-muted-foreground">
              {serviceName && <span className="truncate">{serviceName}</span>}
              <span className="shrink-0">{formatDuration(program.durationMs)}</span>
              {!program.isFree && <span className="shrink-0">有料</span>}
            </div>
            {/* 予約する前に見せる（issue #24 M2-8）。展開しなくても常に見える位置に置く
                （予約後に知らせても遅いため）。取消可能な「取消」ボタン側（既に予約済み）
                では自分自身との重なりしか出ようがないので問い合わせ自体をしない。 */}
            {!reserved && <ProgramOverlapWarning site={site} programId={program.programId} />}
          </div>
          <ChevronDown
            className={cn(
              'size-4 shrink-0 text-muted-foreground transition-transform',
              expanded && 'rotate-180',
            )}
          />
        </button>

        {/* 予約ボタンは行本体と分離した固定幅。最小 44px のタップ領域を確保する */}
        <div className="flex w-20 shrink-0 items-center justify-center border-l border-border">
          <Button
            variant={reserved ? 'destructive' : 'default'}
            size="sm"
            disabled={reserved ? pending : reserveBlocked}
            onClick={reserved ? onCancel : handleReserve}
            className="min-h-11 w-full rounded-none"
          >
            {pending ? (
              <Loader2 className="size-4 animate-spin" />
            ) : reserved ? (
              '取消'
            ) : (
              '予約'
            )}
          </Button>
        </div>
      </div>

      {expanded && (
        <div id={detailId} className="px-4 pb-3">
          <ProgramDetail program={program} />

          {/* 固有名詞（放送中のチャンネル・予約という実体）はリンクにする
              （issue #229「決定済みの方向」）。折りたたみ行のタップ予算には
              触れず、展開領域側に置く。 */}
          {(showLiveLink || reserved) && (
            <div className="mt-3 flex flex-wrap gap-4 text-xs">
              {showLiveLink && (
                <Link
                  to="/live"
                  search={{ serviceId: program.serviceId }}
                  className="text-primary underline-offset-2 hover:underline"
                >
                  ライブで見る
                </Link>
              )}
              {/* 予約済みの番組の overrides 編集は予約詳細画面の担当（下記コメント）。
                  その画面への導線をここに置く。 */}
              {reserved && (
                <Link
                  to="/reservations/$site/$programId"
                  params={{ site, programId: String(program.programId) }}
                  className="text-primary underline-offset-2 hover:underline"
                >
                  予約の詳細
                </Link>
              )}
            </div>
          )}

          {/* 予約済みの番組は「予約詳細」画面（reservation-detail.tsx）で
              overrides を編集する。ここで扱うのは「これから予約する」番組だけ。 */}
          {!reserved && (
            <div className="mt-3 border-t border-border pt-3">
              <EncodeSettingsFields
                value={encodeValue}
                onChange={setEncodeValue}
                disabled={pending}
                note="ここでの変更はこの画面に保存ボタンを持ちません。「予約」を押した時点で反映されます。"
              />
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * ProgramDetail は展開時に表示する詳細。
 *
 * 説明・出演者・映像音声属性は一覧レスポンスに含まれないため、
 * 展開したときに GET /api/programs/{id} で取得する（段階的開示）。
 */
function ProgramDetail({ program }: { program: ProgramListItem }) {
  const site = useCurrentSite()
  const detail = useGetProgram(site, program.programId)
  const d = unwrap(detail.data)

  return (
    <div className="flex flex-col gap-2 text-xs">
      {program.description && (
        <p className="whitespace-pre-wrap text-muted-foreground">{program.description}</p>
      )}

      {detail.isPending && <p className="text-muted-foreground">詳細を読み込み中…</p>}
      {detail.isError && <p className="text-destructive">詳細の取得に失敗しました</p>}

      {d?.extended && Object.keys(d.extended).length > 0 && (
        <dl className="flex flex-col gap-1">
          {Object.entries(d.extended).map(([key, value]) => (
            <div key={key}>
              <dt className="font-medium">{key}</dt>
              <dd className="whitespace-pre-wrap text-muted-foreground">{value}</dd>
            </div>
          ))}
        </dl>
      )}

      {(d?.video || d?.audios) && (
        <p className="text-muted-foreground">
          {[
            d.video?.resolution,
            d.audios?.length ? `音声 ${d.audios.length}` : undefined,
            d.audios?.flatMap((a) => a.langs ?? []).join('/') || undefined,
          ]
            .filter(Boolean)
            .join(' · ')}
        </p>
      )}
    </div>
  )
}
