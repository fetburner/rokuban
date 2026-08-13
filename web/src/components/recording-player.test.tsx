import { fireEvent, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { RecordingPlayer } from '@/components/recording-player'

afterEach(() => {
  localStorage.clear()
  vi.restoreAllMocks()
})

/** jsdom の video 要素は currentTime/duration の実再生をしないので、テスト側から直接設定する。 */
function setMediaProps(video: HTMLVideoElement, props: { currentTime?: number; duration?: number }) {
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

describe('RecordingPlayer の timeupdate 間引き', () => {
  it('同一秒内の連続した timeupdate で localStorage への書き込みが 1 回に収まる', () => {
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem')
    const { container } = render(<RecordingPlayer recordingId={7} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />)
    const video = container.querySelector('video')!

    // 4Hz 相当（0.25 秒間隔）で同一秒内に 4 回発火させる
    for (const t of [10.0, 10.25, 10.5, 10.75]) {
      setMediaProps(video, { currentTime: t, duration: 300 })
      fireEvent.timeUpdate(video)
    }

    expect(setItemSpy).toHaveBeenCalledTimes(1)
  })

  it('秒をまたぐと再び書き込む', () => {
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem')
    const { container } = render(<RecordingPlayer recordingId={8} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />)
    const video = container.querySelector('video')!

    for (const t of [10.0, 10.5, 11.0, 11.5]) {
      setMediaProps(video, { currentTime: t, duration: 300 })
      fireEvent.timeUpdate(video)
    }

    expect(setItemSpy).toHaveBeenCalledTimes(2)
  })

  it('pause は間引かず必ず保存する', () => {
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem')
    const { container } = render(<RecordingPlayer recordingId={9} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />)
    const video = container.querySelector('video')!

    // timeupdate で 10 秒として 1 回保存済みの状態にする
    setMediaProps(video, { currentTime: 10.0, duration: 300 })
    fireEvent.timeUpdate(video)
    expect(setItemSpy).toHaveBeenCalledTimes(1)

    // 同じ秒のまま pause しても、間引かずに必ず書き込む
    setMediaProps(video, { currentTime: 10.2, duration: 300 })
    fireEvent.pause(video)
    expect(setItemSpy).toHaveBeenCalledTimes(2)
  })

  it('終端付近で pause すると removeItem が走る（続きから再開しない）', () => {
    const removeItemSpy = vi.spyOn(Storage.prototype, 'removeItem')
    const { container } = render(<RecordingPlayer recordingId={10} encodedAssets={[{ profile: 'h264', sizeBytes: 123 }]} />)
    const video = container.querySelector('video')!

    setMediaProps(video, { currentTime: 296, duration: 300 })
    fireEvent.pause(video)

    expect(removeItemSpy).toHaveBeenCalledWith('rokuban:playback:10:h264')
  })

  it('プロファイル切替で間引き状態がリセットされる（旧プロファイルの秒を引きずらない）', () => {
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem')
    const { container } = render(
      <RecordingPlayer
        recordingId={11}
        encodedAssets={[
          { profile: 'h264', sizeBytes: 123 },
          { profile: 'h265', sizeBytes: 456 },
        ]}
      />,
    )

    let video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 10.0, duration: 300 })
    fireEvent.timeUpdate(video)
    expect(setItemSpy).toHaveBeenCalledTimes(1)

    // h265 に切り替える（video 要素は key 変更で作り直される）
    fireEvent.change(container.querySelector('select')!, { target: { value: 'h265' } })

    // 切替後の video で同じ 10 秒台の timeupdate が来ても、リセットされていれば保存する
    video = container.querySelector('video')!
    setMediaProps(video, { currentTime: 10.0, duration: 300 })
    fireEvent.timeUpdate(video)

    expect(setItemSpy).toHaveBeenCalledTimes(2)
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
    const options = Array.from(container.querySelectorAll('option'))
    expect(options.map((o) => o.textContent)).toEqual(['h264 (476.8 MB)', 'h265'])
  })

  it('encoded が無く原本のみのとき、VLC リンクに原本サイズを付ける', () => {
    const { container } = render(
      <RecordingPlayer recordingId={23} encodedAssets={[]} hasOriginal originalSizeBytes={3_000_000_000} />,
    )
    const link = container.querySelector('a')!
    expect(link.textContent).toContain('VLC 等で開く (2.8 GB)')
  })

  it('encoded が無く原本のみで originalSizeBytes が省略されているとき、リンクは出すがサイズは出さない', () => {
    const { container } = render(
      <RecordingPlayer recordingId={24} encodedAssets={[]} hasOriginal />,
    )
    const link = container.querySelector('a')!
    expect(link.textContent).toBe('VLC 等で開く')
  })

  it('encoded があり原本もあるとき、ダウンロード / VLC リンクに原本サイズを付ける', () => {
    const { container } = render(
      <RecordingPlayer
        recordingId={25}
        encodedAssets={[{ profile: 'h264', sizeBytes: 100 }]}
        hasOriginal
        originalSizeBytes={4_500_000_000}
      />,
    )
    const link = container.querySelector('a')!
    expect(link.textContent).toContain('ダウンロード / VLC (4.2 GB)')
  })
})
