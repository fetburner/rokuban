import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError, customInstance } from '@/api/client'

describe('customInstance', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('204 (No Content) をボディなしの成功として扱う', async () => {
    // POST /api/breakers/{name}/resume はボディなしで 204 を返す。
    // response.json() を無条件に呼ぶと空ボディで SyntaxError になり、
    // 成功しているのに mutation の onError に落ちてしまう（実際に見つけたバグ）。
    globalThis.fetch = vi.fn().mockResolvedValue(new Response(null, { status: 204 }))

    const result = await customInstance('/api/breakers/ruler_deletes/resume', { method: 'POST' })

    expect(result).toEqual({ data: undefined, status: 204, headers: expect.any(Headers) })
  })

  it('200 はボディを JSON として読む', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    const result = await customInstance<{ data: unknown; status: number }>('/api/health')

    expect(result.status).toBe(200)
    expect(result.data).toEqual({ ok: true })
  })

  it('非 2xx は throw する（黙って成功に見せない）', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: 'not tripped' }), { status: 404 }),
    )

    await expect(customInstance('/api/breakers/unknown/resume', { method: 'POST' })).rejects.toThrow(
      '404',
    )
  })

  it('非 2xx のエラー本文を捨てない', async () => {
    // 400 に添えられた理由（ErrorResponse.error）を落とすと、UI は
    // 「失敗しました」しか言えない。検索の不正な正規表現のように、
    // 理由が分からないとユーザーが直せない失敗がある
    globalThis.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: 'invalid regex "("' }), {
        status: 400,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    const error = await customInstance('/api/programs/search', { method: 'POST' }).catch(
      (e: unknown) => e,
    )

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(400)
    expect((error as ApiError).body).toEqual({ error: 'invalid regex "("' })
  })

  it('JSON でないエラー本文でもステータスは伝わる', async () => {
    // プロキシが返す HTML のエラーページで例外にしてはならない
    globalThis.fetch = vi
      .fn()
      .mockResolvedValue(new Response('<html>502</html>', { status: 502 }))

    const error = await customInstance('/api/programs/search', { method: 'POST' }).catch(
      (e: unknown) => e,
    )

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(502)
    expect((error as ApiError).body).toBeUndefined()
  })
})
