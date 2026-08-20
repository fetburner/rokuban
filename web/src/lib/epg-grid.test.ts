import { describe, expect, it } from 'vitest'

import type { Service } from '@/api/generated'
import {
  axisHeightPx,
  channelTypeLabel,
  groupByChannelType,
  groupProgramsByService,
  hourTicks,
  orderServices,
  pxToTime,
  serviceDisambiguator,
  spanToPx,
  timeToPx,
  visibleColumnRange,
  visibleTimeWindow,
  type TimeAxis,
} from '@/lib/epg-grid'

/**
 * 軸はローカル時刻の 0 時基準で作る（`hourTicks` がローカルの毎時 0 分を返すため、
 * epoch 直値で組むとテストがタイムゾーン依存になる）。
 */
const dayStart = new Date(2026, 6, 25, 0, 0, 0, 0)

const axis: TimeAxis = {
  startMs: dayStart.getTime(),
  endMs: dayStart.getTime() + 24 * 3_600_000,
  pxPerHour: 120,
}

/** at は軸の開始からの分数を epoch ms に直す。 */
function at(minutes: number): number {
  return axis.startMs + minutes * 60_000
}

describe('時間軸の写像', () => {
  it('軸の高さは 1 時間あたりの高さ x 時間数', () => {
    expect(axisHeightPx(axis)).toBe(24 * 120)
  })

  it('時刻は軸の先頭からの距離になる', () => {
    expect(timeToPx(axis, axis.startMs)).toBe(0)
    expect(timeToPx(axis, at(30))).toBe(60)
    expect(timeToPx(axis, at(19 * 60))).toBe(19 * 120)
  })

  it('軸の外の時刻もクランプせずに写す（はみ出しを呼び出し側が判定できる）', () => {
    expect(timeToPx(axis, at(-60))).toBe(-120)
    expect(timeToPx(axis, at(25 * 60))).toBe(25 * 120)
  })

  it('pxToTime は timeToPx の逆写像', () => {
    expect(pxToTime(axis, 0)).toBe(axis.startMs)
    expect(pxToTime(axis, 19 * 120)).toBe(at(19 * 60))
  })
})

describe('spanToPx', () => {
  it('区間の高さは放送時間に比例する', () => {
    expect(spanToPx(axis, at(19 * 60), at(20 * 60))).toEqual({ topPx: 2280, heightPx: 120 })
    // 30 分 = 60px、5 分 = 10px。下限を設けないので短い番組は本当に短い
    expect(spanToPx(axis, at(19 * 60), at(19 * 60 + 30))).toEqual({ topPx: 2280, heightPx: 60 })
    expect(spanToPx(axis, at(19 * 60), at(19 * 60 + 5))).toEqual({ topPx: 2280, heightPx: 10 })
  })

  it('同時刻に始まる区間は同じ縦位置になる（同時性の符号化）', () => {
    const a = spanToPx(axis, at(19 * 60), at(19 * 60 + 30))
    const b = spanToPx(axis, at(19 * 60), at(21 * 60))
    expect(a?.topPx).toBe(b?.topPx)
    expect(a?.heightPx).not.toBe(b?.heightPx)
  })

  it('軸の外にはみ出す分は切り落とす', () => {
    // 前日 23:00 から当日 1:00 まで（軸の手前が切られる）
    expect(spanToPx(axis, at(-60), at(60))).toEqual({ topPx: 0, heightPx: 120 })
    // 23:30 から翌 0:30 まで（軸の先が切られる）
    expect(spanToPx(axis, at(23 * 60 + 30), at(24 * 60 + 30))).toEqual({
      topPx: 23.5 * 120,
      heightPx: 60,
    })
  })

  it('軸と交差しない区間は null', () => {
    expect(spanToPx(axis, at(-120), at(-60))).toBeNull()
    expect(spanToPx(axis, at(24 * 60), at(25 * 60))).toBeNull()
    // 端で接するだけ（終了時刻 = 軸の開始）も交差ではない
    expect(spanToPx(axis, at(-60), at(0))).toBeNull()
  })
})

describe('hourTicks', () => {
  it('軸に含まれる毎時 0 分を返す', () => {
    const ticks = hourTicks(axis)
    expect(ticks).toHaveLength(24)
    expect(ticks[0]).toEqual({ ms: axis.startMs, topPx: 0 })
    expect(ticks[19]).toEqual({ ms: at(19 * 60), topPx: 19 * 120 })
    expect(new Date(ticks[19].ms).getHours()).toBe(19)
  })

  it('軸の開始が時刻境界でなければ次の 0 分から始まる', () => {
    const shifted: TimeAxis = {
      startMs: at(18 * 60 + 30),
      endMs: at(21 * 60),
      pxPerHour: 120,
    }
    const ticks = hourTicks(shifted)
    expect(ticks.map((t) => new Date(t.ms).getHours())).toEqual([19, 20])
    expect(ticks[0].topPx).toBe(60)
  })
})

describe('visibleTimeWindow', () => {
  it('未計測（高さ 0）なら軸全体を返す', () => {
    expect(visibleTimeWindow(axis, 0, 0, 400)).toEqual({
      startMs: axis.startMs,
      endMs: axis.endMs,
    })
  })

  it('スクロール位置とオーバースキャンから時間帯を出す', () => {
    // 2280px = 19:00 までスクロール、可視 600px = 5 時間、余白 120px = 1 時間
    expect(visibleTimeWindow(axis, 2280, 600, 120)).toEqual({
      startMs: at(18 * 60),
      endMs: at(25 * 60 - 60),
    })
  })

  it('軸の外へははみ出さない', () => {
    const window = visibleTimeWindow(axis, 0, 600, 400)
    expect(window.startMs).toBe(axis.startMs)

    const bottom = visibleTimeWindow(axis, axisHeightPx(axis) - 600, 600, 400)
    expect(bottom.endMs).toBe(axis.endMs)
  })
})

describe('visibleColumnRange', () => {
  it('未計測（幅 0）なら全列を返す', () => {
    expect(visibleColumnRange(40, 176, 0, 0, 1)).toEqual({ start: 0, end: 40 })
  })

  it('見えている列だけをオーバースキャンぶん広げて返す', () => {
    // 列幅 176px、左端 1760px（= 10 列目）、可視 704px（= 4 列）、余白 1 列
    expect(visibleColumnRange(40, 176, 1760, 704, 1)).toEqual({ start: 9, end: 15 })
  })

  it('列の総数と 0 を超えない', () => {
    expect(visibleColumnRange(40, 176, 0, 704, 1)).toEqual({ start: 0, end: 5 })
    expect(visibleColumnRange(6, 176, 0, 4000, 1)).toEqual({ start: 0, end: 6 })
  })
})

describe('groupProgramsByService', () => {
  const program = (programId: number, serviceId: number, startMinutes: number) => ({
    programId,
    serviceId,
    startAt: new Date(at(startMinutes)).toISOString(),
    endAt: new Date(at(startMinutes + 30)).toISOString(),
  })

  it('サービスごとに開始時刻の昇順で並べ、ms に解決する', () => {
    const grouped = groupProgramsByService([
      program(2, 1024, 19 * 60),
      program(1, 1024, 18 * 60),
      program(3, 2048, 18 * 60),
    ])

    expect([...grouped.keys()].sort()).toEqual([1024, 2048])
    expect(grouped.get(1024)?.map((p) => p.program.programId)).toEqual([1, 2])
    expect(grouped.get(1024)?.[0].startMs).toBe(at(18 * 60))
    expect(grouped.get(1024)?.[0].endMs).toBe(at(18 * 60 + 30))
    expect(grouped.get(2048)?.map((p) => p.program.programId)).toEqual([3])
  })
})

describe('orderServices', () => {
  const service = (
    serviceId: number,
    channelType: Service['channelType'],
    remoteControlKeyId: number,
  ): Service => ({
    networkId: 32736,
    serviceId,
    name: `service ${serviceId}`,
    channelType,
    channel: '27',
    remoteControlKeyId,
    hasLogoData: false,
    hasPrograms: true,
  })

  it('種別 → リモコン番号 → serviceId の全順序で並べる', () => {
    // リモコン番号と serviceId の大小が食い違うように組む（両者が一致する
    // データだと、比較キーを 1 つ落としても結果が変わらず検証にならない）
    const ordered = orderServices([
      service(400, 'BS', 4),
      service(1024, 'GR', 4),
      service(1032, 'GR', 1),
      service(1033, 'GR', 1),
      service(500, 'SKY', 1),
      service(450, 'CS', 1),
    ])

    expect(ordered.map((s) => s.serviceId)).toEqual([1032, 1033, 1024, 400, 450, 500])
  })

  it('入力を破壊しない', () => {
    const input = [service(1032, 'GR', 4), service(1024, 'GR', 1)]
    orderServices(input)
    expect(input.map((s) => s.serviceId)).toEqual([1032, 1024])
  })

  it('並び替え済みのサービスを種別ごとにまとめ、表示名を返す', () => {
    const groups = groupByChannelType(
      orderServices([
        service(400, 'BS', 4),
        service(1024, 'GR', 4),
        service(1032, 'GR', 1),
      ]),
    )

    expect(groups.map((group) => [group.channelType, group.services.map((s) => s.serviceId)])).toEqual([
      ['GR', [1032, 1024]],
      ['BS', [400]],
    ])
    expect(channelTypeLabel('GR')).toBe('地上波')
    expect(channelTypeLabel('UNKNOWN')).toBe('UNKNOWN')
  })
})

describe('serviceDisambiguator', () => {
  const service = (overrides: Partial<Service> & { serviceId: number }): Service => ({
    networkId: 32736,
    name: '瀬戸内海放送',
    channelType: 'GR',
    channel: '27',
    remoteControlKeyId: 5,
    hasLogoData: false,
    hasPrograms: true,
    ...overrides,
  })

  it('名前が重複しないサービスには何も返さない', () => {
    const services = [
      service({ serviceId: 1024, name: 'NHK総合' }),
      service({ serviceId: 1032, name: 'NHKEテレ' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBeUndefined()
    expect(disambiguate(services[1])).toBeUndefined()
  })

  it('リモコン番号が違えば地上波の種別とリモコン番号だけで区別する', () => {
    // 同名だがリモコン番号が違う（= 実際は別の局・別の中継局）ケース。
    // 物理チャンネルや serviceId まで見なくても 1 段目で解決できることを確認する
    // （2 段目・3 段目まで進んだら値が変わってしまうので検証になる）。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1032, remoteControlKeyId: 12, channel: '27' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 5')
    expect(disambiguate(services[1])).toBe('地上波 12')
  })

  it('リモコン番号まで同じワンセグ/サブサービスは物理チャンネルで区別する', () => {
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1025, remoteControlKeyId: 5, channel: '95' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 5 ・ 27')
    expect(disambiguate(services[1])).toBe('地上波 5 ・ 95')
  })

  it('リモコン番号・物理チャンネルまで同じなら serviceId で区別する', () => {
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1025, remoteControlKeyId: 5, channel: '27' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 5 ・ 27 ・ #32736-1024')
    expect(disambiguate(services[1])).toBe('地上波 5 ・ 27 ・ #32736-1025')
  })

  // mirakc は BS/CS のサービスに remoteControlKeyId を返さないため、`Service`
  // には常にゼロ値の 0 が載る（`internal/mirakc/types.go` の素の int →
  // `epg_services.remote_control_key_id` は NOT NULL → `internal/api/epg.go`
  // がそのまま 0 を返す）。つまり BS/CS のサービスは全件が
  // `remoteControlKeyId > 0` の**偽側**を通る = 本番の主経路であり、
  // 同名サブサービスが並ぶ族（issue #306 の報告そのもの）もここでしか描かれない。
  // 以下 2 件は「0 をリモコン番号として出さない」ことを期待値のリテラルで固定する
  // （この 2 件が無いと `> 0` を `>= 0` に反転しても全テストが緑のままだった）。
  it('リモコン番号を持たない BS は種別だけを出す（0 を番号として出さない）', () => {
    const services = [
      service({ serviceId: 101, channelType: 'BS', remoteControlKeyId: 0, channel: 'BS15_0' }),
      service({ serviceId: 102, channelType: 'BS', remoteControlKeyId: 0, channel: 'BS23_0' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('BS ・ BS15_0')
    expect(disambiguate(services[1])).toBe('BS ・ BS23_0')
  })

  it('リモコン番号を持たない BS と CS は種別だけで区別できる', () => {
    const services = [
      service({ serviceId: 101, channelType: 'BS', remoteControlKeyId: 0, channel: 'BS15_0' }),
      service({ serviceId: 102, channelType: 'CS', remoteControlKeyId: 0, channel: 'CS24' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('BS')
    expect(disambiguate(services[1])).toBe('CS')
  })

  // channel-picker.tsx のリモコン番号バッジは `channelType === 'GR'` も見て
  // いる（BS/CS には意味を持たない番号のため）。同じ材料を使う以上、こちらの
  // 判定も揃える。mirakc は BS/CS に 0 を返す（上のコメント参照）ので今の
  // 主経路では踏まないが、`channelType === 'GR'` を落として `> 0` だけに
  // 戻すと、mirakc が将来 BS/CS に非ゼロを返した場合にここだけ黒く落ちる。
  it('BS がリモコン番号を持っていても番号を出さない（GR 限定の判定）', () => {
    const services = [
      service({ serviceId: 101, channelType: 'BS', remoteControlKeyId: 4, channel: 'BS15_0' }),
      service({ serviceId: 102, channelType: 'BS', remoteControlKeyId: 4, channel: 'BS23_0' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('BS ・ BS15_0')
    expect(disambiguate(services[1])).toBe('BS ・ BS23_0')
  })

  it('渡した配列とは別オブジェクトでも同じ (networkId, serviceId) なら引ける', () => {
    // 引き当てをオブジェクト同一性でやると、`useListServices` の再取得で
    // 別オブジェクトになった瞬間にラベルが全部消える。キーはチップの identity
    // （networkId, serviceId）にしてあるので、複製でも同じラベルが出る。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1025, remoteControlKeyId: 5, channel: '95' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate({ ...services[0] })).toBe('地上波 5 ・ 27')
    expect(disambiguate({ ...services[1] })).toBe('地上波 5 ・ 95')
  })

  it('3 局以上の重複でも全員が区別できる（issue #306 の実例）', () => {
    const services = [
      service({ serviceId: 1, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 2, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 3, remoteControlKeyId: 6, channel: '30' }),
    ]
    const disambiguate = serviceDisambiguator(services)
    const labels = services.map((s) => disambiguate(s))

    expect(new Set(labels).size).toBe(3)
    expect(labels.every((l) => l !== undefined)).toBe(true)
  })
})
