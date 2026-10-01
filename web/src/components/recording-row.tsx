import { Link } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'
import { useState } from 'react'

import type { Recording } from '@/api/generated'
import { DropBadges, EncodeStatusBadges, IngestBadge, StatusBadge } from '@/components/recording-badges'
import { formatBytes, formatDateTime, formatDuration } from '@/lib/format'
import { programTitle } from '@/lib/program-labels'
import { sourceLabels } from '@/lib/recording-search'
import type { RecordingView } from '@/lib/recording-view'
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
 * シリーズページから省略できる。他は権威（`useLiveEnabled` /
 * `shouldShowRecordingSite` 等）が呼び出し側にあるので必須にする。
 */
export function RecordingRow({
  recording,
  trash,
  showSite,
  view,
  selecting = false,
  selected = false,
  onToggle = () => undefined,
  liveEnabled,
}: {
  recording: Recording
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
  onToggle?: () => void
  liveEnabled: boolean
}) {
  const [thumbFailed, setThumbFailed] = useState(false)
  const card = view === 'card'

  return (
    <div
      role={selecting ? 'option' : undefined}
      aria-selected={selecting ? selected : undefined}
      onClick={selecting ? onToggle : undefined}
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
          checked={selected}
          onClick={(event) => event.stopPropagation()}
          onChange={onToggle}
        />
      )}
      {/*
        サムネイルは openapi 外の streamer 経路（/api/media/recordings/{id}/thumbnail）。
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
            src={`/api/media/recordings/${recording.id}/thumbnail`}
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
          <StatusBadge status={recording.status} />
          {!trash && recording.status === 'finished' && recording.watchedAt === undefined && (
            <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-foreground">
              未視聴
            </span>
          )}
          <IngestBadge recording={recording} />
          {/* エンコード失敗は StatusBadge / IngestBadge と同じ「この録画の
              パイプラインがどこで止まっているか」なので隣に置く。メタデータ列の
              末尾（DropBadges の後）に置くと、狭い端末で失敗バッジが 2 行目
              以降に回る（親は flex-wrap なので隠れはしない）。単体ページの
              ヘッダーも同じ並び。docs/frontend/recordings.md */}
          <EncodeStatusBadges recording={recording} />
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
          {recording.dropSummary && <DropBadges summary={recording.dropSummary} />}
        </div>
      </div>
      {!selecting && liveEnabled && recording.status === 'recording' && (
        <Link
          to="/recordings/$id"
          params={{ id: String(recording.id) }}
          hash="chase"
          aria-label={`${programTitle(recording.title)}を追っかけ再生`}
          className="relative z-10 shrink-0 rounded border border-border px-2 py-1 text-xs text-primary hover:bg-muted"
        >
          追っかけ
        </Link>
      )}
      {/* カードは行ではないので、行末の「開く」記号は出さない（面全体がリンク）。 */}
      {!selecting && !card && <ChevronRight className="size-4 shrink-0 text-muted-foreground" />}
    </div>
  )
}
