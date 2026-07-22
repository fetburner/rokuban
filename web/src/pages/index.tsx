import { useGetVersion, useHealthz } from '../api/generated'

export function IndexPage() {
  const { data: health } = useHealthz()
  const { data: version } = useGetVersion()

  return (
    <div className="flex flex-col items-center justify-center min-h-screen gap-6">
      <h1 className="text-4xl font-bold tracking-tight">Rokuban</h1>
      <div className="flex gap-4 text-sm text-zinc-400">
        <span>Status: {health?.data.status ?? '...'}</span>
        <span>Version: {version?.data.version ?? '...'}</span>
      </div>
    </div>
  )
}
