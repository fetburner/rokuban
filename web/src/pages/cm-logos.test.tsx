import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { CMLogoState, Recording } from '@/api/generated'
import { CMLogoStationPage, CMLogosPage } from '@/pages/cm-logos'
import { renderInRouter } from '@/test/router'

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const logo: CMLogoState = {
  networkId: 32678,
  serviceId: 5168,
  serviceName: 'テスト放送局',
  site: 'default',
  state: 'failed',
  recordingCount: 2,
  failedCount: 1,
  pendingCount: 0,
  detectedCount: 1,
  redetectableCount: 2,
  lastFailureStage: 'logo',
  frameRecordingId: 7,
}

const pendingLogo: CMLogoState = {
  ...logo,
  networkId: 1,
  serviceId: 2,
  serviceName: '待機局',
  state: 'learned',
  failedCount: 0,
  pendingCount: 3,
  redetectableCount: 0,
}

const healthyLogo: CMLogoState = {
  ...logo,
  networkId: 3,
  serviceId: 4,
  serviceName: '問題なし局',
  state: 'learned',
  failedCount: 0,
  pendingCount: 0,
}

const recording = {
  id: 7,
  site: 'default',
  source: 'manual',
  serviceName: 'テスト放送局',
  channelType: 'GR',
  channel: '27',
  networkId: 32678,
  serviceId: 5168,
  eventId: 1,
  title: 'テスト番組',
  startAt: '2026-09-30T12:00:00Z',
  durationMs: 600000,
  status: 'finished',
  keepOriginal: 'always',
  cmDetection: { state: 'failed', stage: 'logo' },
  sizeBytes: 1000,
  createdAt: '2026-09-30T12:00:00Z',
} as Recording

const frameBytes = new Uint8Array([0xff, 0xd8, 0xff, 0xd9])

function stubApi(options: {
  logos?: CMLogoState[]
  cmDetect?: boolean
  area?: CMLogoState['logoArea']
} = {}) {
  let currentArea = options.area
  const requests: Array<{ method: string; url: string; body?: unknown }> = []
  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(jsonResponse({ live: false, cmDetect: options.cmDetect ?? true }))
    }
    if (url.pathname === '/api/cm-logos' && method === 'GET') {
      const rows = options.logos ?? [{ ...logo, logoArea: currentArea }]
      return Promise.resolve(jsonResponse(rows))
    }
    if (url.pathname === '/api/recordings' && method === 'GET') {
      return Promise.resolve(jsonResponse([recording]))
    }
    if (url.pathname === '/api/media/recordings/7/frame' && method === 'GET') {
      requests.push({ method, url: url.pathname + url.search })
      return Promise.resolve(
        new Response(frameBytes, {
          status: 200,
          headers: {
            'Content-Type': 'image/jpeg',
            'X-Coded-Width': '1440',
            'X-Coded-Height': '1080',
            'X-Sample-Aspect-Ratio': '4:3',
          },
        }),
      )
    }
    if (url.pathname === '/api/cm-logos/32678/5168/area') {
      const body = init?.body === undefined ? undefined : JSON.parse(String(init.body))
      requests.push({ method, url: url.pathname, body })
      if (method === 'PUT') currentArea = { ...body, updatedAt: '2026-09-30T00:00:00Z' }
      if (method === 'DELETE') currentArea = undefined
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  }) as unknown as typeof fetch
  return { requests }
}

const originalClientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth')
const originalClientHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientHeight')
const originalSetPointerCapture = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'setPointerCapture')
const originalReleasePointerCapture = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'releasePointerCapture')

function restorePrototypeProperty(
  target: typeof HTMLElement.prototype,
  name: 'clientWidth' | 'clientHeight' | 'setPointerCapture' | 'releasePointerCapture',
  descriptor: PropertyDescriptor | undefined,
) {
  if (descriptor) Object.defineProperty(target, name, descriptor)
  else delete target[name]
}

beforeEach(() => {
  Object.defineProperty(HTMLElement.prototype, 'clientWidth', {
    configurable: true,
    get() {
      return this.getAttribute('data-testid') === 'cm-logo-frame' ? 640 : 0
    },
  })
  Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
    configurable: true,
    get() {
      return this.getAttribute('data-testid') === 'cm-logo-frame' ? 640 / (16 / 9) : 0
    },
  })
  Object.defineProperty(HTMLElement.prototype, 'setPointerCapture', { configurable: true, value: () => {} })
  Object.defineProperty(HTMLElement.prototype, 'releasePointerCapture', { configurable: true, value: () => {} })
  Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: () => 'blob:cm-logo-frame' })
  Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: () => {} })
})

afterEach(() => {
  vi.restoreAllMocks()
  restorePrototypeProperty(HTMLElement.prototype, 'clientWidth', originalClientWidth)
  restorePrototypeProperty(HTMLElement.prototype, 'clientHeight', originalClientHeight)
  restorePrototypeProperty(HTMLElement.prototype, 'setPointerCapture', originalSetPointerCapture)
  restorePrototypeProperty(HTMLElement.prototype, 'releasePointerCapture', originalReleasePointerCapture)
  Reflect.deleteProperty(URL, 'createObjectURL')
  Reflect.deleteProperty(URL, 'revokeObjectURL')
})

describe('CMLogosPage', () => {
  it('局を要対応・検出待ち・問題なしの順に分け、問題なしを閉じる', async () => {
    stubApi({ logos: [healthyLogo, pendingLogo, logo] })
    renderInRouter(<CMLogosPage />, { path: '/cm-logos' })

    expect(await screen.findByTestId('cm-logo-attention')).toHaveTextContent('テスト放送局')
    expect(screen.getByTestId('cm-logo-pending')).toHaveTextContent('待機局')
    const healthy = screen.getByTestId('cm-logo-healthy')
    expect(healthy).not.toBeVisible()
    expect(screen.getByText('問題なし（1）')).toBeInTheDocument()
  })
})

describe('CMLogoStationPage', () => {
  it('中央の時刻を初期表示し、スライダーを動かすまでコマを取り直さない', async () => {
    const { requests } = stubApi()
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })

    expect(await screen.findByText('テスト放送局')).toBeInTheDocument()
    await screen.findByTestId('cm-logo-frame-image')
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/media/recordings/7/frame?at=300000')
    const frameRequestsBeforeDrag = requests.filter((request) => request.url.includes('/frame')).length
    const slider = screen.getByTestId('cm-logo-time-slider')
    fireEvent.change(slider, { target: { value: '100000' } })
    expect(requests.filter((request) => request.url.includes('/frame')).length).toBe(frameRequestsBeforeDrag)
    fireEvent.pointerUp(slider)
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledWith('/api/media/recordings/7/frame?at=100000'))
  })

  it('SAR の違うコマで数値入力した枠を記録上の座標で保存する', async () => {
    const { requests } = stubApi()
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })

    await screen.findByTestId('cm-logo-frame-image')
    fireEvent.change(screen.getByTestId('cm-logo-field-x'), { target: { value: '400' } })
    fireEvent.change(screen.getByTestId('cm-logo-field-y'), { target: { value: '300' } })
    fireEvent.change(screen.getByTestId('cm-logo-field-w'), { target: { value: '400' } })
    fireEvent.change(screen.getByTestId('cm-logo-field-h'), { target: { value: '300' } })
    await waitFor(() => expect(screen.getByRole('button', { name: '枠を保存' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: '枠を保存' }))

    await waitFor(() => expect(requests.some((request) => request.method === 'PUT')).toBe(true))
    expect(requests.find((request) => request.method === 'PUT')?.body).toEqual({
      recordingId: 7,
      x: 400,
      y: 300,
      w: 400,
      h: 300,
      codedWidth: 1440,
      codedHeight: 1080,
    })
    expect(await screen.findByTestId('cm-logo-save-message')).toHaveTextContent('検出待ち')
  })

  it('CM 検出が無効なら枠の編集面を出さない', async () => {
    stubApi({ cmDetect: false })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168'],
    })

    expect(await screen.findByText('このデプロイでは CM 検出が無効なので、枠を教える面は出ません。')).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'ロゴの枠' })).not.toBeInTheDocument()
  })
})
