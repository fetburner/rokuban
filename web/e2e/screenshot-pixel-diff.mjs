/**
 * changedPixelBounds は二枚の同じページのスクリーンショットから、描画が変わった画素の矩形を返す。
 * 字幕の cue が native HLS でも実際に描画され、操作バーより上に見えるかを
 * `subtitles.mjs` と `recording-original-vod.mjs` から検証する。
 */
export async function changedPixelBounds(page, before, after, threshold = 40) {
  return page.evaluate(async ({ beforeBase64, afterBase64, threshold }) => {
    const read = async (base64) => {
      const image = new Image()
      image.src = `data:image/png;base64,${base64}`
      await image.decode()
      const canvas = document.createElement('canvas')
      canvas.width = image.naturalWidth
      canvas.height = image.naturalHeight
      const context = canvas.getContext('2d')
      if (!context) throw new Error('2d canvas context is unavailable')
      context.drawImage(image, 0, 0)
      return { width: canvas.width, height: canvas.height, pixels: context.getImageData(0, 0, canvas.width, canvas.height).data }
    }

    const [first, second] = await Promise.all([read(beforeBase64), read(afterBase64)])
    if (first.width !== second.width || first.height !== second.height) {
      throw new Error(`screenshot size changed: ${first.width}x${first.height} / ${second.width}x${second.height}`)
    }
    let left = first.width
    let top = first.height
    let right = -1
    let bottom = -1
    let changed = 0
    for (let y = 0; y < first.height; y += 1) {
      for (let x = 0; x < first.width; x += 1) {
        const index = (y * first.width + x) * 4
        const delta = Math.max(
          Math.abs(first.pixels[index] - second.pixels[index]),
          Math.abs(first.pixels[index + 1] - second.pixels[index + 1]),
          Math.abs(first.pixels[index + 2] - second.pixels[index + 2]),
        )
        if (delta < threshold) continue
        changed += 1
        left = Math.min(left, x)
        top = Math.min(top, y)
        right = Math.max(right, x)
        bottom = Math.max(bottom, y)
      }
    }
    return {
      changedPixels: changed,
      bounds: changed === 0 ? null : { x: left, y: top, width: right - left + 1, height: bottom - top + 1 },
    }
  }, {
    beforeBase64: before.toString('base64'),
    afterBase64: after.toString('base64'),
    threshold,
  })
}
