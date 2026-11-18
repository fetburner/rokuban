import type {
  createLabelRuleResponse,
  deleteLabelRuleResponse,
  getLabelRuleValueKeyResponse,
  listLabelRulesResponse,
  listRecordingShelvesResponse,
  listRulesResponse,
  listSitesResponse,
  updateLabelRuleResponse,
} from '@/api/generated'

/**
 * BodyOf は orval の応答型（status で判別する union）から、その status の body を取る。
 * openapi.yaml の応答の形が変わると、この型が変わり、表のフィクスチャが型エラーになる。
 */
type BodyOf<R extends { data: unknown; status: number }, S extends R['status']> = Extract<
  R,
  { status: S }
>['data']

/**
 * ApiRouteBodies は「メソッド パス」ごとの成功応答の body。パラメータは `:name` と書く。
 * DELETE の body は void なので、値 undefined は 204 No Content として返る。
 */
export interface ApiRouteBodies {
  'GET /api/sites': BodyOf<listSitesResponse, 200>
  'GET /api/rules': BodyOf<listRulesResponse, 200>
  'GET /api/recording-shelves': BodyOf<listRecordingShelvesResponse, 200>
  'GET /api/label-rules': BodyOf<listLabelRulesResponse, 200>
  'POST /api/label-rules': BodyOf<createLabelRuleResponse, 201>
  'GET /api/label-rule-value-key': BodyOf<getLabelRuleValueKeyResponse, 200>
  'PATCH /api/label-rules/:id': BodyOf<updateLabelRuleResponse, 200>
  'DELETE /api/label-rules/:id': BodyOf<deleteLabelRuleResponse, 204>
}

/** ApiRequest は応答を関数で決めるときに渡す要求。作成後に一覧が変わる等の状態を表す。 */
export interface ApiRequest {
  method: string
  url: URL
  /** JSON の request body。無ければ undefined。 */
  body: unknown
  /** パスの `:name` に入った値。 */
  params: Record<string, string>
}

/** ApiRoutes は上書きしたいルートだけを持つ。値は body そのもの、または要求から body を作る関数。 */
export type ApiRoutes = {
  [K in keyof ApiRouteBodies]?: ApiRouteBodies[K] | ((req: ApiRequest) => ApiRouteBodies[K])
}

/** 既定は一覧系を空で返す。どのテストにも共通の無害な応答だけを置く。 */
const defaultRoutes: ApiRoutes = {
  'GET /api/sites': [],
  'GET /api/rules': [],
  'GET /api/label-rules': [],
}

/** jsonResponse は JSON body の Response を作る。 */
export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** ApiCall は fetch の 1 回の呼び出し（fetchMock.mock.calls の 1 件に当たる）。 */
export interface ApiCall {
  method: string
  url: URL
  body: unknown
  params: Record<string, string>
}

function match(routes: ApiRoutes, method: string, pathname: string) {
  for (const key of Object.keys(routes) as (keyof ApiRouteBodies)[]) {
    const [keyMethod, pattern] = key.split(' ')
    if (keyMethod !== method || pattern === undefined) continue
    const names = [...pattern.matchAll(/:(\w+)/g)].map((m) => m[1] ?? '')
    const found = new RegExp(`^${pattern.replace(/:\w+/g, '([^/]+)')}$`).exec(pathname)
    if (found) {
      const params = Object.fromEntries(names.map((name, i) => [name, found[i + 1] ?? '']))
      return { responder: routes[key] as unknown, params }
    }
  }
  return undefined
}

/**
 * stubApi は globalThis.fetch を共有のルート表で置き換える。
 *
 * 表に無いルートは `unexpected fetch: <METHOD> <path>` を投げる。テストは確かめたい
 * ルートだけを渡して上書きする。返す calls は fetch の呼び出し記録。
 */
export function stubApi(overrides: ApiRoutes = {}) {
  const routes: ApiRoutes = { ...defaultRoutes, ...overrides }
  const calls: ApiCall[] = []
  globalThis.fetch = ((input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'
    const hit = match(routes, method, url.pathname)
    if (!hit) throw new Error(`unexpected fetch: ${method} ${url.pathname}`)
    const body = init?.body === undefined ? undefined : JSON.parse(String(init.body))
    calls.push({ method, url, body, params: hit.params })
    const value =
      typeof hit.responder === 'function'
        ? (hit.responder as (req: ApiRequest) => unknown)({ method, url, body, params: hit.params })
        : hit.responder
    if (value === undefined) return Promise.resolve(new Response(null, { status: 204 }))
    return Promise.resolve(jsonResponse(value, method === 'POST' ? 201 : 200))
  }) as unknown as typeof fetch
  return { calls }
}
