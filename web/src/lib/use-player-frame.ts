import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FocusEvent as ReactFocusEvent,
  type MouseEvent as ReactMouseEvent,
  type PointerEvent as ReactPointerEvent,
  type RefObject,
} from 'react'

type PlayerFrameOptions = {
  fullscreenContainerRef?: RefObject<HTMLElement | null>
  onSeekBy?: (seconds: number) => void
  onSeekToFraction?: (fraction: number) => boolean
  onSavePosition?: (video: HTMLVideoElement, keepalive?: boolean) => void
  savePositionKey?: unknown
  getSubtitleTracks?: (video: HTMLVideoElement) => readonly TextTrack[]
  subtitleState?: unknown
}

/**
 * usePlayerFrame は自前の操作バーを持つプレイヤー枠（`RecordingPlaybackControls`）の状態と
 * 共通操作をまとめる。encoded の `RecordingPlayer` と HLS の `LivePlayer` が同じ実装を使い、
 * バーの自動非表示・フォーカス・映像のタップ・キー・全画面・字幕 cue の位置・PiP・位置保存の
 * pagehide/定期処理を揃える。
 *
 * `frameRef` は自分の枠（`RecordingPlaybackControls` の枠）で、設定メニューの探索に使う。
 * `fullscreenContainerRef` は全画面にする要素で、省略すると枠自身を全画面にする。
 * 再生元が替わっても残る親の要素を渡すと、プレイヤーが作り直されても全画面が解除されない。
 * シークの意味と字幕 track の選択は再生元から渡す。
 * `videoKey` は `<video>` 要素が作り直される単位（PiP のイベントを張り直す）。
 * 戻り値の `controls` は `RecordingPlaybackControls` に、`video` は `<video>` にそのまま渡す。
 * `onPlay` / `onPause` / `onVolumeChange` は呼び出し側が自分のハンドラから呼ぶ。
 */
export function usePlayerFrame(
  videoRef: RefObject<HTMLVideoElement | null>,
  frameRef: RefObject<HTMLDivElement | null>,
  videoKey: unknown,
  options: PlayerFrameOptions = {},
) {
  const {
    fullscreenContainerRef,
    onSeekBy,
    onSeekToFraction,
    onSavePosition,
    savePositionKey,
    getSubtitleTracks,
    subtitleState,
  } = options
  const hasSavePosition = onSavePosition !== undefined
  const controlsTimerRef = useRef<number | undefined>(undefined)
  // 映像を押したポインタの種類。タッチは再生 / 一時停止ではなく操作の表示に使う（スマホの定石）。
  const videoPointerTypeRef = useRef('')
  const [mediaPlaying, setMediaPlaying] = useState(false)
  const [muted, setMuted] = useState(false)
  const [volume, setVolume] = useState(1)
  const [pictureInPicture, setPictureInPicture] = useState(false)
  const [isFullscreen, setIsFullscreen] = useState(false)
  const [controlsVisible, setControlsVisible] = useState(true)
  const [toolbarFocused, setToolbarFocused] = useState(false)
  const seekByRef = useRef(onSeekBy)
  const seekToFractionRef = useRef(onSeekToFraction)
  const savePositionRef = useRef(onSavePosition)
  const getSubtitleTracksRef = useRef(getSubtitleTracks)
  const originalSubtitleLinesRef = useRef(new WeakMap<VTTCue, VTTCue['line']>())

  useEffect(() => {
    seekByRef.current = onSeekBy
    seekToFractionRef.current = onSeekToFraction
    savePositionRef.current = onSavePosition
    getSubtitleTracksRef.current = getSubtitleTracks
  }, [getSubtitleTracks, onSavePosition, onSeekBy, onSeekToFraction])

  const hideLater = () => {
    controlsTimerRef.current = window.setTimeout(() => setControlsVisible(false), 3000)
  }
  const handleControlsActivity = () => {
    setControlsVisible(true)
    window.clearTimeout(controlsTimerRef.current)
    if (!mediaPlaying || toolbarFocused) return
    hideLater()
  }
  // バーを出したままにするのはキーボードフォーカス（:focus-visible）だけ。
  // マウスで押したボタンに残ったフォーカスで出しっぱなしにすると、再生中ずっと映像に被る。
  const handleToolbarFocus = (event: ReactFocusEvent<HTMLElement>) => {
    let keyboard = false
    try {
      keyboard = (event.target as Element).matches(':focus-visible')
    } catch {
      keyboard = false
    }
    if (!keyboard) return
    setToolbarFocused(true)
    setControlsVisible(true)
    window.clearTimeout(controlsTimerRef.current)
  }
  const handleToolbarBlur = (event: ReactFocusEvent<HTMLElement>) => {
    const relatedTarget = event.relatedTarget
    const shell = event.currentTarget.closest('[data-testid="recording-player-shell"]')
    const toolbar = shell?.querySelector('[data-testid="player-controls"]')
    const settings = shell?.querySelector('[data-player-popover]')
    if (
      relatedTarget instanceof Node &&
      (toolbar?.contains(relatedTarget) || settings?.contains(relatedTarget))
    ) return
    // マウスで押したボタンの blur（隠れたバーの inert でフォーカスが落ちるときも来る）で
    // 再表示しない。キーボードフォーカスを離れたときだけ、隠すタイマーを張り直す。
    if (!toolbarFocused) return
    setToolbarFocused(false)
    handleControlsActivity()
  }
  // ページのキー操作（F）の effect からも呼ぶので参照を固定する。
  const requestFullscreen = useCallback(() => {
    const container = (fullscreenContainerRef ?? frameRef).current
    if (document.fullscreenElement === container) {
      void document.exitFullscreen?.().catch(() => {})
      return
    }
    if (container?.requestFullscreen) {
      void container.requestFullscreen().catch(() => {})
      return
    }
    const video = videoRef.current as (HTMLVideoElement & { webkitEnterFullscreen?: () => void }) | null
    video?.webkitEnterFullscreen?.()
  }, [frameRef, fullscreenContainerRef, videoRef])
  const togglePlay = () => {
    const video = videoRef.current
    if (!video) return
    if (video.paused) void video.play().catch(() => {})
    else video.pause()
  }

  useEffect(() => {
    const onFullscreenChange = () => setIsFullscreen(document.fullscreenElement === (fullscreenContainerRef ?? frameRef).current)
    document.addEventListener('fullscreenchange', onFullscreenChange)
    // 共有コンテナがすでに全画面のときに別のプレイヤーがマウントされることがある。
    // oxlint-disable-next-line react/set-state-in-effect -- 新しいプレイヤーをブラウザの全画面状態に合わせる。
    onFullscreenChange()
    return () => document.removeEventListener('fullscreenchange', onFullscreenChange)
  }, [frameRef, fullscreenContainerRef])

  useEffect(() => {
    const video = videoRef.current
    if (!video) return
    const onPictureInPictureChange = () => setPictureInPicture(document.pictureInPictureElement === video)
    video.addEventListener('enterpictureinpicture', onPictureInPictureChange)
    video.addEventListener('leavepictureinpicture', onPictureInPictureChange)
    return () => {
      video.removeEventListener('enterpictureinpicture', onPictureInPictureChange)
      video.removeEventListener('leavepictureinpicture', onPictureInPictureChange)
    }
  }, [videoRef, videoKey])

  useEffect(() => {
    const saveIfPlaying = () => {
      const video = videoRef.current
      if (video && !video.paused) savePositionRef.current?.(video)
    }
    const onPageHide = () => {
      const video = videoRef.current
      if (video) savePositionRef.current?.(video, true)
    }
    if (!savePositionRef.current) return
    const timer = window.setInterval(saveIfPlaying, 15_000)
    window.addEventListener('pagehide', onPageHide)
    return () => {
      window.clearInterval(timer)
      window.removeEventListener('pagehide', onPageHide)
    }
  }, [hasSavePosition, savePositionKey, videoKey, videoRef])

  useEffect(() => {
    const video = videoRef.current
    const frame = frameRef.current
    if (!video || !frame || !getSubtitleTracks) return
    const controls = frame.querySelector<HTMLElement>('[data-testid="player-controls-bottom"]')
    const boundTracks = new Set<TextTrack>()
    const lineHeight = () => frame.getBoundingClientRect().height * 0.05
    const restoreCue = (cue: VTTCue) => {
      const original = originalSubtitleLinesRef.current.get(cue)
      if (original !== undefined) cue.line = original
    }
    const update = () => {
      const controlsHeight = controls?.getBoundingClientRect().height ?? 0
      const height = lineHeight()
      const raisedLine = height > 0
        ? -(Math.ceil((controlsHeight + 8) / height) + 1)
        : -5
      for (const track of getSubtitleTracksRef.current?.(video) ?? []) {
        for (const rawCue of Array.from(track.cues ?? [])) {
          const cue = rawCue as VTTCue
          if (controlsVisible && track.mode !== 'disabled') {
            if (!originalSubtitleLinesRef.current.has(cue)) originalSubtitleLinesRef.current.set(cue, cue.line)
            cue.line = raisedLine
          } else {
            restoreCue(cue)
          }
        }
      }
    }
    const onCueChange = () => update()
    const bindTrack = (track: TextTrack) => {
      if (
        boundTracks.has(track) ||
        typeof track.addEventListener !== 'function' ||
        !(getSubtitleTracksRef.current?.(video) ?? []).includes(track)
      ) return
      boundTracks.add(track)
      track.addEventListener('cuechange', onCueChange)
    }
    const onTrackAdded = (event: Event) => {
      const track = (event as TrackEvent).track
      if (track) bindTrack(track)
      update()
    }
    const textTracks = video.textTracks
    const canObserveTracks = typeof textTracks.addEventListener === 'function'
    for (const track of Array.from(textTracks)) bindTrack(track)
    if (canObserveTracks) textTracks.addEventListener('addtrack', onTrackAdded)
    const trackElements = Array.from(video.querySelectorAll<HTMLTrackElement>('track'))
    trackElements.forEach((track) => track.addEventListener('load', update))
    const observer = controls && typeof ResizeObserver !== 'undefined'
      ? new ResizeObserver(update)
      : null
    observer?.observe(frame)
    if (controls) observer?.observe(controls)
    update()
    return () => {
      for (const track of boundTracks) track.removeEventListener('cuechange', onCueChange)
      if (canObserveTracks) textTracks.removeEventListener('addtrack', onTrackAdded)
      trackElements.forEach((track) => track.removeEventListener('load', update))
      observer?.disconnect()
    }
  }, [controlsVisible, frameRef, getSubtitleTracks, subtitleState, videoKey, videoRef])

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const video = videoRef.current
      if (!video || event.ctrlKey || event.metaKey || event.altKey) return
      if (
        event.target instanceof Element &&
        event.target.closest('input, textarea, select, button, a, [role="slider"], [contenteditable]')
      ) return

      const key = event.key.toLowerCase()
      let handled = true
      switch (key) {
        case ' ':
          if (video.paused) void video.play().catch(() => {})
          else video.pause()
          break
        case 'arrowleft':
          seekByRef.current?.(-10)
          handled = seekByRef.current !== undefined
          break
        case 'arrowright':
          seekByRef.current?.(10)
          handled = seekByRef.current !== undefined
          break
        case 'j':
          seekByRef.current?.(-30)
          handled = seekByRef.current !== undefined
          break
        case 'l':
          seekByRef.current?.(30)
          handled = seekByRef.current !== undefined
          break
        case 'm':
          video.muted = !video.muted
          break
        case 'f':
          requestFullscreen()
          break
        default:
          if (/^[0-9]$/.test(key)) {
            handled = seekToFractionRef.current?.(Number(key) / 10) ?? false
          } else {
            handled = false
          }
      }
      if (handled) event.preventDefault()
    }

    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [requestFullscreen, videoRef])

  useEffect(() => () => window.clearTimeout(controlsTimerRef.current), [])

  return {
    mediaPlaying,
    /** `load()` は pause を発火しないので、要素を張り直す側が再生状態を明示的に戻す。 */
    setMediaPlaying,
    /** 生の表示状態（字幕の cue を上げる高さの計算に使う）。 */
    controlsVisible,
    requestFullscreen,
    handleControlsActivity,
    /** 要素の現在値を state に写す（loadedmetadata）。 */
    syncFromVideo: (video: HTMLVideoElement) => {
      setMediaPlaying(!video.paused)
      setMuted(video.muted)
      setVolume(video.volume)
      setPictureInPicture(document.pictureInPictureElement === video)
    },
    onPlay: () => {
      setMediaPlaying(true)
      setControlsVisible(true)
      window.clearTimeout(controlsTimerRef.current)
      if (!toolbarFocused) hideLater()
    },
    onPause: () => {
      setMediaPlaying(false)
      setControlsVisible(true)
      window.clearTimeout(controlsTimerRef.current)
    },
    onVolumeChange: (video: HTMLVideoElement) => {
      setMuted(video.muted)
      setVolume(video.volume)
    },
    /** `<video>` に渡す。タッチでは映像のタップで操作を出す（再生 / 一時停止は中央のボタン）。 */
    video: {
      tabIndex: 0,
      onPointerDown: (event: ReactPointerEvent<HTMLVideoElement>) => {
        videoPointerTypeRef.current = event.pointerType
      },
      onClick: (event: ReactMouseEvent<HTMLVideoElement>) => {
        if (videoPointerTypeRef.current === 'touch') {
          videoPointerTypeRef.current = ''
          handleControlsActivity()
          return
        }
        if (event.currentTarget.paused) void event.currentTarget.play().catch(() => {})
        else event.currentTarget.pause()
      },
    },
    /** `RecordingPlaybackControls` に渡す枠の状態と操作。 */
    controls: {
      frameRef,
      isPlaying: mediaPlaying,
      muted,
      volume,
      pictureInPicture,
      isFullscreen,
      controlsVisible: controlsVisible || !mediaPlaying || toolbarFocused,
      onTogglePlay: togglePlay,
      onToggleMute: () => {
        const video = videoRef.current
        if (!video) return
        video.muted = !video.muted
        setMuted(video.muted)
      },
      onVolumeChange: (nextVolume: number) => {
        const video = videoRef.current
        if (!video) return
        video.volume = nextVolume
        video.muted = nextVolume === 0
        setVolume(video.volume)
        setMuted(video.muted)
      },
      onTogglePictureInPicture: () => {
        const video = videoRef.current
        if (!video || !document.pictureInPictureEnabled) return
        if (document.pictureInPictureElement === video) {
          void document.exitPictureInPicture?.().catch(() => {})
          return
        }
        void video.requestPictureInPicture?.().catch(() => {})
      },
      onToggleFullscreen: requestFullscreen,
      onControlsActivity: handleControlsActivity,
      onHideControls: () => {
        window.clearTimeout(controlsTimerRef.current)
        if (mediaPlaying) setControlsVisible(false)
      },
      onToolbarFocus: handleToolbarFocus,
      onToolbarBlur: handleToolbarBlur,
      onShellKeyDown: handleControlsActivity,
    },
  }
}
