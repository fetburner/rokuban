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
        className="absolute bottom-2 left-2 rounded bg-black/55 px-1 text-[11px] text-white"
      >
        {serviceName}
      </span>
      {progress !== undefined && (
        <div
          aria-hidden
          data-testid="home-hero-progress-line"
          className="absolute inset-x-0 bottom-0 h-1 bg-black/25"
        >
          <div className="h-full bg-foreground" style={{ width: `${progress}%` }} />
        </div>
      )}
    </>
  )
}
