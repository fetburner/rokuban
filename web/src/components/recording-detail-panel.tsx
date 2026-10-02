import { Link } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'

import { cn } from '@/lib/utils'

import {
  getGetRecordingChaptersQueryKey,
  listRecordings,
  useDeleteRecordingChapterEdits,
  useGetRecordingChapters,
  useListEncodeProfiles,
  useListLiveProfiles,
  useListRules,
  useListSites,
  usePutRecordingChapterEdits,
  usePutRecordingWatched,
  useDeleteRecordingWatched,
  useReencodeRecordingProfile,
  type ChapterSpan,
  type Recording,
} from '@/api/generated'
import { apiErrorMessage, unwrap } from '@/api/unwrap'
import { DropStatsTable } from '@/components/drop-stats-table'
import { RecordingAssetControls } from '@/components/recording-actions'
import { DropBadges, EncodeStatusBadges, IngestBadge, StatusBadge } from '@/components/recording-badges'
import { RecordingPlayer } from '@/components/recording-player'
import { LivePlayer } from '@/components/live-player'
import { ThumbnailProgressLine } from '@/components/thumbnail-overlay'
import { useToast } from '@/components/toaster'
import { Button } from '@/components/ui/button'
import {
  formatBytes,
  formatDate,
  formatDateTime,
  formatDateTimeSeconds,
  formatDuration,
  formatPlaybackTime,
  formatTime,
  formatTimelineTime,
} from '@/lib/format'
import { cmDetectStageMessage } from '@/lib/cm-detect-stage'
import { ingestDisplay, type IngestDisplay } from '@/lib/ingest'
import { useLiveEnabled } from '@/lib/capabilities'
import { recordingFileURL } from '@/lib/playback-position'
import { seedRecordingDetail } from '@/lib/recording-detail-cache'
import {
  selectRecordingPlaybackSource,
  transitionRecordingPlaybackSource,
  type RecordingPlaybackTransitionTrigger,
} from '@/lib/recording-playback-source'
import { validLiveProfile } from '@/lib/live'
import { ruleDisambiguator } from '@/lib/rule-label'
import { shouldShowRecordingSite, sourceLabels } from '@/lib/recording-search'
import { recordingsQueryKeyPrefix } from '@/lib/events'
import { programTitle } from '@/lib/program-labels'
import { nextEpisode } from '@/lib/series'
import { useMoveRecordingToTrash } from '@/lib/use-recording-trash'

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

function formatCMOffset(ms: number): string {
  const seconds = Math.floor(ms / 1000)
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remainder = seconds % 60
  return [hours, minutes, remainder].map((n) => String(n).padStart(2, '0')).join(':')
}

function formatRelativeTime(deltaMs: number): string {
  const seconds = Math.round(Math.abs(deltaMs) / 1000)
  const amount = seconds >= 3600
    ? `${Math.floor(seconds / 3600)}時間${Math.floor((seconds % 3600) / 60)}分`
    : seconds >= 60
      ? `${Math.floor(seconds / 60)}分${seconds % 60 > 0 ? `${seconds % 60}秒` : ''}`
      : `${seconds}秒`
  return `${amount}${deltaMs > 0 ? '遅れて' : '早く'}`
}

function cmDetectionLabel(state: Recording['cmDetection']['state']): string {
  switch (state) {
    case 'disabled': return '無効'
    case 'detecting': return '検出中、または再試行待ち'
    case 'detected': return '検出済み'
    case 'failed': return '検出失敗'
  }
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
  onNavigateToRecording,
}: {
  recording: Recording
  trash: boolean
  chase?: boolean
  /** ホームの「最初から」から来た場合は保存位置を復元しない。 */
  startAtBeginning?: boolean
  /** 追っかけ再生の画質（`?liveProfile=`。issue #874）。未検証の生の値。 */
  liveProfile?: string
  onSelectLiveProfile: (name: string) => void
  onNavigateToRecording: (id: number) => void
}) {
  const liveEnabled = useLiveEnabled()
  const queryClient = useQueryClient()
  const toast = useToast()
  const { moveToTrash } = useMoveRecordingToTrash(recording.id)
  const [selectedTab, setSelectedTab] = useState<DetailTab>(defaultDetailTab)
  const [descriptionExpanded, setDescriptionExpanded] = useState(false)
  const encodedAssets = recording.encodedAssets ?? []
  const hasOriginal = recording.sizeBytes !== undefined
  const playbackSelection = {
    status: recording.status,
    hasEncoded: encodedAssets.length > 0,
    hasOriginal,
    liveEnabled,
    isTrashed: trash,
  }
  const initialPlaybackState = () => {
    const source = selectRecordingPlaybackSource(playbackSelection)
    return {
      source,
      started: chase || source === 'encoded',
      startFromBeginning: startAtBeginning,
      positionSeconds: undefined as number | undefined,
      generation: 0,
    }
  }
  const [playbackState, setPlaybackState] = useState(initialPlaybackState)
  const playbackStateRef = useRef(playbackState)
  useLayoutEffect(() => {
    playbackStateRef.current = playbackState
  }, [playbackState])
  const recordingPositionSecondsRef = useRef<number | undefined>(undefined)
  useLayoutEffect(() => {
    recordingPositionSecondsRef.current = undefined
  }, [recording.id])
  const [chaseOffsetSeconds, setChaseOffsetSeconds] = useState<number | undefined>(undefined)
  const [selectedPlaybackProfile, setSelectedPlaybackProfile] = useState<string | undefined>(undefined)
  // 次のエピソードへ移るときページは作り直さず（全画面を保つため）、同じ部品に別の録画が来る。
  // 録画ごとの state（タブ・追っかけの位置・選んだ画質・説明の展開）はここで戻す。
  const [shownRecordingId, setShownRecordingId] = useState(recording.id)
  if (shownRecordingId !== recording.id) {
    setShownRecordingId(recording.id)
    setSelectedTab(defaultDetailTab())
    setDescriptionExpanded(false)
    setChaseOffsetSeconds(undefined)
    setSelectedPlaybackProfile(undefined)
    const nextState = initialPlaybackState()
    setPlaybackState(nextState)
  }
  const showChase = playbackState.source === 'chase'
  const showOriginalVOD = playbackState.source === 'original-vod'
  const showEncoded = playbackState.source === 'encoded'
  const showLiveSource = showChase || showOriginalVOD
  const updatePlaybackState = (next: typeof playbackState) => {
    playbackStateRef.current = next
    setPlaybackState(next)
  }
  const startPlayback = () => {
    updatePlaybackState({ ...playbackStateRef.current, started: true })
  }
  const startPlaybackFromBeginning = () => {
    updatePlaybackState({
      ...playbackStateRef.current,
      started: true,
      startFromBeginning: true,
      positionSeconds: undefined,
      generation: playbackStateRef.current.generation + 1,
    })
  }
  const reselectPlaybackSource = (
    trigger: RecordingPlaybackTransitionTrigger,
    positionSeconds = recordingPositionSecondsRef.current,
  ) => {
    const current = playbackStateRef.current
    if (trigger === 'source-range-exit' && current.source === 'chase' && playbackSelection.status === 'recording') {
      return false
    }
    const transition = transitionRecordingPlaybackSource({
      currentSource: current.source,
      currentPositionSeconds: positionSeconds,
      trigger,
      recording: playbackSelection,
    })
    if (transition.kind === 'keep-current') return false

    const sourceChanged = transition.source !== current.source
    const restartSession = trigger === 'source-range-exit'
    if (!sourceChanged && !restartSession) return false
    updatePlaybackState({
      source: transition.source,
      started: transition.source !== 'none',
      startFromBeginning: false,
      positionSeconds: transition.positionSeconds,
      generation: current.generation + 1,
    })
    return true
  }
  const resumePositionMs = playbackState.positionSeconds !== undefined
    ? Math.max(0, Math.round(playbackState.positionSeconds * 1000))
    : playbackState.startFromBeginning
      ? undefined
      : recording.resumePositionMs
  const startOffsetSeconds = showChase
    ? playbackState.startFromBeginning
      ? 0
      : playbackState.positionSeconds === undefined
        ? chaseOffsetSeconds
        : undefined
    : playbackState.positionSeconds !== undefined
      ? Math.floor(Math.max(0, playbackState.positionSeconds))
      : undefined
  // 追っかけか原本 VOD を表示するときだけ live プロファイルを取る。一覧は
  // セレクタ用で、取得できなくても先頭プロファイルで再生できる既存契約を保つ。
  const liveProfilesQuery = useListLiveProfiles({ query: { enabled: showChase || showOriginalVOD } })
  const liveProfiles = useMemo(
    () => unwrap(liveProfilesQuery.data) ?? [],
    [liveProfilesQuery.data],
  )
  const showOriginalVODPlayer =
    showOriginalVOD && !liveProfilesQuery.isPending && liveProfiles.length > 0
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
    if (!showChase || recording.status !== 'recording') return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [showChase, recording.status])
  const recordingStartMs = recording.startedAt === undefined ? Number.NaN : Date.parse(recording.startedAt)
  const recordingEndMs = recording.endedAt === undefined ? Number.NaN : Date.parse(recording.endedAt)
  const availableChaseSeconds = Number.isFinite(recordingStartMs)
    ? recording.status === 'recording'
      ? Math.max(0, Math.floor((now - recordingStartMs) / 1000))
      : Number.isFinite(recordingEndMs)
        ? Math.max(0, Math.floor((recordingEndMs - recordingStartMs) / 1000))
        : 0
    : 0
  const programStartMs = Date.parse(recording.startAt)
  const plannedChaseSeconds = Math.max(0, Math.ceil(recording.durationMs / 1000))
  const chaseProgrammeHeadSeconds = Number.isFinite(programStartMs) && Number.isFinite(recordingStartMs)
    ? (recordingStartMs - programStartMs) / 1000
    : 0
  const previewTimelineMinSeconds = Math.min(0, chaseProgrammeHeadSeconds)
  const previewTimelineMaxSeconds = Math.max(
    previewTimelineMinSeconds + 1,
    plannedChaseSeconds,
    chaseProgrammeHeadSeconds + availableChaseSeconds,
  )
  const previewTimelineRange = previewTimelineMaxSeconds - previewTimelineMinSeconds
  const previewRecordingStartFraction = Math.max(0, Math.min(1,
    (chaseProgrammeHeadSeconds - previewTimelineMinSeconds) / previewTimelineRange,
  ))
  const previewRecordedFraction = Math.max(0, Math.min(1,
    (chaseProgrammeHeadSeconds + availableChaseSeconds - previewTimelineMinSeconds) / previewTimelineRange,
  ))
  const previewRecordedWidthFraction = Math.max(0, previewRecordedFraction - previewRecordingStartFraction)
  const previewPlannedFraction = Math.max(0, Math.min(1,
    (plannedChaseSeconds - previewTimelineMinSeconds) / previewTimelineRange,
  ))
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
  // 原本 HLS は編集 UI を出さないが、再生バーの目盛り・一覧・自動スキップでは
  // 同じ区間を使う。原本 VOD プレイヤーがある場合は再生用に取得する。
  // ここでカット版しか無い録画に対してチャプターを取りに行かないのは、その
  // 構成では編集も確認再生もできないためである（cut だけの録画をそもそも
  // 凍結できないのは config 検証の仕事）。
  const canEditChapters = !trash && encodedAssets.some((a) => a.cut !== true)
  const chaptersQuery = useGetRecordingChapters(recording.id, {
    query: { enabled: canEditChapters || showOriginalVODPlayer },
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
  const activePlaybackProfile =
    selectedPlaybackProfile !== undefined && encodedAssets.some((asset) => asset.profile === selectedPlaybackProfile)
      ? selectedPlaybackProfile
      : preferredPlaybackProfile
  const showAddEncodePrompt =
    !trash &&
    recording.status === 'finished' &&
    hasOriginal &&
    encodedAssets.length === 0 &&
    (recording.encodeProfiles ?? []).length === 0
  const encodeProfilesQuery = useListEncodeProfiles({ query: { enabled: showAddEncodePrompt } })
  const configuredEncodeProfiles = unwrap(encodeProfilesQuery.data) ?? []
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
  // 詳細データの再取得ごとに取り込み状態を現在時刻で再評価する。mount 時に固定
  // すると、停滞表示が更新されなくなるため state 初期値には移せない。
  // oxlint-disable-next-line react/purity -- 再取得ごとの現在時刻スナップショットが必要
  const ingestState = ingestDisplay(recording, Date.now())
  const registeredSites = unwrap(useListSites().data) ?? []
  const showSite = shouldShowRecordingSite(registeredSites, [recording.site])
  const seriesQuery = useQuery({
    queryKey: [recordingsQueryKeyPrefix, 'series-next', recording.id] as const,
    enabled: !trash && recording.series != null,
    queryFn: () =>
      listRecordings({
        seriesOf: recording.id,
        order: 'asc',
        from: recording.startAt,
        limit: seriesNextPageSize,
      }),
  })
  const seriesRecordings = useMemo(() => unwrap(seriesQuery.data) ?? [], [seriesQuery.data])
  const next = useMemo(() => nextEpisode(seriesRecordings, recording), [seriesRecordings, recording])
  // 棚は過去の回を含めて新しい順に並べるので、「次のエピソード」用の窓（いまの回以降）とは別に引く。
  // 使い回すと過去の回が出ず、件数も誤る。
  const shelfQuery = useQuery({
    queryKey: [recordingsQueryKeyPrefix, 'series-shelf', recording.id] as const,
    enabled: !trash && recording.series != null,
    queryFn: () => listRecordings({ seriesOf: recording.id, order: 'desc', limit: seriesShelfPageSize }),
  })
  const shelfRows = unwrap(shelfQuery.data)
  const shelfTruncated = (shelfRows?.length ?? 0) >= seriesShelfPageSize
  const shelfRecordings = useMemo(() => {
    if (recording.series == null) return []
    const byId = new Map((shelfRows ?? []).map((item) => [item.id, item]))
    // いまの回は開いている最新の状態（再取得で変わる視聴状態など）を使う。
    byId.set(recording.id, recording)
    return [...byId.values()]
      .filter((item) => item.series === recording.series && item.deletedAt === undefined)
      .sort((a, b) => Date.parse(b.startAt) - Date.parse(a.startAt) || b.id - a.id)
  }, [recording, shelfRows])
  const shelfTotalBytes = shelfRecordings.reduce(
    (sum, item) =>
      sum +
      (item.sizeBytes ?? 0) +
      (item.encodedAssets ?? []).reduce((assetSum, asset) => assetSum + (asset.sizeBytes ?? 0), 0),
    0,
  )
  // 移動先の詳細を先にキャッシュへ入れてから移る（全画面を保つ。`seedRecordingDetail`）。
  const openRecording = (id: number) => {
    const target = [...seriesRecordings, ...(shelfRows ?? [])].find((item) => item.id === id)
    if (target !== undefined) seedRecordingDetail(queryClient, target)
    onNavigateToRecording(id)
  }
  // 操作バー（encoded のプレイヤー）が無い状態では、次のエピソードへの導線をシリーズの行に出す。
  const hasPlayerBar = !trash && showEncoded
  // 棚（lg 以上だけで見える）を描く条件。シリーズの導線と表示を current layout に残す。
  const hasShelf = !trash && recording.series != null

  const hasVersions = !trash && (
    encodedAssets.length > 0 || hasOriginal || recording.status === 'recording' ||
    (recording.encodeProfiles?.length ?? 0) > 0
  )
  const hasRecord = !trash
  const tabs: Array<{ id: 'programme' | 'versions' | 'record'; label: string }> = [
    { id: 'programme', label: '番組' },
    ...(hasVersions ? [{ id: 'versions' as const, label: '版' }] : []),
    ...(hasRecord ? [{ id: 'record' as const, label: '記録' }] : []),
  ]
  const activeTab = tabs.some((tab) => tab.id === selectedTab) ? selectedTab : 'programme'
  const programEndAt = new Date(Date.parse(recording.startAt) + recording.durationMs).toISOString()
  const actualTimeLabels = [
    recording.startedAt && Date.parse(recording.startedAt) !== Date.parse(recording.startAt)
      ? `${formatRelativeTime(Date.parse(recording.startedAt) - Date.parse(recording.startAt))}開始`
      : undefined,
    recording.endedAt && Date.parse(recording.endedAt) !== Date.parse(programEndAt)
      ? `${formatRelativeTime(Date.parse(recording.endedAt) - Date.parse(programEndAt))}終了`
      : undefined,
  ].filter((label): label is string => label !== undefined)
  const recordedStartMs = recording.startedAt ? Date.parse(recording.startedAt) : Number.NaN
  const recordedEndMs = recording.endedAt ? Date.parse(recording.endedAt) : Number.NaN
  const recordedDurationMs = recordedEndMs - recordedStartMs
  const outsideProgramSegments =
    Number.isFinite(recordedStartMs) && Number.isFinite(recordedEndMs) && recordedDurationMs > 0
      ? {
          beforeEndPercent: Math.min(100, Math.max(0, ((programStartMs - recordedStartMs) / recordedDurationMs) * 100)),
          afterStartPercent: Math.min(100, Math.max(0, ((Date.parse(programEndAt) - recordedStartMs) / recordedDurationMs) * 100)),
        }
      : undefined

  return (
    <div
      data-testid="recording-detail-body"
      className={cn(
        'w-full text-sm',
        !trash && recording.series != null && 'grid grid-cols-1 gap-8 lg:grid-cols-[minmax(0,1fr)_18rem]',
      )}
    >
      {!trash && (
        <section
          data-testid="recording-playback-group"
          className="col-span-full flex flex-col gap-3"
        >
          {showLiveSource && !playbackState.started && (
            <div
              data-testid="recording-playback-poster"
              className="relative w-full overflow-hidden rounded bg-neutral-950 text-white"
            >
              <div className="group relative aspect-video">
                <img
                  src={`/api/media/recordings/${recording.id}/thumbnail`}
                  alt=""
                  className="absolute inset-0 size-full object-cover opacity-60 transition-opacity group-hover:opacity-75"
                />
                <div className="absolute inset-0 bg-gradient-to-t from-black/75 via-black/20 to-black/50" />
                <button
                  type="button"
                  data-testid="recording-playback-start"
                  aria-label={showChase
                    ? `続きから再生（${formatPlaybackTime((recording.resumePositionMs ?? 0) / 1000)}）`
                    : '再生'}
                  onClick={startPlayback}
                  className="absolute inset-x-0 top-0 bottom-16 flex flex-col items-center justify-center gap-3 text-center"
                >
                  <span className="grid size-16 place-items-center rounded-full bg-white/90 text-3xl text-black shadow-lg">
                    ▶
                  </span>
                  {showChase ? (
                    <>
                      <span className="text-lg font-semibold">
                        続きから（{formatPlaybackTime((recording.resumePositionMs ?? 0) / 1000)}）
                      </span>
                      <span className="text-sm text-white/80">
                        録画済み {formatDuration(availableChaseSeconds * 1000)} · 押すと追っかけ再生を始めます
                      </span>
                    </>
                  ) : (
                    <span className="text-sm font-medium">再生</span>
                  )}
                </button>
                {showChase && (
                  <button
                    type="button"
                    data-testid="recording-playback-start-from-beginning"
                    onClick={startPlaybackFromBeginning}
                    className="absolute inset-x-0 bottom-12 z-10 mx-auto min-h-8 w-fit px-3 text-sm underline underline-offset-2"
                  >
                    先頭から見る
                  </button>
                )}
              </div>
              {showChase && (
                <div
                  data-testid="recording-playback-preview-timeline"
                  role="img"
                  aria-label={`録画時間: 0:00 から ${formatTimelineTime(plannedChaseSeconds)} まで（予定）、録画済み ${formatTimelineTime(availableChaseSeconds)}`}
                  className="bg-gradient-to-t from-black/95 via-black/70 to-transparent px-3 pb-2 pt-3"
                >
                  <div className="relative h-6" aria-hidden="true">
                    <div className="absolute inset-x-0 top-1/2 h-1 -translate-y-1/2 overflow-hidden rounded-full bg-white/30">
                      <div
                        data-testid="recording-playback-preview-recorded"
                        className="absolute inset-y-0 bg-white/55"
                        style={{ left: `${previewRecordingStartFraction * 100}%`, width: `${previewRecordedWidthFraction * 100}%` }}
                      />
                      <div
                        className="absolute inset-y-0"
                        style={{
                          left: `${previewRecordedFraction * 100}%`,
                          right: `${(1 - previewRecordedFraction) * 100}%`,
                          backgroundImage: 'repeating-linear-gradient(90deg, transparent 0 3px, rgb(255 255 255 / 45%) 3px 5px)',
                        }}
                      />
                    </div>
                    <div
                      aria-hidden="true"
                      className="absolute top-1 z-[2] h-4 border-l-2 border-dashed border-white"
                      style={{ left: `${previewPlannedFraction * 100}%` }}
                    />
                    <div
                      data-testid="recording-playback-preview-live-edge"
                      className="absolute top-1/2 z-[3] h-5 w-0.5 -translate-x-1/2 -translate-y-1/2 bg-tally"
                      style={{ left: `${previewRecordedFraction * 100}%` }}
                    />
                  </div>
                  <div className="flex justify-between text-[10px] text-white/75">
                    <span>{formatTimelineTime(previewTimelineMinSeconds)}</span>
                    <span>
                      {chaseProgrammeHeadSeconds + availableChaseSeconds > plannedChaseSeconds
                        ? `予定 ${formatTimelineTime(plannedChaseSeconds)} ┆ 延長中 · 先端 ${formatTimelineTime(chaseProgrammeHeadSeconds + availableChaseSeconds)}`
                        : `${formatTimelineTime(plannedChaseSeconds)} まで（予定）`}
                    </span>
                  </div>
                </div>
              )}
            </div>
          )}

          {showChase && playbackState.started && liveProfile !== undefined && liveProfilesQuery.isPending && (
            <p role="status" className="text-muted-foreground">再生設定を読み込み中…</p>
          )}
          {showChase && playbackState.started && !(liveProfile !== undefined && liveProfilesQuery.isPending) && (
            <LivePlayer
              key={`${playbackState.source}:${playbackState.generation}`}
              mode="chase"
              site={recording.site}
              recordingId={recording.id}
              resumePositionMs={resumePositionMs}
              startOffsetSeconds={startOffsetSeconds}
              profile={explicitLiveProfile}
              availableProfiles={liveProfiles}
              onProfileChange={onSelectLiveProfile}
              allowChaseOffsetRestart={recording.status === 'recording'}
              chaseTimeline={{
                programmeStartMs: programStartMs,
                recordingStartedAtMs: recordingStartMs,
                plannedSeconds: plannedChaseSeconds,
                recordedSeconds: availableChaseSeconds,
              }}
              onChaseOffsetChange={setChaseOffsetSeconds}
              onRecordingPositionChange={(seconds) => { recordingPositionSecondsRef.current = seconds }}
              onSourceRangeExit={(seconds) => reselectPlaybackSource('source-range-exit', seconds)}
              onRecordingPlaybackEnded={(seconds) => reselectPlaybackSource('ended', seconds)}
              onRecordingPlaybackError={(seconds) => reselectPlaybackSource('source-error', seconds)}
            />
          )}

          {showOriginalVOD && playbackState.started && liveProfilesQuery.isPending && (
            <p role="status" className="text-muted-foreground">再生設定を読み込み中…</p>
          )}
          {showOriginalVOD && playbackState.started && !liveProfilesQuery.isPending && liveProfiles.length === 0 && (
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
          )}
          {showOriginalVOD && playbackState.started && !liveProfilesQuery.isPending && liveProfiles.length > 0 && (
            <LivePlayer
              key={`${playbackState.source}:${playbackState.generation}`}
              mode="original-vod"
              site={recording.site}
              recordingId={recording.id}
              chapters={chapters?.spans}
              watched={recording.watchedAt !== undefined}
              watchedPending={putWatchedMutation.isPending || deleteWatchedMutation.isPending}
              onPutWatched={() => void updateWatched(true)}
              onDeleteWatched={() => void updateWatched(false)}
              onWatched={() => void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })}
              resumePositionMs={resumePositionMs}
              startOffsetSeconds={startOffsetSeconds}
              startPositionSeconds={playbackState.positionSeconds !== undefined
                ? Math.max(0, playbackState.positionSeconds - (startOffsetSeconds ?? 0))
                : undefined}
              recordingDurationMs={recording.startedAt !== undefined && recording.endedAt !== undefined
                ? Date.parse(recording.endedAt) - Date.parse(recording.startedAt)
                : undefined}
              profile={explicitLiveProfile}
              availableProfiles={liveProfiles}
              onProfileChange={onSelectLiveProfile}
              onRecordingPositionChange={(seconds) => { recordingPositionSecondsRef.current = seconds }}
              onSourceRangeExit={(seconds) => reselectPlaybackSource('source-range-exit', seconds)}
              onRecordingPlaybackEnded={(seconds) => reselectPlaybackSource('ended', seconds)}
              onRecordingPlaybackError={(seconds) => reselectPlaybackSource('source-error', seconds)}
            />
          )}

          {showEncoded && (
            <RecordingPlayer
              key={`${playbackState.source}:${playbackState.generation}`}
              recordingId={recording.id}
              resumePositionMs={resumePositionMs}
              onWatched={() => void queryClient.invalidateQueries({ queryKey: [recordingsQueryKeyPrefix] })}
              showWatched={recording.status === 'finished'}
              watched={recording.watchedAt !== undefined}
              watchedPending={putWatchedMutation.isPending || deleteWatchedMutation.isPending}
              putWatched={() => void updateWatched(true)}
              deleteWatched={() => void updateWatched(false)}
              preferredProfile={preferredPlaybackProfile}
              nextEpisode={next ? { id: next.id, title: next.title, startAt: next.startAt } : undefined}
              onNextEpisodeNavigate={next ? () => seedRecordingDetail(queryClient, next) : undefined}
              outsideProgramSegments={outsideProgramSegments}
              onNavigateToRecording={openRecording}
              onTrash={moveToTrash}
              onProfileChange={setSelectedPlaybackProfile}
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
              onRecordingPositionChange={(seconds) => { recordingPositionSecondsRef.current = seconds }}
              onRecordingPlaybackEnded={(seconds) => reselectPlaybackSource('ended', seconds)}
              onRecordingPlaybackError={(seconds) => reselectPlaybackSource('source-error', seconds)}
            />
          )}

          {playbackState.source === 'none' && (
            <div className="text-muted-foreground">
              {hasOriginal ? (
                <p>
                  ブラウザ再生用のエンコードがまだありません。原本は{' '}
                  <a
                    href={recordingFileURL(recording.id)}
                    className="inline-flex min-h-6 items-center text-primary underline-offset-2 hover:underline"
                  >
                    VLC 等で開く
                  </a>
                  ことができます。
                </p>
              ) : (
                <p>再生可能なファイルがありません。</p>
              )}
            </div>
          )}

          {/* 操作バーを持つプレイヤー以外では、視聴済みの操作をここに残す。
              再生できない原本のみ・資産なしでも完了録画の操作口になる。 */}
          {!trash && recording.status === 'finished' && !showEncoded &&
            !(showOriginalVODPlayer && playbackState.started) && (
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


        </section>
      )}

      <div data-testid="recording-player-column" className="min-w-0 flex flex-col gap-4">
        <section data-testid="recording-title-row" className="flex flex-col gap-2">
          <h2 className="text-xl font-semibold leading-tight">{programTitle(recording.title)}</h2>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
            <button type="button" className="inline-flex min-h-6 items-center" aria-label="録画状態を記録タブで見る" onClick={() => setSelectedTab('record')}>
              <StatusBadge status={recording.status} />
            </button>
            {recording.ingest && (
              <button type="button" className="inline-flex min-h-6 items-center" aria-label="取り込み状態を記録タブで見る" onClick={() => setSelectedTab('record')}>
                <IngestBadge recording={recording} />
              </button>
            )}
            {(recording.encodeStatus?.length ?? 0) > 0 && (
              <button type="button" className="inline-flex min-h-6 items-center" aria-label="エンコード状態を記録タブで見る" onClick={() => setSelectedTab('record')}>
                <EncodeStatusBadges recording={recording} />
              </button>
            )}
            {hasDrops && recording.dropSummary && (
              <button type="button" className="inline-flex min-h-6 items-center" aria-label="ドロップ状態を記録タブで見る" onClick={() => setSelectedTab('record')}>
                <DropBadges summary={recording.dropSummary} />
              </button>
            )}
            {showSite && <span className="rounded bg-muted px-1.5 py-0.5 text-foreground">{recording.site}</span>}
            <span>{recording.serviceName}</span>
            <span>
              {formatDate(recording.startAt)} {formatTime(recording.startAt)}–{formatTime(programEndAt)}
            </span>
            <span>{formatDuration(recording.durationMs)}</span>
            {actualTimeLabels.length > 0 && (
              <span data-testid="recording-actual-time-difference" className="text-foreground">
                {actualTimeLabels.join('・')}
              </span>
            )}
            {trash && recording.deletedAt && (
              <span>ごみ箱（削除 {formatDateTime(recording.deletedAt)}）</span>
            )}
          </div>
          {recording.series != null && (
            <div
              data-testid="recording-series-links"
              className={cn(
                'flex flex-wrap items-center gap-x-4 gap-y-1 text-sm',
                // 棚がある幅では、行の中身がシリーズのリンクだけなら行ごと出さない（空の余白を残さない）。
                hasShelf && !(next !== undefined && !hasPlayerBar) && 'lg:hidden',
              )}
            >
              {/* シリーズの導線はどの状態でも、どちらかの幅で 1 つ見える。棚がある幅（lg 以上）では
                  棚の見出しが受け持つので、ここは棚が無い幅とごみ箱（棚が無い）だけで出す。
                  回のタイトルとシリーズ名が同じ録画でタイトルの繰り返しに見えないよう、
                  「シリーズ」の見出し語と下線を付ける。 */}
              <span className={cn('inline-flex items-center gap-1.5', hasShelf && 'lg:hidden')}>
                <span className="text-muted-foreground">シリーズ</span>
                <Link
                  to="/recordings/$id/series"
                  params={{ id: String(recording.id) }}
                  aria-label={`このシリーズへ: ${recording.series}`}
                  className="inline-flex min-h-6 items-center text-primary underline underline-offset-4"
                >
                  {recording.series} <span aria-hidden className="ml-1">›</span>
                </Link>
              </span>
              {/* **再生できる行だけを「次」にする。** 開始時刻がずれて supersede されなかった
                  failed 行を指すと、押した先の再生が 404 になる（`lib/series.ts`）。 */}
              {next !== undefined && !hasPlayerBar && (
                <Link
                  to="/recordings/$id"
                  params={{ id: String(next.id) }}
                  className="inline-flex min-h-6 items-center text-muted-foreground underline-offset-2 hover:underline"
                  onClick={() => seedRecordingDetail(queryClient, next)}
                >
                  次のエピソード: {programTitle(next.title)}
                </Link>
              )}
            </div>
          )}
          {recording.description && (
            // 説明はここだけに置く（番組タブには繰り返さない）。2 行で切り、押すと全文を開く。
            <button
              type="button"
              data-testid="recording-description"
              aria-expanded={descriptionExpanded}
              title={descriptionExpanded ? '説明を 2 行に戻す' : '説明の全文を開く'}
              className={cn(
                'text-left text-base whitespace-pre-wrap text-muted-foreground',
                !descriptionExpanded && 'line-clamp-2',
              )}
              onClick={() => setDescriptionExpanded((open) => !open)}
            >
              {recording.description}
            </button>
          )}
        </section>

        <section data-testid="recording-detail-tabs" className="min-w-0">
          <div role="tablist" aria-label="録画詳細" className="flex min-h-10 items-center gap-6 border-b border-border">
            {tabs.map((tab, index) => (
              <button
                key={tab.id}
                id={`recording-tab-${tab.id}`}
                type="button"
                role="tab"
                aria-selected={activeTab === tab.id}
                aria-controls="recording-detail-tab-panel"
                tabIndex={activeTab === tab.id ? 0 : -1}
                className={`min-h-10 min-w-6 border-b-2 px-1.5 text-sm ${activeTab === tab.id ? 'border-foreground font-medium text-foreground' : 'border-transparent text-muted-foreground hover:text-foreground'}`}
                onClick={() => setSelectedTab(tab.id)}
                onKeyDown={(event) => {
                  if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
                  event.preventDefault()
                  const direction = event.key === 'ArrowRight' ? 1 : -1
                  const nextIndex = (index + direction + tabs.length) % tabs.length
                  const nextTab = tabs[nextIndex]
                  setSelectedTab(nextTab.id)
                  document.getElementById(`recording-tab-${nextTab.id}`)?.focus()
                }}
              >
                {tab.label}
              </button>
            ))}
          </div>

          <div
            id="recording-detail-tab-panel"
            role="tabpanel"
            aria-labelledby={`recording-tab-${activeTab}`}
            data-testid="recording-detail-tab-panel"
            tabIndex={0}
            className="min-w-0 pt-4 focus-visible:outline-2 focus-visible:outline-ring"
          >
            {activeTab === 'programme' && (
              <section data-testid="recording-program-group" aria-label="番組" className="flex flex-col gap-4">
                <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-2 text-sm">
                  <dt className="text-muted-foreground">チャンネル</dt>
                  <dd>{recording.serviceName}（{recording.channelType} {recording.channel}）{showSite ? ` · ${recording.site}` : ''}</dd>
                  {recording.startedAt && Date.parse(recording.startedAt) !== Date.parse(recording.startAt) && (
                    <>
                      <dt className="text-muted-foreground">実録画開始</dt>
                      <dd>{formatDateTimeSeconds(recording.startedAt)}</dd>
                    </>
                  )}
                  {recording.endedAt && Date.parse(recording.endedAt) !== Date.parse(programEndAt) && (
                    <>
                      <dt className="text-muted-foreground">実録画終了</dt>
                      <dd>{formatDateTimeSeconds(recording.endedAt)}</dd>
                    </>
                  )}
                  {trash && recording.deletedAt && (
                    <>
                      <dt className="text-muted-foreground">削除日時</dt>
                      <dd>{formatDateTime(recording.deletedAt)}</dd>
                    </>
                  )}
                  <dt className="text-muted-foreground">録画のしかた</dt>
                  <dd>{recording.ruleId !== undefined ? <RuleSection ruleId={recording.ruleId} /> : recording.source === 'manual' ? '手動予約' : sourceLabels[recording.source]}</dd>
                </dl>
              </section>
            )}

            {activeTab === 'versions' && !trash && (
              <section data-testid="recording-assets-group" aria-label="版" className="flex flex-col gap-3">
                {(encodedAssets.length > 0 || hasOriginal || recording.status === 'recording') && (
                  <div role="list" aria-label="録画の版" className="divide-y divide-border rounded-md border border-border px-3">
                    {encodedAssets.map((asset) => (
                      <div
                        key={asset.profile}
                        role="listitem"
                        data-testid="recording-version-row"
                        className="flex min-h-14 flex-wrap items-center gap-x-3 gap-y-1 py-2 text-sm"
                      >
                        <span className="font-medium">{asset.cut ? `カット版 (${asset.profile})` : asset.profile}</span>
                        {asset.profile === activePlaybackProfile && <span className="rounded bg-foreground px-1.5 py-0.5 text-xs text-background">再生中</span>}
                        <span className="ml-auto text-muted-foreground">{asset.sizeBytes === undefined ? 'サイズ不明' : formatBytes(asset.sizeBytes)}</span>
                        <a href={recordingFileURL(recording.id, asset.profile)} download className="inline-flex min-h-6 items-center text-primary underline-offset-2 hover:underline">ダウンロード</a>
                      </div>
                    ))}
                    {hasOriginal ? (
                      <div role="listitem" data-testid="recording-original-row" className="flex min-h-14 flex-wrap items-center gap-3 py-2 text-sm">
                        <span className="font-medium">原本 TS</span>
                        <span className="ml-auto text-muted-foreground">{formatBytes(recording.sizeBytes!)}</span>
                        <a href={recordingFileURL(recording.id)} className="inline-flex min-h-6 items-center text-primary underline-offset-2 hover:underline">ダウンロード / VLC</a>
                      </div>
                    ) : recording.status === 'recording' ? (
                      <div role="listitem" data-testid="recording-original-row" className="flex min-h-14 flex-wrap items-center gap-3 py-2 text-sm">
                        <span className="font-medium">原本 TS</span>
                        <span className="ml-auto text-muted-foreground">{ingestState ? ingestDetailText(ingestState) : '取り込み中'}</span>
                      </div>
                    ) : null}
                  </div>
                )}
                {showAddEncodePrompt && !encodeProfilesQuery.isPending && !encodeProfilesQuery.isError &&
                  configuredEncodeProfiles.length === 0 && (
                    <p className="text-muted-foreground">エンコードプロファイルが設定されていません</p>
                  )}
                <RecordingAssetControls recording={recording} />
              </section>
            )}

            {activeTab === 'record' && !trash && (
              <section data-testid="recording-observations" aria-label="記録" className="flex flex-col gap-3 text-sm">
                {ingestState !== undefined && (
                  <div data-testid="recording-diagnostic-row" className="flex flex-wrap items-center gap-2">
                    <span className="text-muted-foreground">取り込み</span>
                    <span>{ingestDetailText(ingestState)}</span>
                  </div>
                )}
                {(recording.encodeStatus?.length ?? 0) > 0 && (
                  <div data-testid="recording-diagnostic-row" className="flex flex-wrap items-center gap-2">
                    <span className="text-muted-foreground">エンコード</span>
                    <EncodeStatusBadges recording={recording} />
                  </div>
                )}
                <div data-testid="recording-diagnostic-row" className="flex flex-wrap items-center gap-2">
                  <span className="text-muted-foreground">CM 検出</span>
                  <span>{cmDetectionLabel(recording.cmDetection.state)}</span>
                  {recording.cmDetection.state === 'detected' && recording.cmDetection.ranges && (
                    <span>{recording.cmDetection.ranges.length} 区間</span>
                  )}
                  {recording.cmDetection.state === 'failed' && <span>{cmDetectStageMessage(recording.cmDetection.stage)}</span>}
                  {/* logo / area は枠を教えるのが直し方なので、記録の明細からそこへ行けるようにする。 */}
                  {recording.cmDetection.state === 'failed' &&
                    (recording.cmDetection.stage === 'logo' || recording.cmDetection.stage === 'area') && (
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
                </div>
                {showCMDetectorResults && (
                  <details data-testid="cm-detector-results-details" className="text-muted-foreground">
                    <summary className="cursor-pointer">検出器の結果</summary>
                    <ul className="flex flex-col gap-1 py-1">
                      {rawCMRanges.map((range) => (
                        <li key={`${range.startMs}-${range.endMs}`}>{formatCMOffset(range.startMs)} – {formatCMOffset(range.endMs)}</li>
                      ))}
                    </ul>
                  </details>
                )}
                {recording.cmDetection.state === 'failed' && recording.cmDetection.error && (
                  <details data-testid="cm-detection-technical-details" className="text-muted-foreground">
                    <summary className="cursor-pointer">技術的な詳細</summary>
                    <pre className="max-h-40 overflow-auto whitespace-pre-wrap font-mono text-xs">{recording.cmDetection.error}</pre>
                  </details>
                )}
                {hasDrops && recording.dropSummary && (
                  <div data-testid="recording-diagnostic-row" className="flex flex-wrap items-center gap-3">
                    <span className="text-muted-foreground">ドロップ</span>
                    <span>パケット {recording.dropSummary.packets.toLocaleString()}</span>
                    <span>ドロップ {recording.dropSummary.drops.toLocaleString()}</span>
                    <span>エラー {recording.dropSummary.errors.toLocaleString()}</span>
                    <span>スクランブル {recording.dropSummary.scrambled.toLocaleString()}</span>
                  </div>
                )}
                {hasDrops && <DropStatsTable recordingId={recording.id} />}
                {(recording.qualityEvents?.length ?? 0) > 0 && (
                  <details data-testid="recording-quality-events-details" className="text-muted-foreground">
                    <summary className="cursor-pointer">品質イベント {recording.qualityEvents?.length} 件</summary>
                    <ul className="flex flex-col gap-1 py-1">
                      {recording.qualityEvents?.map((event, index) => (
                        <li key={index} className="break-all">{String(event.event ?? 'unknown')}{event.reason ? `: ${JSON.stringify(event.reason)}` : ''}</li>
                      ))}
                    </ul>
                  </details>
                )}
              </section>
            )}
          </div>
        </section>
      </div>

      {hasShelf && (
        <aside data-testid="recording-series-shelf" aria-label="シリーズの録画" className="hidden min-w-0 border-l border-border pl-5 lg:block">
          <div className="mb-3 flex items-baseline justify-between gap-2">
            {/* 棚がある幅では、見出しのシリーズ名がシリーズ画面への導線を受け持つ。 */}
            <h3 className="min-w-0 font-semibold">
              <Link
                to="/recordings/$id/series"
                params={{ id: String(recording.id) }}
                aria-label={`このシリーズへ: ${recording.series}`}
                className="inline-flex min-h-6 max-w-full items-center gap-1 underline underline-offset-4"
              >
                <span className="truncate">{recording.series}</span>
                <span aria-hidden>›</span>
              </Link>
            </h3>
            {shelfQuery.isSuccess && (
              <span data-testid="series-shelf-summary" className="shrink-0 text-xs text-muted-foreground">
                {shelfTruncated
                  ? `${seriesShelfPageSize} 本以上`
                  : `${shelfRecordings.length} 本 · ${formatBytes(shelfTotalBytes)}`}
              </span>
            )}
          </div>
          <ul className="flex flex-col gap-2">
            {shelfRecordings.map((item) => {
              const current = item.id === recording.id
              const parts = current
                ? [item.status === 'recording' ? '録画中' : '再生中']
                : [
                    item.status === 'recording'
                      ? '録画中'
                      : item.watchedAt
                        ? '視聴済み'
                        : item.status === 'finished'
                          ? '未視聴'
                          : undefined,
                    formatDuration(item.durationMs),
                    item.sizeBytes === undefined && (item.encodedAssets?.length ?? 0) > 0 ? '原本なし' : undefined,
                  ].filter((part): part is string => part !== undefined)
              const progress = shelfWatchProgress(item)
              return (
                <li key={item.id}>
                  <Link
                    to="/recordings/$id"
                    params={{ id: String(item.id) }}
                    aria-current={current ? 'page' : undefined}
                    className={cn(
                      'flex min-w-0 items-center gap-3 rounded-md p-2 hover:bg-muted',
                      current && 'ring-1 ring-foreground',
                    )}
                    onClick={() => seedRecordingDetail(queryClient, item)}
                  >
                    <span className="relative aspect-video w-20 shrink-0 overflow-hidden rounded bg-muted">
                      <img
                        src={`/api/media/recordings/${item.id}/thumbnail`}
                        alt=""
                        loading="lazy"
                        className="size-full object-cover"
                        onError={(event) => event.currentTarget.remove()}
                      />
                      {progress !== undefined && (
                        <ThumbnailProgressLine progress={progress} testId="series-shelf-progress-line" />
                      )}
                    </span>
                    <span className="min-w-0">
                      <span className="block truncate text-sm font-medium">{formatDate(item.startAt)}</span>
                      <span className="block truncate text-xs text-muted-foreground">{parts.join(' · ')}</span>
                    </span>
                  </Link>
                </li>
              )
            })}
          </ul>
          <p className="mt-3 text-xs text-muted-foreground">行を押すと、その録画の詳細へ移ります。まとめて操作する場合はシリーズ画面を使います。</p>
        </aside>
      )}
    </div>
  )
}

/**
 * shelfWatchProgress は棚のサムネイルに重ねる進み線の割合（0〜100）。視聴済みは全幅、
 * 途中まで見た回は保存位置の割合、まだ見ていない回は線を出さない（undefined）。
 */
function shelfWatchProgress(item: Pick<Recording, 'watchedAt' | 'resumePositionMs' | 'durationMs'>): number | undefined {
  if (item.watchedAt !== undefined) return 100
  if (item.resumePositionMs === undefined || item.durationMs <= 0) return undefined
  return Math.max(0, Math.min(100, (item.resumePositionMs / item.durationMs) * 100))
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

/** seriesShelfPageSize は棚が 1 回で引く件数（API の上限）。これ以上あるシリーズは件数を「以上」で出す。 */
const seriesShelfPageSize = 200

type DetailTab = 'programme' | 'versions' | 'record'

/** defaultDetailTab はスマホ（md 未満）では番組、それ以外では版を最初に開く。 */
function defaultDetailTab(): DetailTab {
  return typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(max-width: 767px)').matches
    ? 'programme'
    : 'versions'
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
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <span>
        ルール「
        <Link
          to="/search"
          search={{ ruleId }}
          className="text-primary underline-offset-2 hover:underline"
        >
          {label}
        </Link>
        」
      </span>
      <Link
        to="/recordings"
        search={{ ruleId }}
        className="text-muted-foreground underline-offset-2 hover:underline"
      >
        このルールの録画で絞る
      </Link>
    </span>
  )
}
