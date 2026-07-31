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
    const { container } = render(<RecordingPlayer recordingId={7} encodedProfiles={['h264']} />)
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
    const { container } = render(<RecordingPlayer recordingId={8} encodedProfiles={['h264']} />)
    const video = container.querySelector('video')!

    for (const t of [10.0, 10.5, 11.0, 11.5]) {
      setMediaProps(video, { currentTime: t, duration: 300 })
      fireEvent.timeUpdate(video)
    }

    expect(setItemSpy).toHaveBeenCalledTimes(2)
  })

  it('pause は間引かず必ず保存する', () => {
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem')
    const { container } = render(<RecordingPlayer recordingId={9} encodedProfiles={['h264']} />)
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
    const { container } = render(<RecordingPlayer recordingId={10} encodedProfiles={['h264']} />)
    const video = container.querySelector('video')!

    setMediaProps(video, { currentTime: 296, duration: 300 })
    fireEvent.pause(video)

    expect(removeItemSpy).toHaveBeenCalledWith('rokuban:playback:10:h264')
  })
})
