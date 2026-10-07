/**
 * ThumbnailOverlay はヒーローのサムネイル画像の上に載る局名ラベルと進み線。
 * 画像の上に載る幕なので、ライト/ダークで変わってはいけない黒を使う
 * （scripts/check-colors.mjs の ALLOW に理由付きで登録）。
 */
export function ThumbnailOverlay({
  serviceName,
  progress,
}: {
  serviceName: string
  progress?: number
}) {
  return (
    <>
      <span
        data-testid="home-hero-station"
        className="absolute bottom-2 left-2 rounded bg-black/55 px-1 text-xs text-white"
      >
        {serviceName}
      </span>
      {progress !== undefined && <ThumbnailProgressLine progress={progress} testId="home-hero-progress-line" />}
    </>
  )
}

/**
 * ThumbnailProgressLine はサムネイルの下端に重ねる視聴の進み線（0〜100%）。
 * ホームのヒーローと録画詳細のシリーズ棚が同じ線を使う。親は `relative` にする。
 */
export function ThumbnailProgressLine({ progress, testId }: { progress: number; testId: string }) {
  return (
    <div aria-hidden data-testid={testId} className="absolute inset-x-0 bottom-0 h-1 bg-black/25">
      <div className="h-full bg-foreground" style={{ width: `${progress}%` }} />
    </div>
  )
}
