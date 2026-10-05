import { useState } from 'react'

import { recordingThumbnailURL } from '@/lib/recording-media'
import { cn } from '@/lib/utils'

/**
 * RecordingThumbnail は録画のサムネイルを 16:9 の枠に出す。未生成・404 は静かにプレースホルダーへ落とす。
 * worker が SAR を補正した画像に合わせて 16:9 にする。寸法（幅）は呼び出し側が className で渡す。
 * 録画 id が変わったら失敗状態を捨てるため、呼び出し側は `key` に id を渡す。
 */
export function RecordingThumbnail({ recordingId, className }: { recordingId: number; className: string }) {
  const [failed, setFailed] = useState(false)

  return (
    <div className={cn('aspect-video shrink-0 overflow-hidden rounded bg-muted', className)}>
      {!failed ? (
        <img
          src={recordingThumbnailURL(recordingId)}
          alt=""
          className="size-full object-cover"
          loading="lazy"
          onError={() => setFailed(true)}
        />
      ) : (
        <div className="size-full bg-muted" aria-hidden />
      )}
    </div>
  )
}
