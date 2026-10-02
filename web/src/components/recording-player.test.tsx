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
      fireEvent.change(container.querySelector('select')!, { target: { value: profile } })
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
    // 原本 32 s は新世代の keep 外（25-35 の中なので 7 s 目 = カット版 7 s）。
    expect(newVideo.currentTime).toBe(7)

    fetchMock.mockClear()
    setMediaProps(newVideo, { currentTime: 12, duration: 30 })
    fireEvent.pause(newVideo)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    // 旧世代の表なら 32_000。新世代の表: 12 s = 2 つ目の keep の 2 s 目 = 原本 62 s。
    expect(fetchMock.mock.calls[0]![1]).toEqual(expect.objectContaining({ body: JSON.stringify({ positionMs: 62_000 }) }))
  })
})

// issue #236（M7-3）: 押す前にサイズが見える値札。プロファイルセレクタの
// 各選択肢・ダウンロードリンク・VLC リンクに常置し、サイズが取れない資産でも
// 選択肢そのものは隠さない（サイズだけ省く）ことを両方向で確認する。
describe('RecordingPlayer のサイズ常置（値札、issue #236）', () => {
  it('プロファイルが 1 つのとき、サイズ付きのキャプションを出す', () => {
    const { container } = render(
      <RecordingPlayer recordingId={20} encodedAssets={[{ profile: 'h264', sizeBytes: 1_200_000 }]} />,
    )
    expect(container.textContent).toContain('h264 (1.1 MB)')
  })

  it('プロファイルが 1 つで sizeBytes が省略されているとき、プロファイル名は出すがサイズは出さない（隠さない）', () => {
    const { container } = render(
      <RecordingPlayer recordingId={21} encodedAssets={[{ profile: 'h264' }]} />,
    )
    // 選択肢（プロファイル名）自体は必ず出る --- サイズが取れないことを理由に
    // 隠すと「機能しないコントロールは置かない」の逆（機能するコントロールを
    // 隠す）になる。
    expect(container.textContent).toContain('h264')
    expect(container.textContent).not.toMatch(/\d+(\.\d+)? (B|KB|MB|GB|TB)/)
    // video 自体は描かれる（プレイヤーが消えたわけではない）
    expect(container.querySelector('video')).not.toBeNull()
  })

  it('プロファイルが複数のとき、各 <option> にサイズを付ける。サイズが無いものは名前だけになる', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={22}
        encodedAssets={[
          { profile: 'h264', sizeBytes: 500_000_000 },
          { profile: 'h265' },
        ]}
      />,
    )
    const options = Array.from(container.querySelector('select')!.querySelectorAll('option'))
    expect(options.map((o) => o.textContent)).toEqual(['h264 (476.8 MB)', 'h265'])
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

describe('RecordingPlayer の encoded ダウンロード', () => {
  it('複数プロファイルでは選択中プロファイルの URL とファイル名に追従する', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={26}
        encodedAssets={[
          { profile: 'h264', sizeBytes: 100 },
          { profile: 'h265', sizeBytes: 200 },
        ]}
      />,
    )
    const link = container.querySelector('a[aria-label="encoded 動画をダウンロード"]')!

    expect(link).toHaveAttribute('href', '/api/media/recordings/26/file?profile=h264')
    expect(link).toHaveAttribute('download', 'recording-26-h264.mp4')

    fireEvent.change(container.querySelector('select')!, { target: { value: 'h265' } })

    expect(link).toHaveAttribute('href', '/api/media/recordings/26/file?profile=h265')
    expect(link).toHaveAttribute('download', 'recording-26-h265.mp4')
  })

  it('単一プロファイルでもサイズ表示の隣にダウンロードリンクを出す', () => {
    const { container } = render(
      <RecordingPlayer recordingId={27} encodedAssets={[{ profile: 'h264', sizeBytes: 1_200_000 }]} />,
    )
    const link = container.querySelector('a[aria-label="encoded 動画をダウンロード"]')!

    expect(link).toHaveAttribute('href', '/api/media/recordings/27/file?profile=h264')
    expect(link).toHaveAttribute('download', 'recording-27-h264.mp4')
    expect(link).toHaveTextContent('ダウンロード')
    expect(link).not.toHaveTextContent('1.1 MB')
    expect(container.textContent).toContain('h264 (1.1 MB)')
  })

  it('encoded 用リンクと原本 TS リンクを同時に出す', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={28}
        encodedAssets={[{ profile: 'h264', sizeBytes: 100 }]}
        hasOriginal
      />,
    )

    expect(container.querySelector('a[aria-label="encoded 動画をダウンロード"]')).toBeInTheDocument()
    expect(container.querySelector('a[href="/api/media/recordings/28/file"]')).toBeNull()
    expect(container.querySelectorAll('a')).toHaveLength(1)
  })
})

describe('RecordingPlayer の再生操作', () => {
  const asset = [{ profile: 'h264', sizeBytes: 123 }]

  it('再生速度と PiP はブラウザ controls に任せ、自前の重複操作を出さない', () => {
    const { container, queryByLabelText, queryByRole } = render(
      <RecordingPlayer recordingId={30} encodedAssets={asset} />,
    )

    expect(container.querySelector('video')).toHaveProperty('controls', true)
    expect(queryByLabelText('再生速度')).not.toBeInTheDocument()
    expect(queryByRole('button', { name: 'ピクチャーインピクチャー' })).not.toBeInTheDocument()
  })

  // 速度は「この録画をどう見るか」ではなく「自分がどう見るか」の好みなので、
  // ブラウザ controls の ratechange から保存し、録画をまたいでも保つ
  // （docs/frontend/design.md §個人化）。
  it('ブラウザ controls で選んだ速度を保存し、別の録画でも video に適用する', () => {
    const { container, rerender } = render(
      <RecordingPlayer recordingId={30} encodedAssets={asset} />,
    )
    let video = container.querySelector('video')!
    video.playbackRate = 1.5
    fireEvent.rateChange(video)

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

  it('入力欄と video からのキー操作は無視し、それ以外では処理する', () => {
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
    expect(video.currentTime).toBe(50)
    fireEvent.keyDown(window, { key: 'ArrowRight' })
    expect(video.currentTime).toBe(60)
  })

  it('Space で再生し、M でミュートし、F でフルスクリーンにする', () => {
    const { container } = render(<RecordingPlayer recordingId={34} encodedAssets={asset} />)
    const video = container.querySelector('video')!
    const play = vi.spyOn(video, 'play').mockResolvedValue()
    const requestFullscreen = vi.fn(() => Promise.resolve())
    Object.defineProperty(video, 'requestFullscreen', { value: requestFullscreen })

    fireEvent.keyDown(window, { key: ' ' })
    fireEvent.keyDown(window, { key: 'm' })
    fireEvent.keyDown(window, { key: 'F' })

    expect(play).toHaveBeenCalledOnce()
    expect(video.muted).toBe(true)
    expect(requestFullscreen).toHaveBeenCalledOnce()
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

    expect((container.querySelector('select') as HTMLSelectElement).value).toBe('h265')
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
    const select = container.querySelector('select')! as HTMLSelectElement
    fireEvent.change(select, { target: { value: 'h265' } })
    expect(select.value).toBe('h265')
    expect(container.querySelector('video')?.src).toContain('profile=h265')

    // 選択中だった h265 が資産一覧から消える（新しいエンコードが完了して古い
    // 派生物が消えた等）。h264_low を足して選択肢が 2 つのまま残る形にし、
    // <select> 自体が引き続き出ることを確かめつつフォールバック先を見る。
    rerender(
      <RecordingPlayer
        recordingId={40}
        encodedAssets={[
          { profile: 'h264', sizeBytes: 100 },
          { profile: 'h264_low', sizeBytes: 50 },
        ]}
      />,
    )

    expect(select.value).toBe('h264')
    expect(container.querySelector('video')?.src).toContain('profile=h264')
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
    expect(container.querySelector('[aria-label="チャプター"]')).toBeNull()
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

  it('チャプターがあるときはナビゲーションを外に置き、編集は件数付きで畳む', () => {
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
    expect(container.querySelector('[aria-label="チャプター"]')).toBeNull()
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

  it('チャプター要約の件数は保存値から数える（下書きの区間削除では変わらない）', () => {
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
    fireEvent.click(within(details).getByRole('button', { name: '削除' }))
    expect(details.querySelector('[data-testid="chapter-span-row"]')).toBeNull()
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
    expect(container.querySelector('[aria-label="チャプター"]')).toBeNull()
    // 編集 UI も出さない（境界は原本の ms で、カット版の軸には当てられない）。
    expect(queryByText('前のチャプター')).toBeNull()
    expect(container.querySelector('[data-testid="chapter-source"]')).toBeNull()
  })

  it('cut でない encoded では同じ props でも チャプターを出す（対照）', () => {
    const { container, queryByText } = render(
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
    expect(queryByText('前のチャプター')).not.toBeNull()
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
