/** recordingThumbnailURL は録画サムネイルの streamer 配信 URL を組み立てる（OpenAPI 外）。 */
export function recordingThumbnailURL(recordingId: number): string {
  return `/api/media/recordings/${recordingId}/thumbnail`
}
