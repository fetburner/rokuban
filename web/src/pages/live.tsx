import { Link, useSearch as useRouteSearch } from '@tanstack/react-router'
import { Play } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'

import { useListPrograms, useListServices, type ProgramListItem, type Service } from '@/api/generated'
import { unwrap } from '@/api/unwrap'
import { EmptyState, ErrorState, ListSkeleton, PageHeader } from '@/components/page'
import { LivePlayer } from '@/components/live-player'
import { Button } from '@/components/ui/button'
import { useLiveCapability } from '@/lib/capabilities'
import { currentProgramWindow, pickInitialServiceId } from '@/lib/live'
import { orderServices } from '@/lib/epg-grid'
import { formatTime, isAiring } from '@/lib/format'
import { useCurrentSite } from '@/lib/site'
import { cn } from '@/lib/utils'

/**
 * nowPlayingRefetchMs は「いま放送中」表示を作り直す間隔。
 *
 * 番組が終わって次の番組に切り替わるタイミングを追いかけるためのポーリング。
 * SSE の `programs` 相当のトピックは無い（EPG 更新は元々 watcher の定期
 * ジョブなので、番組の切り替わり自体はサーバー側で NOTIFY されない）ため、
 * レベルトリガーの精神（不変条件 5）に沿って一定間隔で再取得する。
 *
 * **`useListPrograms` に `refetchInterval` は渡さない。** `nowMs`（tick）が
 * この間隔で更新されるたびに `currentProgramWindow` の結果が変わって
 * クエリキー自体が変わるので、それだけで同じ周期の再取得が起きる ---
 * `refetchInterval` を並置すると「キー変化による取得」と「タイマーによる
 * 取得」の同じ周期の再取得が二重に走るだけで、後者は何も追加しない
 * （レビュー #190 の指摘）。
 */
const nowPlayingRefetchMs = 30_000

type ChannelGroup = { channelType: string; services: Service[] }

/** channelTypeLabels は channelType の日本語表記（components/channel-picker.tsx と同じ表）。 */
const channelTypeLabels: Record<string, string> = {
  GR: '地上波',
  BS: 'BS',
  CS: 'CS',
  SKY: 'SKY',
}

function channelTypeLabel(channelType: string): string {
  return channelTypeLabels[channelType] ?? channelType
}

/** groupByChannelType は種別ごとの連続した塊にまとめる（orderServices 済みの入力を前提）。 */
function groupByChannelType(ordered: readonly Service[]): ChannelGroup[] {
  const groups: ChannelGroup[] = []
  for (const service of ordered) {
    const last = groups[groups.length - 1]
    if (last && last.channelType === service.channelType) {
      last.services.push(service)
    } else {
      groups.push({ channelType: service.channelType, services: [service] })
    }
  }
  return groups
}

/**
 * LivePage はライブ視聴画面（M4-4。選択と視聴開始の分離は M7-1）。
 *
 * 「チャンネル一覧から選んでブラウザ再生、画質切り替え程度で良い」
 * （docs/frontend.md §ライブ視聴）という方針どおり、機能は絞る。プロファイル
 * （画質）を選ぶ UI は持たない --- `live.profiles` を列挙する API が無く
 * （M4-3 は OpenAPI 対象外。issue #92 着手前のコメント参照）、選択肢を UI に
 * 出すと「機能しないコントロール」になるため、既定プロファイル（先頭）に
 * 固定する。
 *
 * 選択中のチャンネルは `?serviceId=` に持つ（`pages/search.tsx` の `?ruleId` と
 * 同じ形）。これにより特定チャンネルへの直リンクが作れる。チャンネルを切り替える
 * ナビゲーションは `replace` にし、ザッピングでブラウザ履歴が積み上がらないように
 * する。
 *
 * **「選ぶ」と「流す」を別のタップに分ける（issue #234 M7-1）。** チャンネルを
 * 選ぶこと自体は probe もセッション（チューナー確保 + ffmpeg 起動）も起こさない
 * ---「いま放送中」の表示とチャンネル種別だけを見せ、`LivePlayer` は「再生」ボタンを
 * 押すまでマウントしない。確認ダイアログは使わない --- 選択状態の画面そのものが
 * 値札であり、再生は 1 タップで足りる（ダイアログを重ねると、チューナーが有限で
 * ないデスクトップ LAN の利用者にまで摩擦を強いる。docs/frontend.md
 * §ライブ視聴「選択と視聴開始の分離」参照）。
 *
 * デバウンスは持たない。以前はザッピングのたびに probe / セッションが走っていた
 * ため「通り過ぎたチャンネル」を掴まないための緩和として 400ms のデバウンスを
 * 挟んでいたが、選択自体がコスト 0 になった今はデバウンスする対象（probe /
 * セッション開始）がチャンネル選択の瞬間には発生しない。デバウンスの存在理由が
 * 消えたので削除した（issue #234 の受け入れ 4）。
 */
export function LivePage() {
  const liveCapability = useLiveCapability()
  const site = useCurrentSite()
  const services = useListServices(site)
  const routeSearch = useRouteSearch({ from: '/live' })

  const orderedServices = useMemo(() => orderServices(unwrap(services.data) ?? []), [services.data])
  const groups = useMemo(() => groupByChannelType(orderedServices), [orderedServices])

  const selectedServiceId = pickInitialServiceId(orderedServices, routeSearch.serviceId)
  const selectedService = orderedServices.find((s) => s.serviceId === selectedServiceId)

  // isPlaying は「再生」ボタンで明示的に視聴を始めたかどうか。**選択（URL の
  // ?serviceId= = selectedServiceId）が変わるたびに false へ戻す** --- 直リンク・
  // ブックマークで来た場合（issue #234 の含むもの 3）も、チャンネル一覧で
  // 別のチャンネルへ切り替えた場合も、同意はチャンネルごとに 1 回ずつ必要
  // というのが構造で同意を取る設計の要点。この reset の effect を落とすと、A を再生中に
  // B を選んでからまた A を選び直したときに（selectedServiceId が A→B→A と
  // 変化しても isPlaying が true のまま残るため）再生ボタンを経由せず A の
  // LivePlayer が再マウントされてしまう
  const [isPlaying, setIsPlaying] = useState(false)
  useEffect(() => {
    setIsPlaying(false)
  }, [selectedServiceId])

  // nowMs は「いま」を一定間隔で更新するティック。Date.now() を毎レンダー呼ぶだけでは
  // 再レンダーの理由にならず、番組が終わっても表示が切り替わらない。
  const [nowMs, setNowMs] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNowMs(Date.now()), nowPlayingRefetchMs)
    return () => clearInterval(id)
  }, [])

  const window_ = currentProgramWindow(nowMs)
  const nowPlayingQuery = useListPrograms(
    site,
    { start: window_.start, end: window_.end, serviceId: selectedServiceId !== undefined ? [selectedServiceId] : undefined },
    { query: { enabled: selectedServiceId !== undefined } },
  )
  const nowPlaying = useMemo(() => {
    const programs: ProgramListItem[] = unwrap(nowPlayingQuery.data) ?? []
    return programs.find((p) => isAiring(p.startAt, p.endAt, nowMs))
  }, [nowPlayingQuery.data, nowMs])

  return (
    <>
      <PageHeader title="ライブ" />

      {/* 能力 API の 4 状態を潰さずに出し分ける（issue #209 のレビュー指摘）。
          「無効です」と原因を名指しできるのは disabled を実際に受け取ったとき
          だけで、pending / unknown で同じ文言を出すと、live.enabled: true の
          デプロイで能力 API が瞬断しただけでも「設定が無効」と嘘をつく。 */}
      {liveCapability === 'pending' ? (
        <ListSkeleton />
      ) : liveCapability === 'unknown' ? (
        <ErrorState>ライブ視聴が利用できるかを確認できませんでした</ErrorState>
      ) : liveCapability === 'disabled' ? (
        // 主ナビからは消えているので通常ここには来ないが、直リンク・ブックマーク・
        // 戻る操作では来る（issue #209 の受け入れ 2）。何も言わずに再生を試すと
        // プレイリストが 404 になるだけで、原因（サーバー設定）に辿り着けない。
        <EmptyState>
          <p>この環境ではライブ視聴が無効です</p>
          <p className="mt-1 text-muted-foreground">
            サーバーの設定（<code>live.enabled</code>）で有効にすると使えます
          </p>
        </EmptyState>
      ) : services.isError ? (
        <ErrorState>チャンネル一覧の取得に失敗しました</ErrorState>
      ) : services.isPending ? (
        <ListSkeleton />
      ) : orderedServices.length === 0 ? (
        <EmptyState>チャンネルがありません</EmptyState>
      ) : selectedService === undefined ? (
        // pickInitialServiceId はサービスが 1 件以上あれば必ず何かを返すので
        // ここには来ないはずだが、型上 undefined を許すため防御的に置く
        <EmptyState>チャンネルを選んでください</EmptyState>
      ) : (
        <div className="flex flex-col gap-4 p-4 lg:flex-row lg:items-start">
          <div className="flex min-w-0 flex-1 flex-col gap-2">
            {isPlaying ? (
              <LivePlayer
                site={site}
                networkId={selectedService.networkId}
                serviceId={selectedService.serviceId}
              />
            ) : (
              <LiveSelectionPreview
                serviceName={selectedService.name}
                onPlay={() => setIsPlaying(true)}
              />
            )}
            <div>
              <div className="flex items-center gap-2">
                <p className="font-medium">{selectedService.name}</p>
                {/* チャンネル種別（GR/BS/CS）は選択のコストを判断する手がかり
                    （issue #234 の含むもの 1）。「チューナーが空いている」等の
                    保証はしない中立の事実表示 --- mirakc には Rokuban から見えない
                    消費者がいる（docs/data.md §6.5 と同じ下界主義。CLAUDE.md「罠」）。 */}
                <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[0.65rem] font-medium text-muted-foreground">
                  {channelTypeLabel(selectedService.channelType)}
                </span>
                {nowPlaying && <OnAirBadge />}
              </div>
              {nowPlaying ? (
                <p className="text-sm text-muted-foreground">
                  <span>
                    {formatTime(nowPlaying.startAt)}〜{formatTime(nowPlaying.endAt)}
                  </span>{' '}
                  <span>{nowPlaying.name}</span>
                </p>
              ) : !nowPlayingQuery.isPending ? (
                <p className="text-sm text-muted-foreground">いま放送中の番組の情報はありません</p>
              ) : null}
              {/* ライブ ⇄ 番組表の導線（issue #231）。番組表の `?serviceId=` は
                  `/recordings` と同じ形（複数可の配列）なので、1 局分の配列を渡す。 */}
              <Link
                to="/"
                search={{ serviceId: [selectedService.serviceId] }}
                className="mt-1 inline-block text-sm text-primary underline-offset-2 hover:underline"
              >
                この局の番組表
              </Link>
            </div>
          </div>

          <nav aria-label="チャンネル一覧" className="w-full shrink-0 lg:w-72">
            <ul className="flex flex-col gap-3">
              {groups.map((group) => (
                <li key={group.channelType}>
                  <p className="px-1 pb-1 text-xs font-medium text-muted-foreground">
                    {channelTypeLabel(group.channelType)}
                  </p>
                  <ul className="flex flex-col gap-1">
                    {group.services.map((s) => (
                      <li key={`${s.networkId}-${s.serviceId}`}>
                        {/* チャンネルを選ぶこと自体はコスト 0（probe もセッションも
                            起こさない）なので、デバウンスも onClick での介入も無い
                            --- 通常のクリックナビゲーションのまま（issue #234）。
                            `replace` はザッピングでブラウザ履歴が積み上がらない
                            ようにするため。 */}
                        <Link
                          to="/live"
                          search={{ serviceId: s.serviceId }}
                          replace
                          aria-current={s.serviceId === selectedServiceId ? 'page' : undefined}
                          className={cn(
                            'flex min-h-11 w-full items-center gap-2 rounded-md px-2 py-2 text-left text-sm transition-colors hover:bg-muted',
                            s.serviceId === selectedServiceId && 'bg-muted font-medium',
                          )}
                        >
                          {s.channelType === 'GR' && s.remoteControlKeyId > 0 && (
                            <span className="shrink-0 rounded bg-muted px-1 text-[10px] text-muted-foreground">
                              {s.remoteControlKeyId}
                            </span>
                          )}
                          <span className="truncate">{s.name}</span>
                        </Link>
                      </li>
                    ))}
                  </ul>
                </li>
              ))}
            </ul>
          </nav>
        </div>
      )}
    </>
  )
}

/**
 * LiveSelectionPreview は「選択」段階の表示（issue #234 M7-1）。
 *
 * `LivePlayer` と同じ寸法（`aspect-video` の黒地）にして、再生ボタンを押した
 * 瞬間のレイアウトシフトを避ける。probe もセッションもここでは起こさない ---
 * 「再生」ボタンを押した後に呼び出し側が `LivePlayer` をマウントするまで、
 * ネットワーク要求は一切発生しない。
 */
function LiveSelectionPreview({
  serviceName,
  onPlay,
}: {
  serviceName: string
  onPlay: () => void
}) {
  return (
    <div className="relative flex aspect-video w-full max-w-3xl items-center justify-center rounded bg-black">
      <Button type="button" size="lg" aria-label={`${serviceName}を再生`} onClick={onPlay}>
        <Play data-icon="inline-start" />
        再生
      </Button>
    </div>
  )
}

/**
 * OnAirBadge は「いま電波に乗っている」ことを示すバッジ（M4-4 のライブ視聴専用）。
 *
 * **走査線は 3 箇所限定の使用箇所の 1 つ**（ON AIR。docs/frontend/design.md
 * 「走査線は 3 箇所限定」）。タリーレッドの塗り（`tally-scanlines` /
 * `text-tally-foreground`）は録画中バッジと同じ組み合わせをベースにしており、
 * AA を満たすことを確認済み（`e2e/design.mjs`）。選択中チャンネルに `nowPlaying`
 * （いま放送中の番組）があるときだけ呼び出し側が描画する。
 */
function OnAirBadge() {
  return (
    <span className="tally-scanlines shrink-0 rounded px-1.5 py-0.5 text-[0.65rem] font-medium text-tally-foreground">
      ON AIR
    </span>
  )
}
