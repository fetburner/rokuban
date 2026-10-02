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

/**
 * usePlayerFrame は自前の操作バーを持つプレイヤー枠（`RecordingPlaybackControls`）の状態と
 * 操作をまとめる。encoded の `RecordingPlayer` と原本 HLS の `LivePlayer` が同じ実装を使い、
 * バーの自動非表示・フォーカス・映像のタップ・全画面・PiP が片方だけ食い違わないようにする。
 *
 * `videoKey` は `<video>` 要素が作り直される単位（PiP のイベントを張り直す）。
 * 戻り値の `controls` は `RecordingPlaybackControls` に、`video` は `<video>` にそのまま渡す。
 * `onPlay` / `onPause` / `onVolumeChange` は呼び出し側が自分のハンドラから呼ぶ。
 */
export function usePlayerFrame(
  videoRef: RefObject<HTMLVideoElement | null>,
  fullscreenRef: RefObject<HTMLDivElement | null>,
  videoKey: unknown,
) {
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
    const container = fullscreenRef.current
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
  }, [fullscreenRef, videoRef])
  const togglePlay = () => {
    const video = videoRef.current
    if (!video) return
    if (video.paused) void video.play().catch(() => {})
    else video.pause()
  }

  useEffect(() => {
    const onFullscreenChange = () => setIsFullscreen(document.fullscreenElement === fullscreenRef.current)
    document.addEventListener('fullscreenchange', onFullscreenChange)
    return () => document.removeEventListener('fullscreenchange', onFullscreenChange)
  }, [fullscreenRef])

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
      fullscreenRef,
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
