import { Link } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useRef, useState } from 'react'

import {
  getGetRecordingChaptersQueryKey,
  listRecordings,
  useDeleteRecordingChapterEdits,
  useGetRecordingChapters,
  useListLiveProfiles,
  useListEncodeProfiles,
  useListRules,
  useListSites,
  usePutRecordingChapterEdits,
  usePutRecordingWatched,
  useDeleteRecordingWatched,
  useReencodeRecordingProfile,
  useRetryRecordingCMDetection,
  useSetRecordingEncodePolicy,
  type ChapterSpan,
  type Recording,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { DropStatsTable } from '@/components/drop-stats-table'
import { DetailHeading, DetailSummary } from '@/components/detail-heading'
import { RecordingActions, RecordingAssetControls } from '@/components/recording-actions'
import { RecordingPlayer } from '@/components/recording-player'
import { LivePlayer } from '@/components/live-player'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import { formatBytes, formatDateTime, formatTime } from '@/lib/format'
import { cmDetectStageMessage } from '@/lib/cm-detect-stage'
import { ingestDisplay, type IngestDisplay } from '@/lib/ingest'
import { useCMDetectEnabled, useLiveEnabled } from '@/lib/capabilities'
import { recordingFileURL } from '@/lib/playback-position'
import { liveProfileLabel, validLiveProfile } from '@/lib/live'
import { ruleDisambiguator } from '@/lib/rule-label'
import { shouldShowRecordingSite, sourceLabels } from '@/lib/recording-search'
import { recordingsQueryKeyPrefix } from '@/lib/events'
import { programTitle } from '@/lib/program-labels'
import { nextEpisode } from '@/lib/series'

/**
 * ingestDetailText は詳細ページの「取り込み」欄の文言（issue #212）。
 *
 * 一覧のバッジ（`IngestBadge`）より一段詳しく、分母が取れていれば
 * 「1.2 GB / 3.4 GB」まで出す。**分母が無いときに割合をでっち上げない**
 * （mirakc が record の length を返さない構成があるため。`openapi.yaml` の
 * `IngestProgress.expectedBytes`）。
 *
 * `originalDeleted` をここで言い切れるのは、サーバーが「`kind='original'` の
 * 行が state を問わず存在するか」を見て `committed` を返しているから ---
 * `sizeBytes` の省略だけを見ていた頃は未 ingest と区別できず、未 ingest の
 * 録画に「削除済み」と読める表示が出ていた（issue #211）。
 */
function ingestDetailText(display: IngestDisplay): string {
  switch (display.kind) {
    case 'pending':
      return '待機中（まだ原本を取り込んでいません）'
    case 'originalDeleted':
      return '完了（原本は削除済み）'
    case 'transferring': {
      const size =
        display.expectedBytes !== undefined
          ? `${formatBytes(display.writtenBytes)} / ${formatBytes(display.expectedBytes)}`
          : formatBytes(display.writtenBytes)
      const percent = display.percent !== undefined ? `（${display.percent}%）` : ''
      return `${display.stale ? '転送中・停滞' : '転送中'} ${size}${percent}`
    }
  }
}

function formatChaseElapsed(seconds: number): string {
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remainder = seconds % 60
  const parts = [
    hours > 0 ? `${hours}時間` : '',
    minutes > 0 ? `${minutes}分` : '',
    remainder > 0 || (hours === 0 && minutes === 0) ? `${remainder}秒` : '',
  ]
  return parts.filter(Boolean).join('')
}

function formatChasePosition(startAt: string, offsetSeconds: number): string {
  const startMs = Date.parse(startAt)
  const positionAt = new Date(startMs + offsetSeconds * 1000).toISOString()
  return `${formatTime(positionAt)}（開始から${formatChaseElapsed(offsetSeconds)}）`
}

function formatCMOffset(ms: number): string {
  const seconds = Math.floor(ms / 1000)
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remainder = seconds % 60
  return [hours, minutes, remainder].map((n) => String(n).padStart(2, '0')).join(':')
}

/**
 * RecordingDetail は録画 1 件の詳細本体（プレイヤー・メタデータ・操作）。
 * 単体ページ（`pages/recording-detail.tsx`）が使う。一覧はインライン展開せず、
 * 行本体から単体ページへ移動する（issue #311）。
 *
 * 単体ページはここで行われる削除 / 復元 / 完全削除 / 追加エンコードのどの
 * mutate が成功しても自分自身を再描画したいが、それを prop で 1 段ずつ手渡す
 * 形（例: `onMutated`）は「この部品の下で mutate する者は全員 prop を受け取る」
 * という規律を要求し、守らせる仕組みが無い。実際、最初の実装はこの穴を
 * `RecordingActions` にだけ塞いで `AddEncodeProfilesAction`（`recording-actions.tsx`）
 * を素通しし、単体ページで「追加エンコードを依頼」しても再検証されない不具合になった
 * （issue #232 のレビューで実機再現）。
 *
 * 直し方は prop を増やすことではなく、**単体ページ自身のクエリキーを一覧の
 * invalidate に前方一致させる**（`pages/recording-detail.tsx` の
 * `recordingDetailQueryKey` 参照）。ここに mutater を何人足しても、
 * 各自が今のまま `[recordingsQueryKeyPrefix]` を invalidate するだけで単体ページも
 * 自動的に巻き込まれるので、`RecordingDetail` 自身に配線用の prop は要らない。
 * `components/` に移ってページから 1 hop 遠くなった今も同じ規律が効く ---
 * ここに mutater を足す側は「単体ページへ配線したか」を気にせず、狭いキーを
 * invalidate しない限り自動で巻き込まれる。
 */
export function RecordingDetail({
  recording,
  trash,
  chase = false,
  liveProfile,
  startAtBeginning = false,
  onSelectLiveProfile,
}: {
  recording: Recording
  trash: boolean
  chase?: boolean
  /** ホームの「最初から」から来た場合は保存位置を復元しない。 */
  startAtBeginning?: boolean
  /** 追っかけ再生の画質（`?liveProfile=`。issue #874）。未検証の生の値。 */
  liveProfile?: string
  onSelectLiveProfile: (name: string) => void
}) {
  const liveEnabled = useLiveEnabled()
  const cmDetectEnabled = useCMDetectEnabled()
  const queryClient = useQueryClient()
  const toast = useToast()
  const setEncodePolicy = useSetRecordingEncodePolicy()
  const retryCMDetection = useRetryRecordingCMDetection()
  const [chasing, setChasing] = useState(chase)
  // undefined means the user has not chosen a start position yet: the default
  // chase session may restore the saved VOD position. Once the button is
  // clicked, even an explicit 0 must be distinguishable so it can reset to the
  // recording head instead of restoring that saved position.
  const [chaseOffsetSeconds, setChaseOffsetSeconds] = useState<number | undefined>(undefined)
  const [selectedChaseOffsetSeconds, setSelectedChaseOffsetSeconds] = useState(0)
  const selectedChaseOffsetRef = useRef(0)
  const showChase = !trash && recording.status === 'recording' && liveEnabled && chasing
  const encodedAssets = recording.encodedAssets ?? []
  const hasOriginal = recording.sizeBytes !== undefined
  const showOriginalVOD =
    !trash && recording.status === 'finished' && liveEnabled && hasOriginal && encodedAssets.length === 0
  // 追っかけか原本 VOD を表示するときだけ live プロファイルを取る。一覧は
  // セレクタ用で、取得できなくても先頭プロファイルで再生できる既存契約を保つ。
  const liveProfilesQuery = useListLiveProfiles({ query: { enabled: showChase || showOriginalVOD } })
  const liveProfiles = useMemo(
    () => unwrap(liveProfilesQuery.data) ?? [],
    [liveProfilesQuery.data],
  )
  // 未知の名前は落として既定（サーバー側の先頭）に倒す。streamer は未知の
  // 名前を 400 で返すので（`internal/streamer/live.go` の Chase）、旧ブックマーク・
  // 綴り違いの共有リンクをエラー画面にしない（`lib/live.ts` の `validLiveProfile`）。
  //
  // **既定は URL に書き戻さない。** 明示的に選んだ値だけを載せる --- 既定は
  // 両側で同じ先頭プロファイルに解決するので、URL に書く意味が無い
  // （`pages/live.tsx` と同じ規律）。
  const explicitLiveProfile = useMemo(
    () => validLiveProfile(liveProfiles, liveProfile),
    [liveProfiles, liveProfile],
  )
  // oxlint-disable-next-line react/purity -- the live recording edge needs a clock snapshot
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!showChase) return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [showChase])
  const recordingStartMs = recording.startedAt === undefined ? Number.NaN : Date.parse(recording.startedAt)
  const availableChaseSeconds = Number.isFinite(recordingStartMs)
    ? Math.max(0, Math.floor((now - recordingStartMs) / 1000))
    : 0
  // The current edge is still moving while the recording is active. Leave one
  // second of headroom so the initial Range is not exactly at a moving EOF.
  const maxChaseOffsetSeconds = Math.max(0, availableChaseSeconds - 1)
  const selectedOffset = Math.min(selectedChaseOffsetSeconds, maxChaseOffsetSeconds)
  const programStartMs = Date.parse(recording.startAt)
  const plannedChaseSeconds = Math.max(0, Math.ceil(recording.durationMs / 1000))
  // `program_duration_ms` is copied into the recording when it is created. The
  // watcher updates status/timestamps, not the planned duration, so an extension
  // must grow this client-side axis from the elapsed recording time.
  const timelineChaseSeconds = Math.max(1, plannedChaseSeconds, availableChaseSeconds)
  const timelineExtended = availableChaseSeconds > plannedChaseSeconds
  const recordedProgressPercent = Math.min(100, (availableChaseSeconds / timelineChaseSeconds) * 100)
  const plannedEndAt = new Date(programStartMs + recording.durationMs).toISOString()
  const timelineEndAt = new Date(programStartMs + timelineChaseSeconds * 1000).toISOString()
  // チャプター（CM とユーザー区間）。**ごみ箱では取らない** --- ごみ箱では
  // プレイヤーを出さず、配信経路も 404 になる（配信 3 クエリと同じ契約）。
  //
  // 編集 UI は「ブラウザ再生できる encoded があるときだけ」出す。原本 TS しか
  // 無い録画ではタイムラインを見ながら直せないので、押しても何もできない
  // コントロールを置かない（issue #209 と同じ規律）。**カット版を再生しているときは
  // 編集できない** --- カット版は時間軸から cut 区間を取り除いた別の動画で、
  // 原本の ms で置かれた境界をその動画に当てられない。
  //
  // 「今どの encoded を再生しているか」はプレイヤーが持つので、ここでは
  // 「確認に使える（cut でない）encoded が 1 つ以上あるか」で判定する。実際に
  // カット版へ切り替えたときの編集 UI の抑止はプレイヤー側が `playingCut` で行う。
  // ここでカット版しか無い録画に対してチャプターを取りに行かないのは、その
  // 構成では編集も確認再生もできないためである（cut だけの録画をそもそも
  // 凍結できないのは config 検証の仕事）。
  const canEditChapters = !trash && encodedAssets.some((a) => a.cut !== true)
  const chaptersQuery = useGetRecordingChapters(recording.id, {
    query: { enabled: canEditChapters },
  })
  const chapters = unwrap(chaptersQuery.data)
  const putChapters = usePutRecordingChapterEdits()
  const deleteChapters = useDeleteRecordingChapterEdits()
  const putWatchedMutation = usePutRecordingWatched()
  const deleteWatchedMutation = useDeleteRecordingWatched()
  const updateWatched = async (watched: boolean) => {
    try {
      if (watched) await putWatchedMutation.mutateAsync({ id: recording.id })
      else await deleteWatchedMutation.mutateAsync({ id: recording.id })
      toast({ message: watched ? '視聴済みにしました' : '未視聴に戻しました' })
      void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
    } catch (error) {
      toast({ message: apiErrorMessage(error) ?? '視聴状態の更新に失敗しました', kind: 'error' })
    }
  }
  const invalidateChapters = () => {
    void queryClient.invalidateQueries({ queryKey: getGetRecordingChaptersQueryKey(recording.id) })
    void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
  }
  const saveChapters = (spans: ChapterSpan[], version: string) =>
    putChapters
      .mutateAsync({ id: recording.id, data: { version, spans } })
      .then(
        () => {
          invalidateChapters()
          toast({ message: 'チャプターを保存しました' })
        },
        (error: unknown) => {
          // 409（版不一致）はサーバー側の層が変わった合図。再取得して、エディタに
          // 「サーバー側の内容が変わりました」を出させる。
          invalidateChapters()
          toast({ message: apiErrorMessage(error) ?? 'チャプターの保存に失敗しました', kind: 'error' })
          // エディタに失敗を伝える（成功時の「次の値を採用」の印を立てさせない）。
          throw error
        },
      )
  const resetChapters = () => {
    deleteChapters.mutate(
      { id: recording.id },
      {
        onSuccess: () => {
          invalidateChapters()
          toast({ message: '自動検出の結果に戻しました' })
        },
        onError: (error) =>
          toast({ message: apiErrorMessage(error) ?? '自動に戻せませんでした', kind: 'error' }),
      },
    )
  }
  // カット版の作り直し（`encodedAssets[].cutStale`）。**自動では起きない**ので、
  // ユーザーが押したときだけジョブを積む。新しい世代のパスに置き換わるので、
  // 一覧を invalidate して新しい rel_path / cutStale を取り直す。
  const reencode = useReencodeRecordingProfile()
  const reencodeCut = (profile: string) => {
    reencode.mutate(
      { id: recording.id, profile },
      {
        onSuccess: () => {
          invalidateChapters()
          toast({ message: 'カット版の作り直しを依頼しました' })
        },
        onError: (error) =>
          toast({ message: apiErrorMessage(error) ?? '作り直しを依頼できませんでした', kind: 'error' }),
      },
    )
  }

  // 追っかけの配信プロファイル（live.profiles）と、完了後のVODプロファイル
  // （encode.profiles）は別設定なので、URL用の profile を共有しない。再生位置だけ
  // VODの既定プロファイル名に寄せる。active asset が既にあればその実在する先頭を
  // 優先し、録画中でまだ無ければ凍結済み desired の先頭を使う。
  // **cut でない版を優先する。** 確認（チャプターの修正）はカット版ではできない
  // （境界は原本の ms で、カット版の軸には当てられない）ので、既定でカット版を
  // 開くと「再生できるのに編集できない」画面になる。cut のプロファイルだけの
  // 録画は凍結できない（config 検証）ので、ここで cut だけになることはない。
  const preferredPlaybackProfile =
    (encodedAssets.find((a) => a.cut !== true) ?? encodedAssets[0])?.profile ??
    recording.encodeProfiles?.[0]
  const showAddEncodePrompt =
    !trash &&
    recording.status === 'finished' &&
    hasOriginal &&
    encodedAssets.length === 0 &&
    (recording.encodeProfiles ?? []).length === 0
  const encodeProfilesQuery = useListEncodeProfiles({ query: { enabled: showAddEncodePrompt } })
  const configuredEncodeProfiles = unwrap(encodeProfilesQuery.data) ?? []
  const assetsRef = useRef<HTMLElement | null>(null)
  const rawCMRanges = recording.cmDetection.state === 'detected' ? recording.cmDetection.ranges ?? [] : []
  const showCMDetectorResults =
    recording.cmDetection.state === 'detected' &&
    rawCMRanges.length > 0 &&
    (!canEditChapters ||
      chaptersQuery.isError ||
      (!chaptersQuery.isPending &&
        chapters !== undefined &&
        (chapters.source === 'user' || chapters.spans.length === 0)))
  // 0 の要約は「異常なし」を書くことになるので節ごと出さない（docs/frontend/recordings.md）。
  const hasDrops =
    recording.dropSummary != null &&
    recording.dropSummary.drops + recording.dropSummary.errors + recording.dropSummary.scrambled > 0
  const dropSummaryItems =
    recording.dropSummary && hasDrops
      ? [
        { label: 'パケット', value: recording.dropSummary.packets },
        { label: 'ドロップ', value: recording.dropSummary.drops },
        { label: 'エラー', value: recording.dropSummary.errors },
        { label: 'スクランブル', value: recording.dropSummary.scrambled },
      ]
    : []
  // 詳細データの再取得ごとに取り込み状態を現在時刻で再評価する。mount 時に固定
  // すると、停滞表示が更新されなくなるため state 初期値には移せない。
  // oxlint-disable-next-line react/purity -- 再取得ごとの現在時刻スナップショットが必要
  const ingestState = ingestDisplay(recording, Date.now())
  const registeredSites = unwrap(useListSites().data) ?? []
  const showSite = shouldShowRecordingSite(registeredSites, [recording.site])

  return (
    <div data-testid="recording-detail-body" className="flex flex-col gap-4 text-sm">
      {/*
        ごみ箱の録画は配信 3 クエリ（GetOriginalMediaAssetForServing 等）が
        deleted_at IS NOT NULL を 404 にする（docs/api.md §メディア配信）。
        再生・サムネイル・原本リンクはどれも配信経路を叩くので、ごみ箱では
        そもそも出さない（M3-18）。復元してから見る。
        ListTrashRecordings が available_encoded_assets を射影しないままなのも
        この理由による（プレイヤーを出さないので揃える必要がない）。
      */}
      {!trash && (
        <section
          data-testid="recording-playback-group"
          className="flex flex-col gap-3 border-t border-border/60 pt-3"
        >
          <DetailHeading>再生</DetailHeading>
          {showChase && (
            <section className="flex flex-col gap-2" aria-label="追っかけ再生">
              <div className="flex items-center justify-between gap-2">
                <h4 className="font-medium">追っかけ再生</h4>
                <button
                  type="button"
                  onClick={() => setChasing(false)}
                  className="rounded border border-border px-2 py-1 text-xs text-muted-foreground hover:bg-muted"
                >
                  閉じる
                </button>
              </div>
              <div className="flex flex-col gap-1 rounded border border-border/60 px-3 py-2">
                <div className="flex items-center justify-between gap-3 text-muted-foreground">
                  <span>{formatTime(recording.startAt)}</span>
                  <span className="text-right" data-testid="chase-timeline-end">
                    {timelineExtended
                      ? `予定 ${formatTime(plannedEndAt)} / 録画中 ${formatTime(timelineEndAt)}`
                      : `${formatTime(plannedEndAt)} まで（予定）`}
                  </span>
                </div>
                <div
                  className="relative my-1 h-2 rounded bg-muted"
                  data-testid="chase-timeline-track"
                >
                  <div
                    className="absolute inset-y-0 left-0 rounded bg-primary/30"
                    data-testid="chase-timeline-recorded"
                    style={{ width: `${recordedProgressPercent}%` }}
                  />
                  {timelineExtended && (
                    <div
                      aria-hidden="true"
                      className="absolute -top-1 h-4 border-l-2 border-dashed border-foreground"
                      data-testid="chase-timeline-planned-end"
                      style={{ left: `${(plannedChaseSeconds / timelineChaseSeconds) * 100}%` }}
                      title={`予定 ${formatTime(plannedEndAt)}`}
                    />
                  )}
                  <input
                    id={`chase-offset-${recording.id}`}
                    type="range"
                    min={0}
                    max={timelineChaseSeconds}
                    step={1}
                    value={selectedOffset}
                    disabled={!Number.isFinite(recordingStartMs) || maxChaseOffsetSeconds === 0}
                    aria-label="追っかけ再生の位置"
                    aria-valuemin={0}
                    aria-valuenow={selectedOffset}
                    aria-valuemax={maxChaseOffsetSeconds}
                    aria-valuetext={formatChasePosition(recording.startAt, selectedOffset)}
                    className="chase-timeline-slider absolute inset-x-0 -top-5 h-11 w-full cursor-ew-resize bg-transparent focus-visible:outline-2 focus-visible:outline-ring focus-visible:outline-offset-2 disabled:cursor-not-allowed"
                    onChange={(event) => {
                      const requested = Number.parseInt(event.target.value, 10)
                      const next = Number.isFinite(requested)
                        ? Math.min(Math.max(0, requested), maxChaseOffsetSeconds)
                        : 0
                      selectedChaseOffsetRef.current = next
                      setSelectedChaseOffsetSeconds(next)
                    }}
                    onPointerUp={() => setChaseOffsetSeconds(selectedChaseOffsetRef.current)}
                    onKeyUp={(event) => {
                      if (
                        ['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End', 'PageUp', 'PageDown'].includes(
                          event.key,
                        )
                      ) {
                        setChaseOffsetSeconds(selectedChaseOffsetRef.current)
                      }
                    }}
                  />
                </div>
                <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
                  <output htmlFor={`chase-offset-${recording.id}`} className="font-medium">
                    {formatChasePosition(recording.startAt, selectedOffset)}
                  </output>
                  <span className="text-muted-foreground">
                    録画済み {formatChaseElapsed(availableChaseSeconds)}
                  </span>
                </div>
              </div>
              {/* 画質（issue #874）。**選択肢が 2 件以上のときだけ出す** ---
                  1 件しか無いのに出すと、選んでも何も変わらない「機能しない
                  コントロール」に戻る（issue #209 / `pages/live.tsx` と同じ規律）。
                  **切替はセッションを作り直さない** --- 追っかけのセッション鍵は
                  `(recordingID, offset)` でプロファイルを含まないので、同じセッションの
                  別プレイリストを取るだけである（`internal/streamer/live.go`。
                  `docs/api/media.md` §録画中の追っかけ再生）。`LivePlayer` は
                  key で作り直さない --- 作り直すと再生位置が先頭に戻る。
                  `value` は controlled なので、URL が未知の名前を運んでいても
                  既定の先頭に一致して表示される。 */}
              {liveProfiles.length > 1 && (
                <label className="flex items-center gap-2 text-xs text-muted-foreground">
                  <span>画質</span>
                  <select
                    aria-label="画質"
                    value={explicitLiveProfile ?? liveProfiles[0]?.name}
                    onChange={(e) => onSelectLiveProfile(e.target.value)}
                    className="h-8 rounded-md border border-border bg-background px-2 text-sm text-foreground outline-none"
                  >
                    {liveProfiles.map((p) => (
                      <option key={p.name} value={p.name}>
                        {liveProfileLabel(p)}
                      </option>
                    ))}
                  </select>
                </label>
              )}
              {!(liveProfile !== undefined && liveProfilesQuery.isPending) && (
                <LivePlayer
                  mode="chase"
                  site={recording.site}
                  recordingId={recording.id}
                  resumePositionMs={startAtBeginning ? undefined : recording.resumePositionMs}
                  startOffsetSeconds={startAtBeginning ? 0 : chaseOffsetSeconds}
                  profile={explicitLiveProfile}
                />
              )}
            </section>
          )}

          {!trash && recording.status === 'recording' && liveEnabled && !chasing && (
            <button
              type="button"
              onClick={() => setChasing(true)}
              className="self-start rounded border border-border px-3 py-1.5 text-sm text-primary hover:bg-muted"
            >
              追っかけ再生
            </button>
          )}

          {showOriginalVOD && (
            <section className="flex flex-col gap-2" aria-label="原本 TS をブラウザ再生">
              <div>
                <h4 className="font-medium">原本 TS をブラウザ再生</h4>
                <p className="text-muted-foreground">
                  原本 MPEG-2 を一時的に HLS へ変換します。再生用ファイルは保存しません。
                </p>
              </div>
              {liveProfilesQuery.isPending ? (
                <p role="status" className="text-muted-foreground">再生設定を読み込み中…</p>
              ) : liveProfiles.length === 0 ? (
                <p className="text-muted-foreground">
                  HLS 再生プロファイルを利用できません。原本は{' '}
                  <a
                    href={recordingFileURL(recording.id)}
                    className="inline-flex min-h-6 items-center text-primary underline-offset-2 hover:underline"
                  >
                    VLC 等で開く
                  </a>
                  ことができます。
                </p>
              ) : (
                <>
                  {liveProfiles.length > 1 && (
                    <label className="flex items-center gap-2 text-xs text-muted-foreground">
                      <span>画質</span>
                      <select
                        aria-label="画質"
                        value={explicitLiveProfile ?? liveProfiles[0]?.name}
                        onChange={(e) => onSelectLiveProfile(e.target.value)}
                        className="h-8 rounded-md border border-border bg-background px-2 text-sm text-foreground outline-none"
                      >
                        {liveProfiles.map((p) => (
                          <option key={p.name} value={p.name}>{liveProfileLabel(p)}</option>
                        ))}
                      </select>
                    </label>
                  )}
                  <LivePlayer
                    mode="original-vod"
                    site={recording.site}
                    recordingId={recording.id}
                    resumePositionMs={startAtBeginning ? undefined : recording.resumePositionMs}
                    profile={explicitLiveProfile}
                  />
                </>
              )}
            </section>
          )}

          {!trash && !showChase && (encodedAssets.length > 0 || (hasOriginal && !showOriginalVOD)) && (
            <RecordingPlayer
              recordingId={recording.id}
              resumePositionMs={startAtBeginning ? undefined : recording.resumePositionMs}
              onWatched={() => void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })}
              showWatched={recording.status === 'finished'}
              watched={recording.watchedAt !== undefined}
              watchedPending={putWatchedMutation.isPending || deleteWatchedMutation.isPending}
              putWatched={() => void updateWatched(true)}
              deleteWatched={() => void updateWatched(false)}
              preferredProfile={preferredPlaybackProfile}
              encodedAssets={encodedAssets}
              hasOriginal={hasOriginal}
              chapters={chapters?.spans}
              chapterSource={chapters?.source}
              chapterVersion={chapters?.version}
              chapterDetectionPending={chapters?.detectionPending}
              onSaveChapters={canEditChapters ? saveChapters : undefined}
              onResetChapters={canEditChapters ? resetChapters : undefined}
              chapterSavePending={putChapters.isPending || deleteChapters.isPending}
              onReencode={trash ? undefined : reencodeCut}
              reencodePending={reencode.isPending}
            />
          )}

          {/* 操作バーを持つプレイヤー（encoded あり）以外では、視聴済みの操作をここに残す。
              原本のみ・エンコード無し・資産無しでも完了録画の唯一の操作になるため。 */}
          {!trash && recording.status === 'finished' && !(!showChase && encodedAssets.length > 0) && (
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={putWatchedMutation.isPending || deleteWatchedMutation.isPending}
              onClick={() => void updateWatched(recording.watchedAt === undefined)}
            >
              {recording.watchedAt !== undefined ? '未視聴に戻す' : '視聴済みにする'}
            </Button>
          )}

          {showAddEncodePrompt && !encodeProfilesQuery.isPending && !encodeProfilesQuery.isError &&
            (configuredEncodeProfiles.length > 0 ? (
              <Button
                type="button"
                size="sm"
                onClick={() => {
                  assetsRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' })
                  assetsRef.current?.focus({ preventScroll: true })
                }}
              >
                エンコードを追加
              </Button>
            ) : (
              <p className="text-muted-foreground">エンコードプロファイルが設定されていません</p>
            ))}
        </section>
      )}

      {!trash && (recording.series != null || recording.ruleId !== undefined) && (
        <section
          data-testid="recording-continuation-group"
          aria-label="続き"
          className="flex flex-col gap-3 border-t border-border/60 pt-3"
        >
          <DetailHeading>続き</DetailHeading>
          {/* `series` が null の録画にはハブも「次回」も出さない。 */}
          {recording.series != null && <SeriesLinks recording={recording} />}
          {/* 手動予約由来の録画には ruleId が無い。 */}
          {recording.ruleId !== undefined && <RuleSection ruleId={recording.ruleId} />}
        </section>
      )}

      <section
        data-testid="recording-program-group"
        aria-label="番組"
        className="flex flex-col gap-3 border-t border-border/60 pt-3"
      >
        <DetailHeading>番組</DetailHeading>
        {recording.description && (
          <p data-testid="recording-description" className="whitespace-pre-wrap text-base text-muted-foreground">
            {recording.description}
          </p>
        )}
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
          <dt className="text-muted-foreground">物理チャンネル</dt>
          <dd>
            {recording.serviceName} ({recording.channelType}/{recording.channel})
            {showSite ? ` · ${recording.site}` : ''}
          </dd>
          <dt className="text-muted-foreground">番組開始</dt>
          <dd>{formatDateTime(recording.startAt)}</dd>
          <dt className="text-muted-foreground">番組終了</dt>
          <dd>{formatDateTime(new Date(Date.parse(recording.startAt) + recording.durationMs).toISOString())}</dd>
          {recording.startedAt && (
            <>
              <dt className="text-muted-foreground">実録画開始</dt>
              <dd>{formatDateTime(recording.startedAt)}</dd>
            </>
          )}
          {recording.endedAt && (
            <>
              <dt className="text-muted-foreground">実録画終了</dt>
              <dd>{formatDateTime(recording.endedAt)}</dd>
            </>
          )}
          <dt className="text-muted-foreground">種別</dt>
          <dd>{sourceLabels[recording.source]}</dd>
          {trash && recording.deletedAt && (
            <>
              <dt className="text-muted-foreground">削除日時</dt>
              <dd>{formatDateTime(recording.deletedAt)}</dd>
            </>
          )}
        </dl>
      </section>

      {!trash && (
        <section
          ref={assetsRef}
          tabIndex={-1}
          data-testid="recording-assets-group"
          aria-label="資産"
          className="flex scroll-mt-20 flex-col gap-3 border-t border-border/60 pt-3 focus-visible:outline-2 focus-visible:outline-ring focus-visible:outline-offset-2"
        >
          <DetailHeading>資産</DetailHeading>
          {hasOriginal && (
            <p className="text-muted-foreground">
              原本 TS:{' '}
              <a
                href={recordingFileURL(recording.id)}
                className="inline-flex min-h-6 items-center text-primary underline-offset-2 hover:underline"
              >
                ダウンロード / VLC{recording.sizeBytes !== undefined && ` (${formatBytes(recording.sizeBytes)})`}
              </a>
            </p>
          )}
          <RecordingAssetControls recording={recording} />

          <section className="flex flex-col gap-2" aria-label="CM 検出">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <DetailHeading>CM 検出</DetailHeading>
              {(cmDetectEnabled || recording.cmDetection.state !== 'disabled') && (
                <Button
                  type="button"
                  size="sm"
                  variant={recording.cmDetection.state === 'disabled' ? 'secondary' : 'outline'}
                  disabled={setEncodePolicy.isPending || (recording.cmDetection.state === 'disabled' && !hasOriginal)}
                  onClick={() => {
                    const enable = recording.cmDetection.state === 'disabled'
                    setEncodePolicy.mutate(
                      { id: recording.id, data: { cmDetect: enable } },
                      {
                        onSuccess: () => {
                          void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
                          toast({ message: enable ? 'CM 検出を有効にしました' : 'CM 検出を停止しました' })
                        },
                        onError: (error) =>
                          toast({
                            message:
                              apiErrorMessage(error) ??
                              (enable ? 'CM 検出の有効化に失敗しました' : 'CM 検出の停止に失敗しました'),
                            kind: 'error',
                          }),
                      },
                    )
                  }}
                >
                  {recording.cmDetection.state === 'disabled' ? '検出を有効化' : '検出を停止'}
                </Button>
              )}
            </div>
            <p className="text-muted-foreground">
              {recording.cmDetection.state === 'disabled' && '無効'}
              {recording.cmDetection.state === 'detecting' && '検出中、または再試行待ち'}
              {recording.cmDetection.state === 'detected' && '検出済み'}
              {recording.cmDetection.state === 'failed' && '3 回の試行に失敗しました'}
              {recording.cmDetection.state === 'disabled' && !hasOriginal &&
                '（原本の取り込み後に有効化できます）'}
            </p>
            {recording.cmDetection.state === 'failed' && (
              <p className="text-muted-foreground" data-testid="cm-detection-failure-message">
                {cmDetectStageMessage(recording.cmDetection.stage)}
              </p>
            )}
            {recording.cmDetection.state === 'detected' && rawCMRanges.length === 0 && (
              <p className="text-muted-foreground">検出器が CM 区間を検出しませんでした</p>
            )}
            {showCMDetectorResults && (
              <details data-testid="cm-detector-results-details" className="group text-muted-foreground">
                <DetailSummary>
                  <DetailHeading compact>検出器の結果</DetailHeading>
                </DetailSummary>
                <ul className="flex flex-col gap-1 py-1">
                  {rawCMRanges.map((range) => (
                    <li key={`${range.startMs}-${range.endMs}`}>
                      {formatCMOffset(range.startMs)} – {formatCMOffset(range.endMs)}
                    </li>
                  ))}
                </ul>
              </details>
            )}
            {recording.cmDetection.state === 'failed' && (() => {
              // logo / area は枠を教えるのが直し方なのでリンクを主導線にし、それ以外は再試行を主にする。
              const teachLogo = recording.cmDetection.stage === 'logo' || recording.cmDetection.stage === 'area'
              return (
              <div className="flex flex-wrap items-center gap-3">
                {teachLogo && (
                  <Link
                    to="/cm-logos"
                    search={{
                      network: recording.networkId,
                      service: recording.serviceId,
                      recording: recording.id,
                    }}
                    className="text-primary underline underline-offset-4"
                  >
                    CM 検出のロゴを教える
                  </Link>
                )}
                <Button
                  type="button"
                  size="sm"
                  variant={teachLogo ? 'secondary' : 'default'}
                  disabled={retryCMDetection.isPending || !hasOriginal || !cmDetectEnabled}
                  onClick={() => {
                    retryCMDetection.mutate(
                      { id: recording.id },
                      {
                        onSuccess: () => {
                          void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })
                          toast({ message: 'CM 検出を再試行します' })
                        },
                        onError: (error) =>
                          toast({
                            message: apiErrorMessage(error) ?? 'CM 検出の再試行に失敗しました',
                            kind: 'error',
                          }),
                      },
                    )
                  }}
                >
                  再試行
                </Button>
              </div>
              )
            })()}
            {recording.cmDetection.state === 'failed' && recording.cmDetection.error && (
              <details data-testid="cm-detection-technical-details" className="text-muted-foreground">
                <summary className="cursor-pointer">技術的な詳細</summary>
                <pre className="max-h-40 overflow-auto whitespace-pre-wrap font-mono text-xs">
                  {recording.cmDetection.error}
                </pre>
              </details>
            )}
          </section>
        </section>
      )}

      {(ingestState !== undefined ||
        (recording.qualityEvents?.length ?? 0) > 0 ||
        hasDrops) && (
        <div
          data-testid="recording-observations"
          className="flex flex-col gap-3 border-t border-border/60 pt-3"
        >
          {ingestState !== undefined && (
            <section>
              <DetailHeading>取り込み</DetailHeading>
              <p className="text-muted-foreground">{ingestDetailText(ingestState)}</p>
            </section>
          )}
          {recording.qualityEvents && recording.qualityEvents.length > 0 && (
            <section>
              <DetailHeading>品質イベント</DetailHeading>
              <ul className="flex flex-col gap-1 text-muted-foreground">
                {recording.qualityEvents.map((event, i) => (
                  <li key={i} className="break-all">
                    {String(event.event ?? 'unknown')}
                    {event.reason ? `: ${JSON.stringify(event.reason)}` : ''}
                  </li>
                ))}
              </ul>
            </section>
          )}
          {hasDrops && (
            <section>
              <DetailHeading>ドロップ集計</DetailHeading>
              <ul className="flex flex-wrap gap-x-4 gap-y-1 text-muted-foreground">
                {dropSummaryItems.map((item) => (
                  <li key={item.label}>{item.label} {item.value.toLocaleString()}</li>
                ))}
              </ul>
            </section>
          )}
          {/* PID 別の内訳は行数が多いので、初期状態では畳む。 */}
          {hasDrops && <DropStatsTable recordingId={recording.id} />}
        </div>
      )}

      <section
        data-testid="recording-actions-group"
        aria-label="操作"
        className="flex flex-col gap-3 border-t border-border/60 pt-3"
      >
        <DetailHeading>操作</DetailHeading>
        <RecordingActions recording={recording} trash={trash} />
      </section>
    </div>
  )
}

/**
 * seriesNextPageSize は「次のエピソード」を探すときに引く件数（API の既定と同じ）。
 *
 * 引き方は `?seriesOf=<id>&order=asc&from=<起点の startAt>` で、返るページは
 * **起点の時刻から始まる昇順の窓**である。起点より後の回はこのページの
 * 先頭側に入るので、通常は 1 ページで足りる。再生できない行が 49 件以上続くと
 * はみ出し、その場合は「次のエピソード」が出ない（`lib/series.ts` の `nextEpisode`）。
 */
const seriesNextPageSize = 50

/**
 * SeriesLinks は「このシリーズへ」（番組ハブ）と「次のエピソード」。
 *
 * `RecordingDetail` は単体ページだけが使うので、ここに置いても一覧へは漏れない。
 * 削除・追加エンコードなどの mutate は `recordingsQueryKeyPrefix` を invalidate
 * するので、このクエリも自動で巻き込まれる（`recordingDetailQueryKey` と同じ規律）。
 */
function SeriesLinks({ recording }: { recording: Recording }) {
  const query = useQuery({
    queryKey: [recordingsQueryKeyPrefix, 'series-next', recording.id] as const,
    queryFn: () =>
      listRecordings({
        seriesOf: recording.id,
        order: 'asc',
        from: recording.startAt,
        limit: seriesNextPageSize,
      }),
  })
  const next = useMemo(
    () => nextEpisode(unwrap(query.data) ?? [], recording),
    [query.data, recording],
  )

  return (
    <section className="flex flex-wrap items-center gap-x-4 gap-y-2" aria-label="シリーズ">
      <Link
        to="/recordings/$id/series"
        params={{ id: String(recording.id) }}
        className="inline-flex min-h-6 items-center text-primary underline-offset-2 hover:underline"
      >
        このシリーズへ
      </Link>
      {/* **再生できる行だけを「次」にする。** 開始時刻がずれて supersede されなかった
          failed 行を指すと、押した先の再生が 404 になる（`lib/series.ts`）。 */}
      {next !== undefined && (
        <Link
          to="/recordings/$id"
          params={{ id: String(next.id) }}
          className="inline-flex min-h-6 items-center text-muted-foreground underline-offset-2 hover:underline"
        >
          次のエピソード: {programTitle(next.title)}
        </Link>
      )}
    </section>
  )
}

/**
 * RuleSection は「この録画はどのルールが録ったのか」への導線（issue #230）。
 * 呼び出し側（RecordingDetail）が `recording.ruleId !== undefined` を確認して
 * からマウントするので、ここでは「ある」ことを前提にできる。
 *
 * **ルール名の解決は `useListRules` のキャッシュから引く（単体取得の
 * `GET /api/rules/{id}` / `useGetRule` はあるが使わない）。** `RulesPage` が
 * `useListRules()`（パラメータなし = 常に全件）で一覧を引く設計に既に乗って
 * いるので、録画ごとに個別の 1 件取得を増やす理由がない。`/rules` を
 * 経由していればキャッシュに乗っており、していなければここで引く（後者は
 * 下記の `#N` → ルール名の差し替えとして見える）。同じ `queryKey`
 * （`/api/rules`）の 1 本のクエリで、`ruleId` ごとの取得は発行しない。
 *
 * **`rules.find` が見つからない場合は `#N` 表記に落とす。** これは「ルールが
 * 削除された」ケースではない --- `recordings.rule_id` は `rules` への FK
 * `recordings_rule_id_fkey` が `ON DELETE SET NULL` なので、ルールを削除すると
 * `recordings.rule_id` が NULL になり `Recording.ruleId` 自体が省略され、この
 * セクションごと消える（`#N` へは落ちない）。`#N` に落ちるのは `rules.find`
 * が空を返す間、つまり一覧クエリが未解決 / 失敗（どちらも `query.data` が
 * `undefined`）か、返ってきた一覧にその id がまだ無い（新しく作られたルール等）
 * という一時的な状態。未解決の場合に `#N` → ルール名へ差し替わることは
 * `recording-detail.test.tsx`「ルール一覧が未解決の間は #N を出し、解決後に
 * ルール名へ差し替わる」で固定した。
 *
 * 原則「固有名詞はリンク」（issue #221）に従い、ルールの識別（名前 or
 * `#N`）そのものをリンクテキストにする --- 装飾テキストの隣にリンクを
 * 置く形にしない。名前が重複する場合だけ `#<id>` を名前に添える。リンク先は
 * `/search?ruleId=N`（ルールの実質的な編集画面。`RulesPage` のルール名リンクと
 * 同じ着地先）。
 */
function RuleSection({ ruleId }: { ruleId: number }) {
  const query = useListRules()
  const rules = unwrap(query.data) ?? []
  const rule = rules.find((r) => r.id === ruleId)
  const disambiguateRule = ruleDisambiguator(rules)
  const disambiguator = rule === undefined ? undefined : disambiguateRule(rule)
  const label =
    rule === undefined
      ? `#${ruleId}`
      : `${rule.name}${disambiguator === undefined ? '' : ` (${disambiguator})`}`

  return (
    <section>
      <h4 className="mb-1 font-medium">ルール</h4>
      <div className="flex flex-wrap items-center gap-3">
        <Link
          to="/search"
          search={{ ruleId }}
          className="text-primary underline-offset-2 hover:underline"
        >
          {label}
        </Link>
        <Link
          to="/recordings"
          search={{ ruleId }}
          className="text-muted-foreground underline-offset-2 hover:underline"
        >
          このルールの録画で絞る
        </Link>
      </div>
    </section>
  )
}
