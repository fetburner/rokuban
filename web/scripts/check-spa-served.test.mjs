import { describe, expect, it } from 'vitest'
import { checkSPAServed } from './check-spa-served.mjs'

const index = '<!doctype html><html><head><link href="/assets/app.css"></head><body><script src="/assets/app.js"></script></body></html>'

function response(status, contentType, body) {
  return {
    status,
    headers: { get: (name) => name === 'content-type' ? contentType : null },
    text: async () => body,
  }
}

function serverResponse({ fallbackStatus = 200, assetType = 'text/javascript' } = {}) {
  return async (input) => {
    const url = new URL(input)
    if (url.pathname === '/') return response(200, 'text/html; charset=utf-8', index)
    if (url.pathname.startsWith('/assets/')) return response(200, assetType, 'asset')
    if (url.pathname === '/ci-spa-fallback/deep/route') {
      return response(fallbackStatus, fallbackStatus === 200 ? 'text/html' : 'text/plain', fallbackStatus === 200 ? index : 'missing')
    }
    return response(404, 'text/plain', 'missing')
  }
}

describe('本体 SPA 配信の確認', () => {
  it('index.html の資産と深いパスのフォールバックを受け入れる', async () => {
    await expect(checkSPAServed('http://localhost:40773', serverResponse())).resolves.toBeUndefined()
  })

  it('深いパスが 404 のサーバーを拒否する', async () => {
    await expect(checkSPAServed('http://localhost:40773', serverResponse({ fallbackStatus: 404 })))
      .rejects.toThrow('深いパスは HTTP 404')
  })

  it('資産を HTML にフォールバックするサーバーを拒否する', async () => {
    await expect(checkSPAServed('http://localhost:40773', serverResponse({ assetType: 'text/html' })))
      .rejects.toThrow('が HTML にフォールバックしました')
  })
})
