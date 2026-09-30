import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { CMLogoState } from '@/api/generated'
import { CMLogosPage } from '@/pages/cm-logos'
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
  detectedCount: 0,
  redetectableCount: 0,
  lastFailureStage: 'logo',
  frameRecordingId: 7,
}

const frameBytes = new Uint8Array([0xff, 0xd8, 0xff, 0xd9])

function stubApi(overrides: { cmDetect?: boolean; area?: CMLogoState['logoArea'] } = {}) {
  let currentArea = overrides.area
  const requests: Array<{ method: string; url: string; body?: unknown }> = []
  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(jsonResponse({ live: false, cmDetect: overrides.cmDetect ?? true }))
    }
    if (url.pathname === '/api/cm-logos' && method === 'GET') {
      return Promise.resolve(jsonResponse([{ ...logo, logoArea: currentArea }]))
    }
    if (url.pathname === '/api/media/recordings/7/frame' && method === 'GET') {
      return Promise.resolve(
        new Response(frameBytes, {
          status: 200,
          headers: {
            'Content-Type': 'image/jpeg',
            'X-Coded-Width': '1920',
            'X-Coded-Height': '1080',
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
      return this.getAttribute('data-testid') === 'cm-logo-frame' ? 360 : 0
    },
  })
  Object.defineProperty(HTMLElement.prototype, 'setPointerCapture', {
    configurable: true,
    value: () => {},
  })
  Object.defineProperty(HTMLElement.prototype, 'releasePointerCapture', {
    configurable: true,
    value: () => {},
  })
  Object.defineProperty(URL, 'createObjectURL', {
    configurable: true,
    value: () => 'blob:cm-logo-frame',
  })
  Object.defineProperty(URL, 'revokeObjectURL', {
    configurable: true,
    value: () => {},
  })
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
  it('局の失敗理由を表示し、枠を開くと指定位置の原寸コマを取り寄せる', async () => {
    const user = userEvent.setup()
    const { requests } = stubApi()
    renderInRouter(<CMLogosPage />, {
      path: '/cm-logos',
      initialEntries: ['/cm-logos?network=32678&service=5168&recording=7'],
    })

    expect(await screen.findByText('テスト放送局')).toBeInTheDocument()
    expect(screen.getByTestId('cm-logo-warning')).toHaveTextContent('ロゴを見つけられず、CM を検出できませんでした。')
    await user.click(screen.getByTestId('cm-logo-toggle'))
    expect(await screen.findByRole('region', { name: 'ロゴの枠' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: '次のコマ' }))
    await screen.findByTestId('cm-logo-frame-image')
    expect(requests).toEqual([])
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/media/recordings/7/frame?at=5000')
  })

  it('拡大表示中に描いた枠を記録上の画素へ変換して保存する', async () => {
    const user = userEvent.setup()
    const { requests } = stubApi()
    renderInRouter(<CMLogosPage />, {
      path: '/cm-logos',
      initialEntries: ['/cm-logos?network=32678&service=5168&recording=7'],
    })

    await user.click(await screen.findByTestId('cm-logo-toggle'))
    await user.click(screen.getByRole('button', { name: '次のコマ' }))
    const box = await screen.findByTestId('cm-logo-frame')
    await screen.findByTestId('cm-logo-frame-image')
    box.getBoundingClientRect = () =>
      ({ left: 0, top: 0, right: 640, bottom: 360, width: 640, height: 360 } as DOMRect)

    fireEvent.pointerDown(box, { pointerId: 1, clientX: 100, clientY: 80 })
    fireEvent.pointerMove(box, { pointerId: 1, clientX: 250, clientY: 200 })
    fireEvent.pointerUp(box, { pointerId: 1, clientX: 250, clientY: 200 })

    const save = screen.getByRole('button', { name: '枠を保存' })
    await waitFor(() => expect(save).toBeEnabled())
    await user.click(save)

    await waitFor(() => expect(requests.some((request) => request.method === 'PUT')).toBe(true))
    expect(requests.find((request) => request.method === 'PUT')?.body).toEqual({
      recordingId: 7,
      x: 1272,
      y: 96,
      w: 180,
      h: 144,
      codedWidth: 1920,
      codedHeight: 1080,
    })
  })

  it('CM 検出が無効なら枠を教える面を開かない', async () => {
    const user = userEvent.setup()
    stubApi({ cmDetect: false })
    renderInRouter(<CMLogosPage />, { path: '/cm-logos' })

    await user.click(await screen.findByTestId('cm-logo-toggle'))
    expect(screen.queryByRole('region', { name: 'ロゴの枠' })).not.toBeInTheDocument()
    expect(screen.getByText('このデプロイでは CM 検出が無効なので、枠を教える面は出ません。')).toBeInTheDocument()
  })
})
