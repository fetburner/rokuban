import { Link } from '@tanstack/react-router'
import { ChevronRight, Copy, ExternalLink, FolderOpen, Trash2 } from 'lucide-react'
import { useRef, useState, type KeyboardEvent } from 'react'

import type { Recording } from '@/api/generated'
import type { LiveCapability } from '@/lib/capabilities'
import { DropBadges, EncodeStatusBadges, IngestBadge, RecordingVerdictBadge } from '@/components/recording-badges'
import { useCopyLink } from '@/lib/use-copy-link'
import { useMoveRecordingToTrash } from '@/lib/use-recording-trash'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { recordingThumbnailURL } from '@/lib/recording-media'
import { programTitle } from '@/lib/program-labels'
import { sourceLabels } from '@/lib/recording-search'
import type { RecordingView } from '@/lib/recording-view'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuLinkItem,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import { useMediaQuery } from '@/lib/use-media-query'
import { cn } from '@/lib/utils'

/** RecordingRowView は録画一覧とシリーズページで共有する表示形式。`card` はサムネイルを大きく並べる。 */
export type RecordingRowView = RecordingView

/**
 * RecordingRow は録画一覧とシリーズページで共有する録画の 1 行。
 *
 * 行本体は詳細（`/recordings/$id`）への全面カバーリンク（予約一覧
 * `reservations.tsx` と同じ配置文法）。視聴・削除・エンコードは詳細ページに
 * 寄せ、一覧はインライン展開も常時「再生」列も持たない（issue #311）--- 詳細と
 * 展開が同じ `RecordingDetail` を共有していたので、一覧に同じプレイヤーを二重に
 * 抱える理由が無くなった。ごみ箱・`encodedAssets` が空の行も同じく詳細へリンクし、
 * 再生系の出し分け（`deleted_at` / encoded の有無）は詳細側の規律に任せる。
 *
 * 選択関連の props（selecting / selected / onToggle）だけは編集モードを持たない
 * シリーズページから省略できる。他は呼び出し側で決まる（`showSite` 等）ので必須にする。
 */
export function RecordingRow({
  recording,
  liveCapability,
  trash,
  showSite,
  view,
  selecting = false,
  selected = false,
  active = false,
  onToggle = () => undefined,
  onOptionClick,
  onOptionKeyDown,
  onOptionFocus,
}: {
  recording: Recording
  liveCapability: LiveCapability
  trash: boolean
  /** レジストリと読み込み済み録画の site の和集合が 2 件以上のときに出す。 */
  showSite: boolean
  /**
   * `card` はサムネイルを大きく縦に積む。**出す情報は list と同じ**で、
   * 変えるのは並べ方だけ --- 表示形式ごとに出す事実を変えると、切り替えた
   * ときに「見えていたはずのもの」が黙って消える。
   */
  view: RecordingRowView
  selecting?: boolean
  selected?: boolean
  /** 編集モードで Tab 位置を担う option かどうか。 */
  active?: boolean
  onToggle?: () => void
  /** 行クリックの Shift 修飾を親の範囲選択へ渡す。 */
  onOptionClick?: (shiftKey: boolean) => void
  /** option 自身へフォーカスがある間のキーボード操作。 */
  onOptionKeyDown?: (event: KeyboardEvent<HTMLDivElement>) => void
  /** roving tabindex の現在位置を親へ伝える。 */
  onOptionFocus?: () => void
}) {
  const [thumbFailed, setThumbFailed] = useState(false)
  const card = view === 'card'
  const rowRef = useRef<HTMLDivElement>(null)
  const finePointer = useMediaQuery('(pointer: fine)')
  const contextMenuEnabled = finePointer && !selecting
  const moveToTrash = useMoveRecordingToTrash(recording.id)
  const detailPath = `/recordings/${recording.id}`

  const copyLink = useCopyLink(detailPath)

  const row = (
    <div
      ref={rowRef}
      role={selecting ? 'option' : undefined}
      aria-selected={selecting ? selected : undefined}
      // 選択モードは roving tabindex（active 行だけ 0）。通常時は fine pointer の右クリック
      // メニューが閉じた後にフォーカスを返す先として -1（contextMenuEnabled は選択モードで偽）。
      tabIndex={selecting ? (active ? 0 : -1) : contextMenuEnabled ? -1 : undefined}
      data-recording-option={selecting ? recording.id : undefined}
      onClick={
        selecting
          ? (event) => {
              event.currentTarget.focus()
              if (onOptionClick) onOptionClick(event.shiftKey)
              else onToggle()
            }
          : undefined
      }
      onMouseDown={(event) => {
        if (!selecting || !event.shiftKey || event.button !== 0) return
        // Shift+click selects a row range. Prevent the browser from extending a text
        // selection as well; explicitly focus because preventDefault suppresses mousedown focus.
        if (event.target instanceof HTMLInputElement) return
        event.preventDefault()
        event.currentTarget.focus()
      }}
      onKeyDown={selecting ? onOptionKeyDown : undefined}
      onFocus={
        selecting
          ? (event) => {
              if (event.target === event.currentTarget) onOptionFocus?.()
            }
          : undefined
      }
      className={cn(
        // base の gap は list 分岐に持たせる。card 分岐の gap-2 と両方 base に
        // 置くと twMerge が常に後勝ち（gap-2）で解決し、base の gap-3 は
        // list でも死にクラスになる（レビュー指摘）。
        'relative hover:bg-muted/40',
        card
          ? 'flex h-full flex-col gap-2 rounded border border-border p-2'
          : 'flex min-h-14 items-center gap-3 border-b border-border px-4 py-2.5',
        selecting && 'cursor-pointer',
        selected && 'bg-muted/40',
      )}
    >
      {/* 編集モード中は全面リンクを外す。残すと checkbox と行クリックを奪う。 */}
      {!selecting && (
        <Link
          to="/recordings/$id"
          params={{ id: String(recording.id) }}
          aria-label={programTitle(recording.title)}
          className="absolute inset-0"
        />
      )}
      {selecting && (
        <input
          type="checkbox"
          aria-label={`${programTitle(recording.title)}を選択`}
          className="size-4 shrink-0 accent-primary"
          tabIndex={-1}
          checked={selected}
          onClick={(event) => event.stopPropagation()}
          onChange={onToggle}
        />
      )}
      {/*
        サムネイルは OpenAPI 外の streamer 経路。URL は recordingThumbnailURL が組み立てる。
        未生成時は 404 → onError でプレースホルダ。hasThumbnail 列は持たない（M3-4）。
        ごみ箱の録画は配信側が deleted_at IS NOT NULL を 404 にする契約（docs/api.md
        §メディア配信）なので、そもそもリクエストを出さずプレースホルダ固定にする
        （M3-18: 未生成と 404 で区別が付かない曖昧さもこれで消える）。
      */}
      <div
        className={cn(
          'aspect-video shrink-0 overflow-hidden rounded bg-muted',
          card ? 'w-full' : 'h-12',
        )}
      >
        {!trash && !thumbFailed ? (
          <img
            src={recordingThumbnailURL(recording.id)}
            alt=""
            className="size-full object-cover"
            loading="lazy"
            onError={() => setThumbFailed(true)}
          />
        ) : (
          <div className="size-full bg-muted" aria-hidden />
        )}
      </div>
      <div className="min-w-0 flex-1">
        <div className={cn('text-base', card ? 'line-clamp-2' : 'truncate')}>
          {programTitle(recording.title)}
        </div>
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
          <RecordingVerdictBadge
            recording={recording}
            liveCapability={liveCapability}
            isTrashed={trash}
          />
          {!trash && recording.status === 'finished' && recording.watchedAt === undefined && (
            <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
              未視聴
            </span>
          )}
          <IngestBadge recording={recording} />
          {/* 結論の後ろに内訳（取り込み・エンコード・ドロップ）を並べる。メタデータ列の
              末尾に回すと、狭い端末で失敗バッジが 2 行目以降に回る。 */}
          <EncodeStatusBadges recording={recording} />
          {recording.dropSummary && <DropBadges summary={recording.dropSummary} />}
          {showSite && (
            /* 文字色は text-foreground を明示（bg-muted 小バッジの合成後コントラスト
               対策。docs/frontend/design.md「コントラストは毎回測る」）。 */
            <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
              {recording.site}
            </span>
          )}
          <span className="shrink-0">{sourceLabels[recording.source]}</span>
          <span className="shrink-0">{recording.serviceName}</span>
          <span className="shrink-0">{formatDateTime(recording.startAt)}</span>
          <span className="shrink-0">{formatDuration(recording.durationMs)}</span>
          {recording.sizeBytes !== undefined && (
            <span className="shrink-0">{formatBytes(recording.sizeBytes)}</span>
          )}
          {trash && recording.deletedAt && (
            <span className="shrink-0">削除 {formatDateTime(recording.deletedAt)}</span>
          )}
        </div>
      </div>
      {/* カードは行ではないので、行末の「開く」記号は出さない（面全体がリンク）。 */}
      {!selecting && !card && <ChevronRight className="size-4 shrink-0 text-muted-foreground" />}
    </div>
  )

  if (!contextMenuEnabled) return row

  return (
    <ContextMenu>
      <ContextMenuTrigger render={row} />
      <ContextMenuContent returnFocusRef={rowRef}>
        <ContextMenuLinkItem
          closeOnClick
          render={<Link to="/recordings/$id" params={{ id: String(recording.id) }} />}
        >
          <FolderOpen />
          開く
        </ContextMenuLinkItem>
        <ContextMenuLinkItem
          closeOnClick
          render={
            <Link
              to="/recordings/$id"
              params={{ id: String(recording.id) }}
              target="_blank"
              rel="noopener noreferrer"
            />
          }
        >
          <ExternalLink />
          新しいタブで開く
        </ContextMenuLinkItem>
        <ContextMenuItem onClick={() => void copyLink()}>
          <Copy />
          リンクをコピー
        </ContextMenuItem>
        {!trash && (
          <ContextMenuItem
            variant="destructive"
            disabled={moveToTrash.pending}
            onClick={moveToTrash.moveToTrash}
          >
            <Trash2 />
            ごみ箱へ移す
          </ContextMenuItem>
        )}
      </ContextMenuContent>
    </ContextMenu>
  )
}
