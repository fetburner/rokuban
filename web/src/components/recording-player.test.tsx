import { fireEvent, render, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { RecordingPlayer } from '@/components/recording-player'

afterEach(() => {
  localStorage.clear()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

function stubSuccessfulAPI() {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: true,
    status: 204,
    statusText: 'No Content',
    headers: new Headers(),
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

/** jsdom の video 要素は currentTime/duration の実再生をしないので、テスト側から直接設定する。 */
function setMediaProps(
  video: HTMLVideoElement,
  props: { currentTime?: number; duration?: number; paused?: boolean },
) {
  if (props.paused !== undefined) {
    // jsdom の video は常に paused なので、再生中の経路（自動スキップ）を通すには
    // ここを偽にする必要がある。
    Object.defineProperty(video, 'paused', {
      value: props.paused,
      writable: true,
      configurable: true,
    })
  }
  if (props.currentTime !== undefined) {
    Object.defineProperty(video, 'currentTime', {
      value: props.currentTime,
      writable: true,
      configurable: true,
    })
  }
  if (props.duration !== undefined) {
    Object.defineProperty(video, 'duration', {
      value: props.duration,
      writable: true,
      configurable: true,
    })
  }
}

function openPlaybackSettings(container: HTMLElement): HTMLElement {
  const existing = container.querySelector<HTMLElement>('[data-testid="playback-settings"]')
  if (existing) return existing
  fireEvent.click(container.querySelector('button[aria-label="再生設定"]')!)
  return container.querySelector<HTMLElement>('[data-testid="playback-settings"]')!
}

/** openSubmenu は設定メニューの「›」の行（画質 / 再生速度）から下の階層へ入り、メニューを返す。 */
function openSubmenu(container: HTMLElement, label: '画質' | '再生速度'): HTMLElement {
  const menu = openPlaybackSettings(container)
  fireEvent.click(within(menu).getByRole('menuitem', { name: label }))
  return container.querySelector<HTMLElement>('[data-testid="playback-settings"]')!
}

/** radioTexts は下の階層の選択肢の表示文字列を返す。 */
function radioTexts(menu: HTMLElement): string[] {
  return within(menu).getAllByRole('menuitemradio').map((el) => el.textContent ?? '')
}

/** selectProfile は設定メニューの画質の下の階層から profile を選ぶ。 */
function selectProfile(container: HTMLElement, profile: string) {
  const menu = openSubmenu(container, '画質')
  const option = within(menu).getAllByRole('menuitemradio').find((el) => el.textContent?.includes(profile))
  fireEvent.click(option!)
}

describe('RecordingPlayer の字幕サイドカー', () => {
  it('encoded 動画に WebVTT subtitle track を付ける', () => {
    const { container } = render(
      <RecordingPlayer recordingId={7} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />,
    )
    const track = container.querySelector('track')
    expect(track).not.toBeNull()
    expect(track?.kind).toBe('subtitles')
    expect(track?.src).toContain('/api/media/recordings/7/file?profile=h264&track=subtitles')
  })
})

describe('RecordingPlayer のサーバー再生位置', () => {
  it('pause で原本時間軸の ms を API に保存する', async () => {
    const fetchMock = stubSuccessfulAPI()
    const { container } = render(
      <RecordingPlayer recordingId={9} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />,
    )
    const video = container.querySelector('video')!

    setMediaProps(video, { currentTime: 30.25, duration: 300 })
    fireEvent.pause(video)

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledWith('/api/recordings/9/playback-position', expect.objectContaining({
      method: 'PUT',
      body: JSON.stringify({ positionMs: 30_250 }),
    }))
    expect(Object.keys(localStorage)).toEqual([])
  })

  it('2 秒未満と終端 90% では位置を消し、視聴済みにする', async () => {
    const fetchMock = stubSuccessfulAPI()
    const { container } = render(
      <RecordingPlayer recordingId={10} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />,
    )
    const video = container.querySelector('video')!

    setMediaProps(video, { currentTime: 1.5, duration: 300 })
    fireEvent.pause(video)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock.mock.calls[0]![0]).toBe('/api/recordings/10/playback-position')
    expect(fetchMock.mock.calls[0]![1]).toEqual(expect.objectContaining({ method: 'DELETE' }))

    fetchMock.mockClear()
    setMediaProps(video, { currentTime: 270, duration: 300 })
    fireEvent.timeUpdate(video)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledWith('/api/recordings/10/watched', expect.objectContaining({ method: 'PUT' }))
  })

  it('カット版の再開 ms を復元し、pause 位置を原本時間軸へ変換する', async () => {
    const fetchMock = stubSuccessfulAPI()
    const keepRanges = [
      { startMs: 10_000, endMs: 20_000 },
      { startMs: 30_000, endMs: 50_000 },
    ]
    const { container } = render(
      <RecordingPlayer
        recordingId={13}
        resumePositionMs={15_000}
        preferredProfile="cut"
        encodedAssets={[
          { profile: 'cut', sizeBytes: 2, cut: true, keepRanges },
        ]}
      />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 0, duration: 30 })
    fireEvent.loadedMetadata(video)
    expect(video.currentTime).toBe(5)

    setMediaProps(video, { currentTime: 10, duration: 30 })
    fireEvent.pause(video)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledWith('/api/recordings/13/playback-position', expect.objectContaining({
      method: 'PUT',
      body: JSON.stringify({ positionMs: 30_000 }),
    }))
  })

  it('90% 到達の視聴済み PUT が通ったら onWatched を呼ぶ', async () => {
    stubSuccessfulAPI()
    const onWatched = vi.fn()
    const { container } = render(
      <RecordingPlayer
        recordingId={14}
        onWatched={onWatched}
        encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]}
      />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 0, duration: 300 })
    fireEvent.loadedMetadata(video)
    setMediaProps(video, { currentTime: 270, duration: 300 })
    fireEvent.timeUpdate(video)
    await waitFor(() => expect(onWatched).toHaveBeenCalledTimes(1))
  })

  describe('同じページ内の画質切替（resumePositionMs はページを開いた時点の値のまま）', () => {
    const keepRanges = [
      { startMs: 10_000, endMs: 20_000 },
      { startMs: 30_000, endMs: 50_000 },
    ]
    const assets = [
      { profile: 'h264', sizeBytes: 1 },
      { profile: 'h265', sizeBytes: 2 },
      { profile: 'cut', sizeBytes: 3, cut: true, keepRanges },
    ]

    function switchTo(container: HTMLElement, profile: string) {
      selectProfile(container, profile)
      const video = container.querySelector('video')!
      setMediaProps(video, { currentTime: 0, duration: 1000 })
      fireEvent.loadedMetadata(video)
      return video
    }

    function watch(container: HTMLElement, seconds: number) {
      const video = container.querySelector('video')!
      setMediaProps(video, { currentTime: 0, duration: 1000 })
      fireEvent.loadedMetadata(video)
      setMediaProps(video, { currentTime: seconds, duration: 1000 })
      fireEvent.pause(video)
    }

    it('非カット → 非カットは同じ原本秒から始まる', () => {
      stubSuccessfulAPI()
      const { container } = render(
        <RecordingPlayer recordingId={20} resumePositionMs={5_000} preferredProfile="h264" encodedAssets={assets} />,
      )
      watch(container, 600)
      expect(switchTo(container, 'h265').currentTime).toBe(600)
    })

    it('非カット → カットは対応するカット版の秒から始まる', () => {
      stubSuccessfulAPI()
      const { container } = render(
        <RecordingPlayer recordingId={21} resumePositionMs={5_000} preferredProfile="h264" encodedAssets={assets} />,
      )
      watch(container, 35)
      // 原本 35 s = keep[1] の 5 s 目 = カット版 10 + 5 s
      expect(switchTo(container, 'cut').currentTime).toBe(15)
    })

    it('カット → 非カットは原本の秒から始まる', () => {
      stubSuccessfulAPI()
      const { container } = render(
        <RecordingPlayer recordingId={22} resumePositionMs={5_000} preferredProfile="cut" encodedAssets={assets} />,
      )
      watch(container, 15)
      expect(switchTo(container, 'h264').currentTime).toBe(35)
    })
  })

  it('再生中に keepRanges が別世代へ変わっても、保存される原本の秒は各世代の変換表で写される', async () => {
    const fetchMock = stubSuccessfulAPI()
    const oldRanges = [
      { startMs: 10_000, endMs: 20_000 },
      { startMs: 30_000, endMs: 50_000 },
    ]
    const newRanges = [
      { startMs: 25_000, endMs: 35_000 },
      { startMs: 60_000, endMs: 80_000 },
    ]
    const props = { recordingId: 23, preferredProfile: 'cut' }
    const { container, rerender } = render(
      <RecordingPlayer {...props} encodedAssets={[{ profile: 'cut', sizeBytes: 2, cut: true, keepRanges: oldRanges }]} />,
    )
    const oldVideo = container.querySelector('video')!
    setMediaProps(oldVideo, { currentTime: 0, duration: 30 })
    fireEvent.loadedMetadata(oldVideo)
    setMediaProps(oldVideo, { currentTime: 12, duration: 30 })
    fireEvent.pause(oldVideo)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock.mock.calls[0]![1]).toEqual(expect.objectContaining({ body: JSON.stringify({ positionMs: 32_000 }) }))

    rerender(
      <RecordingPlayer {...props} encodedAssets={[{ profile: 'cut', sizeBytes: 2, cut: true, keepRanges: newRanges }]} />,
    )
    const newVideo = container.querySelector('video')!
    // 別世代のファイルなので <video> ごと作り直される。
    expect(newVideo).not.toBe(oldVideo)
    setMediaProps(newVideo, { currentTime: 0, duration: 30 })
    fireEvent.loadedMetadata(newVideo)
    // 原本 32 s は新世代の keep（25-35 s）に含まれ、カット版の 7 s 目になる。
    expect(newVideo.currentTime).toBe(7)

    fetchMock.mockClear()
    setMediaProps(newVideo, { currentTime: 12, duration: 30 })
    fireEvent.pause(newVideo)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    // 旧世代の表なら 32_000。新世代の表: 12 s = 2 つ目の keep の 2 s 目 = 原本 62 s。
    expect(fetchMock.mock.calls[0]![1]).toEqual(expect.objectContaining({ body: JSON.stringify({ positionMs: 62_000 }) }))
  })
})

// issue #236（M7-3）: 設定メニュー内の値札。サイズが取れない資産でも
// プロファイル名を選択肢から隠さない。
describe('RecordingPlayer のサイズ常置（値札、issue #236）', () => {
  it('プロファイルが 1 つのとき、画質の選択肢にサイズを付ける', () => {
    const { container } = render(
      <RecordingPlayer recordingId={20} encodedAssets={[{ profile: 'h264', sizeBytes: 1_200_000 }]} />,
    )
    expect(radioTexts(openSubmenu(container, '画質'))).toEqual(['h2641.1 MB'])
  })

  it('プロファイルが 1 つで sizeBytes が省略されているとき、プロファイル名は出すがサイズは出さない（隠さない）', () => {
    const { container } = render(
      <RecordingPlayer recordingId={21} encodedAssets={[{ profile: 'h264' }]} />,
    )
    const menu = openSubmenu(container, '画質')
    expect(radioTexts(menu)).toEqual(['h264'])
    // 選択肢（プロファイル名）自体は必ず出る --- サイズが取れないことを理由に
    // 隠すと「機能しないコントロールは置かない」の逆（機能するコントロールを
    // 隠す）になる。
    expect(container.textContent).toContain('h264')
    expect(container.textContent).not.toMatch(/\d+(\.\d+)? (B|KB|MB|GB|TB)/)
    // video 自体は描かれる（プレイヤーが消えたわけではない）
    expect(container.querySelector('video')).not.toBeNull()
  })

  it('プロファイルが複数のとき、画質の各選択肢にサイズを付ける。サイズが無いものは名前だけになる', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={22}
        encodedAssets={[
          { profile: 'h264', sizeBytes: 500_000_000 },
          { profile: 'h265' },
        ]}
      />,
    )
    expect(radioTexts(openSubmenu(container, '画質'))).toEqual(['h264476.8 MB', 'h265'])
  })

  it('encoded が無く原本のみのとき、空状態から VLC リンクを出す', () => {
    const { container } = render(
      <RecordingPlayer recordingId={23} encodedAssets={[]} hasOriginal />,
    )
    const link = container.querySelector('a')!
    expect(link.textContent).toBe('VLC 等で開く')
  })

  it('encoded が無く原本のみでサイズが無くても VLC リンクは出す', () => {
    const { container } = render(
      <RecordingPlayer recordingId={24} encodedAssets={[]} hasOriginal />,
    )
    const link = container.querySelector('a')!
    expect(link.textContent).toBe('VLC 等で開く')
  })

  it('encoded 再生では原本リンクをプレイヤーの外に置く', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={25}
        encodedAssets={[{ profile: 'h264', sizeBytes: 100 }]}
        hasOriginal
      />,
    )
    expect(container.querySelector('a[href="/api/media/recordings/25/file"]')).toBeNull()
  })
})

// 定石の VOD プレイヤーは設定メニューにダウンロードを置かない。
describe('RecordingPlayer の設定メニューにダウンロードを置かない', () => {
  it('メニューのどの階層にもダウンロードが無く、プレイヤー内のリンクは 0 本', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={28}
        encodedAssets={[
          { profile: 'h264', sizeBytes: 100 },
          { profile: 'h265', sizeBytes: 200 },
        ]}
        hasOriginal
      />,
    )
    expect(openPlaybackSettings(container)).not.toHaveTextContent('ダウンロード')
    expect(openSubmenu(container, '画質')).not.toHaveTextContent('ダウンロード')
    expect(container.querySelectorAll('a')).toHaveLength(0)
  })
})

describe('RecordingPlayer の再生操作', () => {
  const asset = [{ profile: 'h264', sizeBytes: 123 }]

  it('native controls を外し、自前の再生操作とアクセシブルな seekbar を出す', () => {
    const { container, getByRole, queryByRole } = render(
      <RecordingPlayer recordingId={30} encodedAssets={asset} />,
    )

    expect(container.querySelector('video')).toHaveProperty('controls', false)
    expect(getByRole('slider', { name: 'シークバー' })).toHaveAttribute('aria-valuetext', '0:00 / 0:00')
    expect(getByRole('button', { name: '再生' })).toBeInTheDocument()
    expect(getByRole('button', { name: '再生設定' })).toBeInTheDocument()
    expect(queryByRole('button', { name: 'ピクチャーインピクチャー' })).not.toBeInTheDocument()
    expect(queryByRole('combobox', { name: '再生速度' })).not.toBeInTheDocument()
  })

  // 速度は「この録画をどう見るか」ではなく「自分がどう見るか」の好みなので、
  // 自前メニューの変更を保存し、録画をまたいでも保つ
  // （docs/frontend/design.md §個人化）。
  it('メニューで選んだ速度を保存し、別の録画でも video に適用する', () => {
    const { container, rerender } = render(
      <RecordingPlayer recordingId={30} encodedAssets={asset} />,
    )
    let video = container.querySelector('video')!
    const speeds = openSubmenu(container, '再生速度')
    expect(radioTexts(speeds)).toEqual(['0.5x', '0.75x', '標準', '1.25x', '1.5x', '1.75x', '2x'])
    fireEvent.click(within(speeds).getByRole('menuitemradio', { name: '1.5x' }))

    expect(localStorage.getItem('rokuban:playback-rate')).toBe('1.5')
    expect(video.defaultPlaybackRate).toBe(1.5)

    rerender(<RecordingPlayer recordingId={31} encodedAssets={asset} />)

    video = container.querySelector('video')!
    expect(video.playbackRate).toBe(1.5)
    expect(video.defaultPlaybackRate).toBe(1.5)
  })

  it('保存済みの速度は開いた直後から video に効く', () => {
    localStorage.setItem('rokuban:playback-rate', '2')
    const { container } = render(
      <RecordingPlayer recordingId={32} encodedAssets={asset} />,
    )

    expect(container.querySelector('video')!.playbackRate).toBe(2)
    expect(container.querySelector('video')!.defaultPlaybackRate).toBe(2)
  })

  it('自前のボタンと設定が video の再生状態へ反映される', () => {
    const { container, getByRole, rerender } = render(
      <RecordingPlayer recordingId={32} encodedAssets={asset} />,
    )
    const video = container.querySelector('video')!
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    const pause = vi.spyOn(video, 'pause').mockImplementation(() => {})
    const subtitleTrack = { kind: 'subtitles', mode: 'disabled', cues: null }
    Object.defineProperty(video, 'textTracks', { value: [subtitleTrack], configurable: true })

    fireEvent.click(getByRole('button', { name: '再生' }))
    expect(play).toHaveBeenCalledOnce()
    fireEvent.play(video)
    setMediaProps(video, { paused: false })
    expect(getByRole('button', { name: '一時停止' })).toBeInTheDocument()
    fireEvent.click(getByRole('button', { name: '一時停止' }))
    expect(pause).toHaveBeenCalledOnce()

    fireEvent.click(getByRole('button', { name: 'ミュート' }))
    expect(video.muted).toBe(true)
    expect(getByRole('button', { name: 'ミュート解除' })).toBeInTheDocument()
    fireEvent.change(getByRole('slider', { name: '音量' }), { target: { value: '0.25' } })
    expect(video.volume).toBe(0.25)

    // スマホにミュート / 音量を置かない（端末の音量ボタンで足りる）ので、メニューには無い。
    const settings = openPlaybackSettings(container)
    expect(within(settings).queryByRole('slider', { name: '音量' })).toBeNull()
    expect(within(settings).queryByRole('button', { name: /ミュート/ })).toBeNull()
    expect(settings.querySelector('select, input')).toBeNull()
    // 2 択はスイッチ（menuitemcheckbox）。押しても閉じず、状態がスイッチと CC に戻る。
    const subtitleSwitch = within(settings).getByRole('menuitemcheckbox', { name: '字幕' })
    expect(subtitleSwitch).toHaveAttribute('aria-checked', 'false')
    fireEvent.click(subtitleSwitch)
    expect(subtitleTrack.mode).toBe('showing')
    expect(within(settings).getByRole('menuitemcheckbox', { name: '字幕' })).toHaveAttribute('aria-checked', 'true')
    expect(getByRole('button', { name: '字幕' })).toHaveAttribute('aria-pressed', 'true')
    // バーの CC はメニューの字幕スイッチと同じ操作。
    fireEvent.click(getByRole('button', { name: '字幕' }))
    expect(subtitleTrack.mode).toBe('disabled')
    expect(getByRole('button', { name: '字幕' })).toHaveAttribute('aria-pressed', 'false')
    // 3 択以上は「›」で下の階層。選ぶと video に効き、メニューは閉じる。
    fireEvent.click(within(openSubmenu(container, '再生速度')).getByRole('menuitemradio', { name: '2x' }))
    expect(video.playbackRate).toBe(2)
    expect(container.querySelector('[data-testid="playback-settings"]')).toBeNull()

    rerender(<RecordingPlayer recordingId={32} encodedAssets={asset} />)
    expect(container.querySelector('video')).toBe(video)
  })

  it('PiP はブラウザが対応するときだけ表示し、クリックで video に要求する', () => {
    const previous = Object.getOwnPropertyDescriptor(document, 'pictureInPictureEnabled')
    Object.defineProperty(document, 'pictureInPictureEnabled', { value: true, configurable: true })
    try {
      const { container, getByRole } = render(
        <RecordingPlayer recordingId={32} encodedAssets={asset} />,
      )
      const video = container.querySelector('video')!
      const requestPictureInPicture = vi.fn().mockResolvedValue(undefined)
      Object.defineProperty(video, 'requestPictureInPicture', { value: requestPictureInPicture })
      fireEvent.click(getByRole('button', { name: 'ピクチャーインピクチャー' }))
      expect(requestPictureInPicture).toHaveBeenCalledOnce()
      // スマホのシートの PiP 行（md 以上では CSS で隠れる）。
      const settings = openPlaybackSettings(container)
      fireEvent.click(within(settings).getByRole('menuitem', { name: 'ピクチャー・イン・ピクチャー' }))
      expect(requestPictureInPicture).toHaveBeenCalledTimes(2)
    } finally {
      if (previous) Object.defineProperty(document, 'pictureInPictureEnabled', previous)
      else Reflect.deleteProperty(document, 'pictureInPictureEnabled')
    }
  })

  it('完了録画のバーから視聴状態を PUT / DELETE し、未完了録画では隠す', () => {
    const putWatched = vi.fn()
    const deleteWatched = vi.fn()
    const { getByRole, queryByRole, rerender } = render(
      <RecordingPlayer
        recordingId={32}
        encodedAssets={asset}
        showWatched
        putWatched={putWatched}
        deleteWatched={deleteWatched}
      />,
    )
    fireEvent.click(getByRole('button', { name: '視聴済みにする' }))
    expect(putWatched).toHaveBeenCalledOnce()

    rerender(
      <RecordingPlayer
        recordingId={32}
        encodedAssets={asset}
        showWatched
        watched
        putWatched={putWatched}
        deleteWatched={deleteWatched}
      />,
    )
    fireEvent.click(getByRole('button', { name: '未視聴に戻す' }))
    expect(deleteWatched).toHaveBeenCalledOnce()

    rerender(<RecordingPlayer recordingId={32} encodedAssets={asset} />)
    expect(queryByRole('button', { name: '未視聴に戻す' })).not.toBeInTheDocument()
  })

  it('矢印キーで 10 秒、J/L で 30 秒移動する', () => {
    const { container } = render(<RecordingPlayer recordingId={31} encodedAssets={asset} />)
    const video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 60, duration: 100 })

    fireEvent.keyDown(window, { key: 'ArrowLeft' })
    expect(video.currentTime).toBe(50)
    fireEvent.keyDown(window, { key: 'ArrowRight' })
    expect(video.currentTime).toBe(60)
    fireEvent.keyDown(window, { key: 'j' })
    expect(video.currentTime).toBe(30)
    fireEvent.keyDown(window, { key: 'L' })
    expect(video.currentTime).toBe(60)
  })

  it('メタデータ読み込み前の前進キーで currentTime を壊さない', () => {
    const { container } = render(<RecordingPlayer recordingId={32} encodedAssets={asset} />)
    const video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 10, duration: Number.NaN })

    fireEvent.keyDown(window, { key: 'ArrowRight' })

    expect(video.currentTime).toBe(20)
  })

  it('0〜9 キーで再生時間の 10% 単位へ移動する', () => {
    const { container } = render(<RecordingPlayer recordingId={32} encodedAssets={asset} />)
    const video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 10, duration: 200 })

    fireEvent.keyDown(window, { key: '7' })

    expect(video.currentTime).toBe(140)
  })

  it('入力欄とボタンではページキーを処理せず、video では処理する', () => {
    const { container } = render(
      <div>
        <input aria-label="検索" />
        <RecordingPlayer recordingId={33} encodedAssets={asset} />
      </div>,
    )
    const video = container.querySelector('video')!
    const input = container.querySelector('input')!
    setMediaProps(video, { currentTime: 50, duration: 100 })

    fireEvent.keyDown(input, { key: 'ArrowRight' })
    expect(video.currentTime).toBe(50)
    fireEvent.keyDown(video, { key: 'ArrowRight' })
    expect(video.currentTime).toBe(60)
    fireEvent.keyDown(container.querySelector('button[aria-label="再生"]')!, { key: 'ArrowRight' })
    expect(video.currentTime).toBe(60)
  })

  it('seekbar の矢印キーは一度だけシークし、aria 時刻を更新する', () => {
    const { container, getByRole } = render(<RecordingPlayer recordingId={33} encodedAssets={asset} />)
    const video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 50, duration: 100 })
    fireEvent.loadedMetadata(video)

    const slider = getByRole('slider', { name: 'シークバー' })
    fireEvent.keyDown(slider, { key: 'ArrowRight' })

    expect(video.currentTime).toBe(60)
    expect(slider).toHaveAttribute('aria-valuenow', '60')
    expect(slider).toHaveAttribute('aria-valuetext', '1:00 / 1:40')
  })

  it('Space で再生し、M でミュートし、F とボタンで同じコンテナを全画面にする', () => {
    const { container } = render(<RecordingPlayer recordingId={34} encodedAssets={asset} />)
    const video = container.querySelector('video')!
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    const requestFullscreen = vi.fn(() => Promise.resolve())
    const playerFrame = container.querySelector('[data-testid="recording-player-frame"]')!
    Object.defineProperty(playerFrame, 'requestFullscreen', { value: requestFullscreen })

    fireEvent.keyDown(window, { key: ' ' })
    fireEvent.keyDown(window, { key: 'm' })
    fireEvent.keyDown(window, { key: 'F' })
    fireEvent.click(container.querySelector('button[aria-label="全画面表示"]')!)

    expect(play).toHaveBeenCalledOnce()
    expect(video.muted).toBe(true)
    expect(requestFullscreen).toHaveBeenCalledTimes(2)
  })

  it('要素全画面が無い Safari では video.webkitEnterFullscreen に落ちる', () => {
    const { container } = render(<RecordingPlayer recordingId={34} encodedAssets={asset} />)
    const video = container.querySelector('video')!
    const frame = container.querySelector('[data-testid="recording-player-frame"]')!
    const enterFullscreen = vi.fn()
    Object.defineProperty(frame, 'requestFullscreen', { value: undefined, configurable: true })
    Object.defineProperty(video, 'webkitEnterFullscreen', { value: enterFullscreen, configurable: true })

    fireEvent.keyDown(window, { key: 'f' })

    expect(enterFullscreen).toHaveBeenCalledOnce()
  })

  it('修飾キー付きのブラウザ・OS ショートカットを横取りしない', () => {
    const { container } = render(<RecordingPlayer recordingId={35} encodedAssets={asset} />)
    const video = container.querySelector('video')!
    const requestFullscreen = vi.fn(() => Promise.resolve())
    Object.defineProperty(video, 'requestFullscreen', { value: requestFullscreen })
    setMediaProps(video, { currentTime: 50, duration: 100 })

    fireEvent.keyDown(window, { key: 'f', metaKey: true })
    fireEvent.keyDown(window, { key: 'ArrowRight', ctrlKey: true })
    fireEvent.keyDown(window, { key: 'm', altKey: true })

    expect(requestFullscreen).not.toHaveBeenCalled()
    expect(video.currentTime).toBe(50)
    expect(video.muted).toBe(false)
  })
})

describe('RecordingPlayer の selectedProfile 導出', () => {
  it('preferredProfile が資産にあれば追っかけと同じ既定値を選ぶ', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={39}
        preferredProfile="h265"
        encodedAssets={[
          { profile: 'h264', sizeBytes: 100 },
          { profile: 'h265', sizeBytes: 200 },
        ]}
      />,
    )

    const menu = openPlaybackSettings(container)
    expect(within(menu).getByRole('menuitem', { name: '画質' })).toHaveAccessibleDescription(/h265/)
    expect(
      within(openSubmenu(container, '画質')).getByRole('menuitemradio', { checked: true }),
    ).toHaveTextContent('h265')
    expect(container.querySelector('video')?.src).toContain('profile=h265')
  })

  it('選択中プロファイルが encodedAssets から消えたら先頭へフォールバックする', () => {
    const { container, rerender } = render(
      <RecordingPlayer
        recordingId={40}
        encodedAssets={[
          { profile: 'h264', sizeBytes: 100 },
          { profile: 'h265', sizeBytes: 200 },
        ]}
      />,
    )
    selectProfile(container, 'h265')
    expect(container.querySelector('video')?.src).toContain('profile=h265')

    // 選択中だった h265 が資産一覧から消える（新しいエンコードが完了して古い
    // 派生物が消えた等）。h264_low を足して選択肢が 2 つのまま残る形にし、
    // 選択肢が引き続き出ることを確かめつつフォールバック先を見る。
    rerender(
      <RecordingPlayer
        recordingId={40}
        encodedAssets={[
          { profile: 'h264', sizeBytes: 100 },
          { profile: 'h264_low', sizeBytes: 50 },
        ]}
      />,
    )

    expect(container.querySelector('video')?.src).toContain('profile=h264')
    const options = within(openSubmenu(container, '画質')).getAllByRole('menuitemradio')
    expect(options.map((el) => [el.textContent, el.getAttribute('aria-checked')])).toEqual([
      ['h264100 B', 'true'],
      ['h264_low50 B', 'false'],
    ])
  })
})

// 再生設定は動画サービスの定石に寄せた行リスト。位置（歯車の真上・画面下のシート）は
// jsdom で測れないので web/e2e/chapters.mjs ⑦⑧ が見る。ここは役割とキーボードだけ。
describe('RecordingPlayer の設定メニュー（行リスト）', () => {
  const assets = [
    { profile: 'h264', sizeBytes: 100 },
    { profile: 'h265', sizeBytes: 200 },
  ]
  const chapters = [{ startMs: 30_000, endMs: 40_000, label: 'CM', cut: true }]

  it('行は CM を飛ばす / 字幕 / 再生速度 / 画質で、› の行は同じ枠の中身を差し替える', () => {
    const { container } = render(<RecordingPlayer recordingId={60} encodedAssets={assets} chapters={chapters} />)
    const menu = openPlaybackSettings(container)
    expect(menu).toHaveAttribute('role', 'menu')
    expect(
      Array.from(menu.querySelectorAll('[role^="menuitem"]'))
        // PiP 行は pictureInPictureEnabled が偽の jsdom では出ない。
        .map((el) => `${el.getAttribute('role')}:${el.getAttribute('aria-label') ?? el.textContent}`),
    ).toEqual([
      'menuitemcheckbox:CM を飛ばす',
      'menuitemcheckbox:字幕',
      'menuitem:再生速度',
      'menuitem:画質',
    ])
    expect(within(menu).getByRole('menuitem', { name: '再生速度' })).toHaveAccessibleDescription('標準')

    fireEvent.click(within(menu).getByRole('menuitem', { name: '画質' }))
    const quality = container.querySelector<HTMLElement>('[data-testid="playback-settings"]')!
    expect(quality).toBe(menu)
    expect(within(quality).queryByRole('menuitem', { name: '再生速度' })).toBeNull()
    expect(within(quality).getByRole('menuitem', { name: '戻る（画質）' })).toBeInTheDocument()
    fireEvent.click(within(quality).getByRole('menuitem', { name: '戻る（画質）' }))
    expect(within(menu).getByRole('menuitem', { name: '再生速度' })).toBeInTheDocument()
  })

  it('CM を飛ばすスイッチは端末の好みとして保存する', () => {
    const { container } = render(<RecordingPlayer recordingId={61} encodedAssets={assets} chapters={chapters} />)
    const toggle = within(openPlaybackSettings(container)).getByRole('menuitemcheckbox', { name: 'CM を飛ばす' })
    const before = toggle.getAttribute('aria-checked')
    fireEvent.click(toggle)
    expect(toggle).toHaveAttribute('aria-checked', before === 'true' ? 'false' : 'true')
  })

  it('矢印キーで行を移り、Esc で 1 段戻り、もう一度 Esc で閉じて歯車にフォーカスを戻す', async () => {
    const { container, getByRole } = render(
      <RecordingPlayer recordingId={62} encodedAssets={assets} chapters={chapters} />,
    )
    const menu = openPlaybackSettings(container)
    await waitFor(() => expect(document.activeElement).toBe(within(menu).getByRole('menuitemcheckbox', { name: 'CM を飛ばす' })))
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowDown' })
    expect(document.activeElement).toBe(within(menu).getByRole('menuitemcheckbox', { name: '字幕' }))
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowUp' })
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowUp' })
    expect(document.activeElement).toBe(within(menu).getByRole('menuitem', { name: '画質' }))
    // → で下の階層へ入ると選択中の選択肢にフォーカスが移る。
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowRight' })
    await waitFor(() => expect(document.activeElement).toHaveAttribute('aria-checked', 'true'))
    expect(document.activeElement).toHaveAttribute('role', 'menuitemradio')
    fireEvent.keyDown(document.activeElement!, { key: 'Escape' })
    await waitFor(() => expect(document.activeElement).toBe(within(menu).getByRole('menuitem', { name: '画質' })))
    fireEvent.keyDown(document.activeElement!, { key: 'Escape' })
    expect(container.querySelector('[data-testid="playback-settings"]')).toBeNull()
    expect(document.activeElement).toBe(getByRole('button', { name: '再生設定' }))
  })

  it('時刻の横にいまのチャプター名を出す（区間の隙間は本編）', () => {
    const { container, getByTestId } = render(
      <RecordingPlayer recordingId={63} encodedAssets={assets} chapters={chapters} />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 35, duration: 120 })
    fireEvent.timeUpdate(video)
    expect(getByTestId('playback-chapter')).toHaveTextContent('· CM')
    setMediaProps(video, { currentTime: 50, duration: 120 })
    fireEvent.timeUpdate(video)
    expect(getByTestId('playback-chapter')).toHaveTextContent('· 本編')
  })

  it('チャプター編集がある録画では、時刻の横のチャプター名からチャプター一覧を開く', () => {
    const { getByTestId } = render(
      <RecordingPlayer
        recordingId={65}
        encodedAssets={assets}
        chapters={chapters}
        chapterVersion="auto:1"
        onSaveChapters={vi.fn()}
        onResetChapters={vi.fn()}
      />,
    )
    const details = getByTestId('chapter-editor-details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    fireEvent.click(getByTestId('playback-chapter'))
    expect(details.open).toBe(true)
  })

  it('タッチで映像を叩くと操作を出すだけで、再生は中央のボタンで始める', () => {
    const { container } = render(<RecordingPlayer recordingId={64} encodedAssets={assets} />)
    const video = container.querySelector('video')!
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    fireEvent.pointerDown(video, { pointerType: 'touch' })
    fireEvent.click(video)
    expect(play).not.toHaveBeenCalled()
    fireEvent.pointerDown(video, { pointerType: 'mouse' })
    fireEvent.click(video)
    expect(play).toHaveBeenCalledOnce()
  })
})

describe('RecordingPlayer のシークプレビュー', () => {
  // タイルが 404 の録画でプレビューが出ないこと・ホバー位置の正しさは jsdom では
  // 測れない（getBoundingClientRect が 0 を返し、位置の計算まで進まない）。
  // web/e2e/seek-tiles.mjs の ①〜⑦ が実ブラウザで判定する。
  it('帯の上のマウス移動でタイル画像の問い合わせを始める（映像の上では始めない）', () => {
    const { container, getByTestId } = render(
      <RecordingPlayer recordingId={92} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 1800 })

    fireEvent.pointerMove(video, { pointerType: 'mouse', clientX: 100 })
    expect(container.querySelector('img[src*="/seek-tiles"]')).toBeNull()

    fireEvent.pointerMove(getByTestId('seek-scrub'), { pointerType: 'mouse', clientX: 100 })
    const probe = container.querySelector('img[src*="/seek-tiles"]')
    expect(probe?.getAttribute('src')).toBe('/api/media/recordings/92/seek-tiles')
  })

  it('録画を切り替えると帯の再生済み割合を前の録画から持ち越さない', () => {
    const asset = [{ profile: 'h264', sizeBytes: 123 }]
    const { container, getByTestId, rerender } = render(
      <RecordingPlayer recordingId={94} encodedAssets={asset} />,
    )
    const fill = () => (getByTestId('seek-scrub').firstElementChild!.firstElementChild as HTMLElement).style.width
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 100, currentTime: 40 })
    fireEvent.timeUpdate(video)
    expect(fill()).toBe('40%')

    // 親は key を付けないので同じインスタンスのまま録画だけが変わる。
    rerender(<RecordingPlayer recordingId={95} encodedAssets={asset} />)
    expect(fill()).toBe('0%')
  })

  it('タッチではタイルを取りに行かない（プレビューはマウスだけ）', () => {
    const { container, getByTestId } = render(
      <RecordingPlayer recordingId={93} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />,
    )
    setMediaProps(container.querySelector('video')!, { duration: 1800 })

    fireEvent.pointerMove(getByTestId('seek-scrub'), { pointerType: 'touch', clientX: 100 })
    expect(container.querySelector('img[src*="/seek-tiles"]')).toBeNull()
  })
})

describe('RecordingPlayer のチャプター', () => {
  const asset = [{ profile: 'h264', sizeBytes: 123 }]
  const cmSpan = { startMs: 10_000, endMs: 20_000, label: 'CM', cut: true }

  it('未取得の間は目盛りもチャプター一覧も出さず、編集 UI だけを出す', () => {
    // 目盛りと一覧は区間が要る（帯の位置は実寸に対する割合で決まる）。取得前の
    // 1 フレームに空の一覧を出さない。
    const { container, getByTestId } = render(
      <RecordingPlayer
        recordingId={96}
        encodedAssets={asset}
        chapterVersion="v1"
        onSaveChapters={() => Promise.resolve()}
        onResetChapters={() => {}}
      />,
    )
    expect(getByTestId('chapter-source')).not.toBeNull()
    expect(container.querySelectorAll('[data-testid="chapter-marker"]')).toHaveLength(0)
    expect(container.querySelector('[data-testid="chapter-navigation"]')).toBeNull()
  })

  it('版が未取得の間は編集 UI を出さない（下書きの基にする版が無い）', () => {
    const { queryByTestId } = render(
      <RecordingPlayer
        recordingId={95}
        encodedAssets={asset}
        onSaveChapters={() => Promise.resolve()}
        onResetChapters={() => {}}
      />,
    )
    expect(queryByTestId('chapter-source')).toBeNull()
  })

  it('チャプターがあるときはナビゲーションをシークバーに集め、編集は件数付きで畳む', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={100}
        encodedAssets={asset}
        chapters={[cmSpan]}
        chapterVersion="v1"
        onSaveChapters={() => Promise.resolve()}
        onResetChapters={() => {}}
      />,
    )
    // **目盛りの位置は `<video>.duration` を分母にする。** 確定する前に描くと
    // 割合が 0 除算になるので、duration が来るまで目盛りは出さない。
    expect(container.querySelector('[data-testid="chapter-marker"]')).toBeNull()
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 60, currentTime: 0 })
    fireEvent.loadedMetadata(video)
    expect(container.querySelector('[data-testid="chapter-navigation"]')).not.toBeNull()
    const details = container.querySelector<HTMLDetailsElement>('[data-testid="chapter-editor-details"]')!
    expect(details.open).toBe(false)
    expect(details.querySelector('summary')?.textContent).toBe('チャプター 1 件')
    expect(details.querySelector('summary')?.textContent).not.toMatch(/確認済み|未確認/)
    fireEvent.click(details.querySelector('summary')!)
    expect(details.open).toBe(true)
    expect(details.querySelector('input[aria-label="ラベル"]')).toHaveValue('CM')
    expect(details.textContent).toContain('切る')
    expect(container.querySelector('[data-testid="chapter-marker"]')).not.toBeNull()
  })

  it('4回のtimeupdateで再描画しても開いた編集detailsを保つ', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={101}
        encodedAssets={asset}
        chapters={[cmSpan]}
        chapterVersion="v1"
        onSaveChapters={() => Promise.resolve()}
        onResetChapters={() => {}}
      />,
    )
    const details = container.querySelector<HTMLDetailsElement>('[data-testid="chapter-editor-details"]')!
    fireEvent.click(details.querySelector('summary')!)
    const video = container.querySelector('video')!
    for (const seconds of [1, 2, 3, 4]) {
      setMediaProps(video, { currentTime: seconds })
      fireEvent.timeUpdate(video)
    }
    expect(container.querySelector('[data-testid="chapter-editor-details"]')).toBe(details)
    expect(details.open).toBe(true)
  })

  it('チャプター編集の境界行・区間行の時刻ボタンで再生位置が移る', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={102}
        encodedAssets={asset}
        chapters={[cmSpan]}
        chapterVersion="v1"
        onSaveChapters={() => Promise.resolve()}
        onResetChapters={() => {}}
      />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 60, currentTime: 0 })
    fireEvent.loadedMetadata(video)
    const details = container.querySelector<HTMLDetailsElement>('[data-testid="chapter-editor-details"]')!
    fireEvent.click(details.querySelector('summary')!)

    const boundary = details.querySelector('[data-testid="chapter-boundary"] button')!
    fireEvent.click(boundary)
    expect(video.currentTime).toBe(10)

    setMediaProps(video, { currentTime: 0 })
    const span = details.querySelector('[data-testid="chapter-span-row"] button')!
    fireEvent.click(span)
    expect(video.currentTime).toBe(10)
  })

  it('チャプター下書きを削除しても要約の件数は保存値のまま', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={103}
        encodedAssets={asset}
        chapters={[cmSpan]}
        chapterVersion="v1"
        onSaveChapters={() => Promise.resolve()}
        onResetChapters={() => {}}
      />,
    )
    const details = container.querySelector<HTMLDetailsElement>('[data-testid="chapter-editor-details"]')!
    fireEvent.click(details.querySelector('summary')!)
    const deleteDraft = Array.from(details.querySelectorAll('[data-testid="chapter-span-row"] button'))
      .find((button) => button.textContent === '削除')!
    fireEvent.click(deleteDraft)
    expect(details.querySelectorAll('[data-testid="chapter-span-row"]')).toHaveLength(0)
    expect(details.querySelector('summary')?.textContent).toBe('チャプター 1 件')
  })

  it('自動スキップは直前位置が区間の手前のときだけ飛ばす', () => {
    const { container } = render(
      <RecordingPlayer recordingId={97} encodedAssets={asset} chapters={[cmSpan]} />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 60, currentTime: 9.8, paused: false })
    fireEvent.timeUpdate(video)
    setMediaProps(video, { currentTime: 10.1, paused: false })
    fireEvent.timeUpdate(video)
    expect(video.currentTime).toBe(20)
  })

  it('シークで区間の中に入ったときは飛ばさない', () => {
    const { container } = render(
      <RecordingPlayer recordingId={98} encodedAssets={asset} chapters={[cmSpan]} />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 60, currentTime: 15, paused: false })
    fireEvent.seeked(video)
    setMediaProps(video, { currentTime: 15.1, paused: false })
    fireEvent.timeUpdate(video)
    expect(video.currentTime).toBe(15.1)
  })

  it('目盛りを区間の割合で描き、cut を testid の属性に出す', () => {
    const { container } = render(
      <RecordingPlayer recordingId={99} encodedAssets={asset} chapters={[cmSpan]} />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 40, currentTime: 0 })
    fireEvent.loadedMetadata(video)
    const marker = container.querySelector('[data-testid="chapter-marker"]') as HTMLElement
    expect(marker.getAttribute('data-cut')).toBe('true')
    expect(marker.style.left).toBe('25%')
    expect(marker.style.width).toBe('25%')
  })
})

describe('RecordingPlayer のカット版', () => {
  const chapters = [
    { startMs: 0, endMs: 15000, cut: true },
    { startMs: 60000, endMs: 75000, cut: true },
  ]

  it('cut の encoded を再生しているあいだは、チャプターの目盛り・一覧・編集 UI を出さない', () => {
    const { container, queryByText } = render(
      <RecordingPlayer
        recordingId={11}
        encodedAssets={[{ profile: 'cut', sizeBytes: 1, cut: true }]}
        chapters={chapters}
        chapterVersion="v1"
        onSaveChapters={async () => undefined}
        onResetChapters={() => undefined}
      />,
    )
    // duration を確定させても目盛りは出ない（「duration が来ていないだけ」と
    // 区別できるよう、対照のテストと同じ手順を踏む）。
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 1800, currentTime: 0 })
    fireEvent.loadedMetadata(video)
    expect(container.querySelectorAll('[data-testid="chapter-marker"]')).toHaveLength(0)
    // 編集 UI も出さない（境界は原本の ms で、カット版の軸には当てられない）。
    expect(container.querySelector('[data-testid="chapter-navigation"]')).toBeNull()
    expect(queryByText('前のチャプター')).toBeNull()
    expect(container.querySelector('[data-testid="chapter-source"]')).toBeNull()
  })

  it('cut でない encoded では同じ props でも チャプターを出す（対照）', () => {
    const { container, queryByRole } = render(
      <RecordingPlayer
        recordingId={12}
        encodedAssets={[{ profile: 'h264', sizeBytes: 1 }]}
        chapters={chapters}
        chapterVersion="v1"
        onSaveChapters={async () => undefined}
        onResetChapters={() => undefined}
      />,
    )
    const video = container.querySelector('video')!
    setMediaProps(video, { duration: 1800, currentTime: 0 })
    fireEvent.loadedMetadata(video)
    expect(container.querySelectorAll('[data-testid="chapter-marker"]')).toHaveLength(2)
    expect(queryByRole('button', { name: '前のチャプター' })).not.toBeNull()
  })

  it('cutStale のカット版にだけ「編集前の内容です」と作り直しを出す', () => {
    const onReencode = vi.fn()
    const { queryByText, rerender } = render(
      <RecordingPlayer
        recordingId={13}
        encodedAssets={[{ profile: 'cut', sizeBytes: 1, cut: true, cutStale: true }]}
        chapters={chapters}
        onReencode={onReencode}
      />,
    )
    expect(queryByText(/編集前の内容です/)).not.toBeNull()
    fireEvent.click(queryByText('作り直す')!)
    expect(onReencode).toHaveBeenCalledWith('cut')

    // 一致していれば（cutStale が偽なら）バナーを出さない。
    rerender(
      <RecordingPlayer
        recordingId={13}
        encodedAssets={[{ profile: 'cut', sizeBytes: 1, cut: true, cutStale: false }]}
        chapters={chapters}
        onReencode={onReencode}
      />,
    )
    expect(queryByText(/編集前の内容です/)).toBeNull()
  })
})
