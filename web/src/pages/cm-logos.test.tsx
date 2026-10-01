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
const previewPng = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='

/** 数値入力へ値を入れて blur で確定する。 */
function typeNumber(key: 'x' | 'y' | 'w' | 'h', value: string) {
  const input = screen.getByTestId(`cm-logo-field-${key}`)
  fireEvent.change(input, { target: { value } })
  fireEvent.blur(input)
}

function stubApi(options: {
  logos?: CMLogoState[]
  logo?: CMLogoState
  candidate?: NonNullable<CMLogoState['candidate']>
  cmDetect?: boolean
  area?: CMLogoState['logoArea']
  recordings?: Recording[]
  frameHeaders?: Record<string, string>
  removeOriginalsOnCandidateDiscard?: boolean
} = {}) {
  let currentArea = options.area
  let currentCandidate = options.candidate
  let currentFrameRecordingId = (options.logo ?? logo).frameRecordingId
  let currentLearnedAt = (options.logo ?? logo).learnedAt
  let currentRecordings = options.recordings ?? [recording]
  let logoFetchCount = 0
  const requests: Array<{ method: string; url: string; body?: unknown }> = []
  globalThis.fetch = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'
    if (url.pathname === '/api/capabilities') {
      return Promise.resolve(jsonResponse({ live: false, cmDetect: options.cmDetect ?? true }))
    }
    if (url.pathname === '/api/cm-logos' && method === 'GET') {
      logoFetchCount += 1
      const state = {
        ...(options.logo ?? logo),
        frameRecordingId: currentFrameRecordingId,
        logoArea: currentArea,
        ...(currentCandidate === undefined ? {} : { candidate: currentCandidate }),
      }
      if (currentLearnedAt === undefined) delete state.learnedAt
      else state.learnedAt = currentLearnedAt
      const rows = options.logos ?? [state]
      return Promise.resolve(jsonResponse(rows))
    }
    if (url.pathname === '/api/recordings' && method === 'GET') {
      return Promise.resolve(jsonResponse(currentRecordings))
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
            ...options.frameHeaders,
          },
        }),
      )
    }
    if (url.pathname === '/api/recordings/7/cm-detection/retry' && method === 'POST') {
      requests.push({ method, url: url.pathname })
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    if (url.pathname === '/api/cm-logos/32678/5168/area') {
      const body = init?.body === undefined ? undefined : JSON.parse(String(init.body))
      requests.push({ method, url: url.pathname, body })
      if (method === 'PUT') currentArea = { ...body, updatedAt: '2026-09-30T00:00:00Z' }
      if (method === 'DELETE') currentArea = undefined
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    if (url.pathname === '/api/cm-logos/32678/5168/candidate/adopt' && method === 'POST') {
      const body = init?.body === undefined ? undefined : JSON.parse(String(init.body))
      requests.push({ method, url: url.pathname, body })
      currentCandidate = undefined
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    if (url.pathname === '/api/cm-logos/32678/5168/candidate' && method === 'DELETE') {
      requests.push({ method, url: url.pathname })
      currentCandidate = undefined
      if (options.removeOriginalsOnCandidateDiscard) {
        currentFrameRecordingId = 0
        currentRecordings = []
      }
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    if (url.pathname === '/api/cm-logos/32678/5168' && method === 'DELETE') {
      requests.push({ method, url: url.pathname })
      currentLearnedAt = undefined
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
  }) as unknown as typeof fetch
  return {
    requests,
    logoFetchCount: () => logoFetchCount,
    setCandidate: (next: NonNullable<CMLogoState['candidate']> | undefined) => {
      currentCandidate = next
    },
  }
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
    // 失敗と検出待ちが同居する局は「要対応」に入る（判定の順序を固定する）。
    const mixedLogo = { ...logo, networkId: 5, serviceId: 6, serviceName: '混在局', failedCount: 1, pendingCount: 3 }
    stubApi({ logos: [healthyLogo, pendingLogo, mixedLogo, logo] })
    renderInRouter(<CMLogosPage />, { path: '/cm-logos' })

    expect(await screen.findByTestId('cm-logo-attention')).toHaveTextContent('テスト放送局')
    expect(screen.getByTestId('cm-logo-attention')).toHaveTextContent('混在局')
    expect(screen.getByTestId('cm-logo-pending')).toHaveTextContent('待機局')
    expect(screen.getByTestId('cm-logo-pending')).not.toHaveTextContent('混在局')
    const healthy = screen.getByTestId('cm-logo-healthy')
    expect(healthy).not.toBeVisible()
    expect(screen.getByText('問題なし（1）')).toBeInTheDocument()
  })
})

describe('CMLogosPage の候補分類', () => {
  const learned = { ...healthyLogo, networkId: 5, serviceId: 6, learnedAt: '2026-09-29T00:00:00Z' }
  const candidateBase = {
    x: 1, y: 1, w: 10, h: 10, codedWidth: 1440, codedHeight: 1080, recordingId: 7, attemptedAt: '2026-09-30T00:00:00Z',
  }

  it('ready 候補の局は問題なしの件数でも要対応に入り、札は候補あり', async () => {
    stubApi({ logos: [{ ...learned, serviceName: '候補局', candidate: { state: 'ready', ...candidateBase } }] })
    renderInRouter(<CMLogosPage />, { path: '/cm-logos' })
    const section = await screen.findByTestId('cm-logo-attention')
    expect(section).toHaveTextContent('候補局')
    expect(section).toHaveTextContent('ロゴ候補を確認して採用してください')
    expect(section).toHaveTextContent('候補あり')
  })

  it('running 候補の局は検出待ちに入る', async () => {
    stubApi({ logos: [{ ...learned, serviceName: '解析局', candidate: { state: 'running', ...candidateBase } }] })
    renderInRouter(<CMLogosPage />, { path: '/cm-logos' })
    const section = await screen.findByTestId('cm-logo-pending')
    expect(section).toHaveTextContent('解析局')
    expect(section).toHaveTextContent('ロゴ候補を解析中です')
  })

  it('枠があり候補の行が無い解析待ちの局は検出待ちに入る', async () => {
    stubApi({
      logos: [{
        ...healthyLogo,
        serviceName: '待ち局',
        logoArea: { x: 1, y: 1, w: 10, h: 10, codedWidth: 1440, codedHeight: 1080, updatedAt: '2026-09-30T00:00:00Z' },
      }],
    })
    renderInRouter(<CMLogosPage />, { path: '/cm-logos' })
    const section = await screen.findByTestId('cm-logo-pending')
    expect(section).toHaveTextContent('待ち局')
    expect(section).toHaveTextContent('ロゴ候補を解析中です')
  })

  it('原本のある録画がない局は解析待ちにせず、開始できない理由を表示する', async () => {
    stubApi({
      logos: [{
        ...healthyLogo,
        serviceName: '原本なし局',
        frameRecordingId: 0,
        logoArea: { x: 1, y: 1, w: 10, h: 10, codedWidth: 1440, codedHeight: 1080, updatedAt: '2026-09-30T00:00:00Z' },
      }],
    })
    renderInRouter(<CMLogosPage />, { path: '/cm-logos' })

    expect(await screen.findByText('原本のある録画がないため、ロゴ候補の解析を始められません。')).toBeInTheDocument()
    expect(screen.queryByTestId('cm-logo-pending')).not.toBeInTheDocument()
  })

  it('failed 候補の局は工程の一文つきで要対応に入る', async () => {
    stubApi({ logos: [{ ...learned, serviceName: '失敗局', candidate: { state: 'failed', stage: 'logo', ...candidateBase } }] })
    renderInRouter(<CMLogosPage />, { path: '/cm-logos' })
    const section = await screen.findByTestId('cm-logo-attention')
    expect(section).toHaveTextContent('失敗局')
    expect(section).toHaveTextContent('ロゴを見つけられず、CM を検出できませんでした。')
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
    const slider = screen.getByTestId('cm-logo-time')
    fireEvent.input(slider, { target: { value: '100000' } })
    expect(requests.filter((request) => request.url.includes('/frame')).length).toBe(frameRequestsBeforeDrag)
    fireEvent.change(slider)
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledWith('/api/media/recordings/7/frame?at=100000'))
  })

  it('SAR の違うコマで数値入力した枠を記録上の座標で保存する', async () => {
    const { requests } = stubApi()
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })

    await screen.findByTestId('cm-logo-frame-image')
    typeNumber('x', '400')
    typeNumber('y', '300')
    typeNumber('w', '400')
    typeNumber('h', '300')
    await waitFor(() => expect(screen.getByRole('button', { name: 'ロゴを解析' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: 'ロゴを解析' }))

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
    expect(await screen.findByTestId('cm-logo-candidate-running')).toHaveTextContent('解析中です')
  })

  it('数値入力は入力中に丸めず、blur で下限・範囲へ確定する', async () => {
    stubApi()
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    await screen.findByTestId('cm-logo-frame-image')
    const width = screen.getByTestId('cm-logo-field-w')
    const x = screen.getByTestId('cm-logo-field-x')

    // 400 を打つ途中の 4 が下限 8 へ書き戻されると、続きの 00 が 800 になる。
    fireEvent.change(width, { target: { value: '4' } })
    expect(width).toHaveValue(4)
    fireEvent.change(width, { target: { value: '40' } })
    fireEvent.change(width, { target: { value: '400' } })
    fireEvent.blur(width)
    expect(width).toHaveValue(400)

    // 空にして打ち直せる。消している途中で値が戻ってはならない。
    fireEvent.change(x, { target: { value: '' } })
    expect(x).toHaveValue(null)
    fireEvent.change(x, { target: { value: '5' } })
    fireEvent.change(x, { target: { value: '50' } })
    expect(x).toHaveValue(50)
    fireEvent.blur(x)
    expect(x).toHaveValue(50)

    // 確定時は下限と映像内へ寄せる。
    fireEvent.change(width, { target: { value: '3' } })
    fireEvent.blur(width)
    expect(width).toHaveValue(8)
    fireEvent.change(width, { target: { value: '99999' } })
    fireEvent.blur(width)
    expect(width).toHaveValue(1440 - 50)
  })

  it('枠に寄る焦点は押した時点で固定し、枠を動かしても画像は動かない', async () => {
    stubApi()
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    const image = await screen.findByTestId('cm-logo-frame-image')
    typeNumber('x', '600')
    typeNumber('y', '400')
    typeNumber('w', '100')
    typeNumber('h', '100')
    fireEvent.click(screen.getByRole('button', { name: '枠に寄る' }))
    const before = (image as HTMLElement).style.left
    expect(before).not.toBe('')
    typeNumber('x', '700')
    expect((image as HTMLElement).style.left).toBe(before)
  })

  it('表示枠の比は SAR から導き、16:9 に固定しない', async () => {
    stubApi({
      frameHeaders: { 'X-Coded-Width': '720', 'X-Coded-Height': '480', 'X-Sample-Aspect-Ratio': '8:9' },
    })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    await screen.findByTestId('cm-logo-frame-image')
    const ratio = parseFloat(screen.getByTestId('cm-logo-frame').style.aspectRatio)
    expect(ratio).toBeCloseTo(4 / 3, 3)
  })

  it('ハンドルは見た目の要素で掴み、角から離れていても新しい枠を描かない。ドラッグ後は整数', async () => {
    stubApi()
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    await screen.findByTestId('cm-logo-frame-image')
    typeNumber('x', '400')
    typeNumber('y', '300')
    typeNumber('w', '400')
    typeNumber('h', '300')
    const handle = screen.getByTestId('cm-logo-handle-se')
    const frame = screen.getByTestId('cm-logo-frame')
    // 箱 640x360 / coded 1440x1080 → 横縦とも 0.4444 CSS px / 画素。右下の角は (355.6, 266.7)。
    fireEvent.pointerDown(handle, { clientX: 355.6 + 18, clientY: 266.7 + 18, pointerId: 1 })
    fireEvent.pointerMove(frame, { clientX: 400.3, clientY: 300.3, pointerId: 1 })
    fireEvent.pointerUp(frame, { clientX: 400.3, clientY: 300.3, pointerId: 1 })
    expect(screen.getByTestId('cm-logo-field-x')).toHaveValue(400)
    expect(screen.getByTestId('cm-logo-field-y')).toHaveValue(300)
    const w = Number((screen.getByTestId('cm-logo-field-w') as HTMLInputElement).value)
    expect(Number.isInteger(w)).toBe(true)
    expect(w).toBe(501)
  })

  it('成功した録画にも再検出を出し、押すと再検出を依頼する', async () => {
    const detected = { ...recording, cmDetection: { state: 'detected' } } as Recording
    const { requests } = stubApi({ recordings: [detected] })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    fireEvent.click(await screen.findByRole('button', { name: '再検出' }))
    await waitFor(() =>
      expect(requests.some((request) => request.method === 'POST' && request.url.endsWith('/retry'))).toBe(true),
    )
  })

  it('原本の無い録画へのディープリンクは捨て、原本のある frameRecordingId のコマを使う', async () => {
    const noOriginal = { ...recording, id: 9, sizeBytes: undefined } as Recording
    const { requests } = stubApi({ recordings: [noOriginal, recording] })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=9'],
    })
    await screen.findByTestId('cm-logo-frame-image')
    expect(requests.some((request) => request.url.startsWith('/api/media/recordings/7/frame'))).toBe(true)
    expect(requests.some((request) => request.url.includes('/recordings/9/'))).toBe(false)
  })

  it('候補が running の間は解析中と画面を離れても続くことを表示する', async () => {
    const candidate: NonNullable<CMLogoState['candidate']> = {
      state: 'running',
      x: 400,
      y: 300,
      w: 400,
      h: 300,
      codedWidth: 1440,
      codedHeight: 1080,
      recordingId: 7,
      attemptedAt: '2026-09-30T00:00:00Z',
    }
    stubApi({ candidate })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    expect(await screen.findByTestId('cm-logo-candidate-running')).toHaveTextContent('解析中です')
    expect(screen.queryByTestId('cm-logo-candidate-adopt')).not.toBeInTheDocument()
    expect(screen.getByTestId('cm-logo-candidate-running')).toHaveTextContent('画面を離れても続きます')
  })

  it('候補が failed のとき工程の一文と描き直しを表示する', async () => {
    stubApi({
      candidate: {
        state: 'failed',
        stage: 'area',
        error: 'raw error must not be shown',
        x: 400,
        y: 300,
        w: 400,
        h: 300,
        codedWidth: 1440,
        codedHeight: 1080,
        recordingId: 7,
        attemptedAt: '2026-09-30T00:00:00Z',
      },
    })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    expect(await screen.findByTestId('cm-logo-candidate-failed')).toHaveTextContent(
      '教えた枠が録画の解像度と合わないため、枠を使えませんでした。',
    )
    expect(screen.getByTestId('cm-logo-candidate-failed')).toHaveTextContent('枠を描き直して解析し直してください。')
    expect(screen.queryByTestId('cm-logo-candidate-adopt')).not.toBeInTheDocument()
    expect(screen.getByTestId('cm-logo-candidate-failure-message')).not.toHaveTextContent('raw error must not be shown')
  })

  it('工程が枠で直らない failed 候補には描き直しの案内を出さない', async () => {
    stubApi({
      candidate: {
        state: 'failed',
        stage: 'setup',
        x: 400,
        y: 300,
        w: 400,
        h: 300,
        codedWidth: 1440,
        codedHeight: 1080,
        recordingId: 7,
        attemptedAt: '2026-09-30T00:00:00Z',
      },
    })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    expect(await screen.findByTestId('cm-logo-candidate-failed')).toHaveTextContent('ロゴの枠では直せない失敗です。')
    expect(screen.getByTestId('cm-logo-candidate-failed')).not.toHaveTextContent('枠を描き直して')
  })

  it('原本を失った候補を破棄した後は解析不能の理由を表示してポーリングしない', async () => {
    const { requests, logoFetchCount } = stubApi({
      logo: { ...logo, serviceName: '候補破棄局', frameRecordingId: 7 },
      area: { x: 1, y: 1, w: 10, h: 10, codedWidth: 1440, codedHeight: 1080, updatedAt: '2026-09-30T00:00:00Z' },
      candidate: {
        state: 'ready',
        x: 1,
        y: 1,
        w: 10,
        h: 10,
        codedWidth: 1440,
        codedHeight: 1080,
        recordingId: 7,
        attemptedAt: '2026-09-30T00:00:00Z',
      },
      recordings: [recording],
      removeOriginalsOnCandidateDiscard: true,
    })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })

    fireEvent.click(await screen.findByTestId('cm-logo-candidate-discard'))
    expect(await screen.findByText('原本のある録画がないため、ロゴ候補の解析を始められません。')).toBeInTheDocument()
    expect(screen.getByTestId('cm-logo-no-original')).toBeInTheDocument()
    expect(requests.some((request) => request.method === 'DELETE' && request.url.endsWith('/candidate'))).toBe(true)

    const fetchCountAfterDiscard = logoFetchCount()
    await new Promise((resolve) => window.setTimeout(resolve, 5200))
    expect(logoFetchCount()).toBe(fetchCountAfterDiscard)
  }, 12000)

  it('原本のない局で覚えたロゴを捨てた後は解析不能の理由を表示してポーリングしない', async () => {
    const { requests, logoFetchCount } = stubApi({
      logo: { ...logo, serviceName: 'ロゴ削除局', frameRecordingId: 0, learnedAt: '2026-10-01T00:00:00Z' },
      area: { x: 1, y: 1, w: 10, h: 10, codedWidth: 1440, codedHeight: 1080, updatedAt: '2026-09-30T00:00:00Z' },
      recordings: [],
    })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168'],
    })

    fireEvent.click(await screen.findByText('高度な操作'))
    fireEvent.click(await screen.findByRole('button', { name: '覚えたロゴを捨てる' }))
    expect(await screen.findByText('原本のある録画がないため、ロゴ候補の解析を始められません。')).toBeInTheDocument()
    expect(screen.getByTestId('cm-logo-no-original')).toBeInTheDocument()
    expect(requests.some((request) => request.method === 'DELETE' && request.url === '/api/cm-logos/32678/5168')).toBe(true)

    const fetchCountAfterDelete = logoFetchCount()
    await new Promise((resolve) => window.setTimeout(resolve, 5200))
    expect(logoFetchCount()).toBe(fetchCountAfterDelete)
  }, 12000)

  it.each([
    ['学習が無い', undefined, true],
    ['学習が枠より古い', '2026-09-29T00:00:00Z', true],
    ['学習が枠より新しい', '2026-09-30T01:00:00Z', false],
  ])('枠があり候補の行が無いとき（%s）の解析待ち表示は %s', async (_name, learnedAt, waiting) => {
    stubApi({
      logo: { ...logo, ...(learnedAt ? { learnedAt } : {}) },
      area: { x: 1, y: 1, w: 10, h: 10, codedWidth: 1440, codedHeight: 1080, updatedAt: '2026-09-30T00:00:00Z' },
    })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    await screen.findByTestId('cm-logo-time')
    if (waiting) expect(await screen.findByTestId('cm-logo-candidate-running')).toBeInTheDocument()
    else expect(screen.queryByTestId('cm-logo-candidate-running')).not.toBeInTheDocument()
  })

  it('解析待ちの間は candidate が無くてもポーリングし、候補が届いたら表示を切り替える', async () => {
    const { setCandidate } = stubApi({
      area: { x: 1, y: 1, w: 10, h: 10, codedWidth: 1440, codedHeight: 1080, updatedAt: '2026-09-30T00:00:00Z' },
    })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    await screen.findByTestId('cm-logo-candidate-running')
    setCandidate({
      state: 'failed',
      stage: 'logo',
      x: 1,
      y: 1,
      w: 10,
      h: 10,
      codedWidth: 1440,
      codedHeight: 1080,
      recordingId: 7,
      attemptedAt: '2026-09-30T00:00:10Z',
    })
    expect(await screen.findByTestId('cm-logo-candidate-failed', undefined, { timeout: 8000 })).toBeInTheDocument()
  }, 12000)

  it('採用待ちで止めた録画は採用待ちの文を出し、再試行を出さない', async () => {
    stubApi({ recordings: [{ ...recording, cmDetection: { state: 'failed', stage: 'adopt' } } as Recording] })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    expect(await screen.findByText(/この局はロゴの採用待ちです/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '再検出' })).not.toBeInTheDocument()
    expect(screen.queryByText('3 回の試行に失敗しました')).not.toBeInTheDocument()
  })

  it('他の失敗した録画には再試行を出す', async () => {
    stubApi()
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    expect(await screen.findByRole('button', { name: '再検出' })).toBeInTheDocument()
  })

  it('候補が ready のとき現在のロゴと並べて表示し、採用 body に redetect を載せる', async () => {
    const { requests } = stubApi({
      logo: {
        ...logo,
        state: 'learned',
        failedCount: 0,
        pendingCount: 2,
        learnedAt: '2026-09-29T00:00:00Z',
        previewPng,
        codedWidth: 1440,
        codedHeight: 1080,
      },
      candidate: {
        state: 'ready',
        previewPng,
        x: 400,
        y: 300,
        w: 400,
        h: 300,
        codedWidth: 1440,
        codedHeight: 1080,
        recordingId: 7,
        attemptedAt: '2026-09-30T00:00:00Z',
      },
    })
    renderInRouter(<CMLogoStationPage />, {
      path: '/cm-logos/$networkId/$serviceId',
      initialEntries: ['/cm-logos/32678/5168?recording=7'],
    })
    expect(await screen.findByTestId('cm-logo-candidate-ready')).toBeInTheDocument()
    expect(screen.getByTestId('cm-logo-candidate-ready')).toHaveTextContent(
      '採用前に検出した録画 1 件（うち原本が残っている 2 件）も検出し直す',
    )
    expect(screen.getByTestId('cm-logo-current-preview')).toBeInTheDocument()
    expect(screen.getByTestId('cm-logo-candidate-preview')).toBeInTheDocument()
    const checkbox = screen.getByTestId('cm-logo-candidate-redetect')
    expect(checkbox).toBeChecked()
    fireEvent.click(checkbox)
    expect(checkbox).not.toBeChecked()
    fireEvent.click(screen.getByTestId('cm-logo-candidate-adopt'))
    await waitFor(() => expect(requests.some((request) => request.url.endsWith('/candidate/adopt'))).toBe(true))
    expect(requests.find((request) => request.url.endsWith('/candidate/adopt'))?.body).toEqual({ redetect: false })
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
