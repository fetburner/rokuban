import { describe, expect, it } from 'vitest'

import type { Service } from '@/api/generated'
import { serviceDisambiguator } from '@/lib/service-label'

describe('serviceDisambiguator', () => {
  const service = (overrides: Partial<Service> & { serviceId: number }): Service => ({
    id: (overrides.networkId ?? 32736) * 100_000 + overrides.serviceId,
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

  it('リモコン番号が違えば地上波の3桁番号だけで区別する', () => {
    // 同名だがリモコン番号が違う（= 実際は別の局・別の中継局）ケース。
    // 物理チャンネルや serviceId まで見なくても 1 段目で解決できることを確認する
    // （2 段目・3 段目まで進んだら値が変わってしまうので検証になる）。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1032, remoteControlKeyId: 12, channel: '27' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 051')
    expect(disambiguate(services[1])).toBe('地上波 121')
  })

  it('リモコン番号まで同じマルチ編成は、物理チャンネルが同じでも違っても3桁番号（段1）だけで区別する', () => {
    // 物理チャンネルが同じ（同じ中継局）でも違う（別の中継局）でも、3 桁番号は
    // serviceId から決まるので段 1 だけで一意になる --- 物理チャンネル（段 2）
    // まで進んだら値が変わってしまうので、両ケースとも段 1 で止まることを見る。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1025, remoteControlKeyId: 5, channel: '95' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 051')
    expect(disambiguate(services[1])).toBe('地上波 052')

    const sameChannel = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1025, remoteControlKeyId: 5, channel: '27' }),
    ]
    const disambiguateSameChannel = serviceDisambiguator(sameChannel)

    expect(disambiguateSameChannel(sameChannel[0])).toBe('地上波 051')
    expect(disambiguateSameChannel(sameChannel[1])).toBe('地上波 052')
  })

  it('同名のマルチ編成 3 本を3桁番号だけで区別する', () => {
    const services = [
      service({ networkId: 32675, serviceId: 5144, remoteControlKeyId: 5, channel: '27' }),
      service({ networkId: 32675, serviceId: 5145, remoteControlKeyId: 5, channel: '27' }),
      service({ networkId: 32675, serviceId: 5146, remoteControlKeyId: 5, channel: '27' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(services.map((s) => disambiguate(s))).toEqual([
      '地上波 051',
      '地上波 052',
      '地上波 053',
    ])
  })

  it('3桁番号・物理チャンネルまで同じなら serviceId で区別する', () => {
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1032, remoteControlKeyId: 5, channel: '27' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 051 ・ 27 ・ #3273601024')
    expect(disambiguate(services[1])).toBe('地上波 051 ・ 27 ・ #3273601032')
  })

  // 判定は `channelType === 'GR' && remoteControlKeyId > 0` の 2 条件で、
  // 落ちる経路が別なので固定するテストも分ける。
  //
  // (1) `remoteControlKeyId > 0` を殺すには GR かつ 0 のフィクスチャが要る。
  // BS/CS のフィクスチャでは前置の `channelType === 'GR'` で短絡して
  // `> 0` に到達しないので、`>= 0` に反転しても落ちない。
  it('リモコン番号 0 の地上波は種別だけを出す（0 を番号として出さない）', () => {
    // mirakc が remoteControlKeyId を返さないと `Service` にはゼロ値の 0 が載る
    // （`internal/mirakc/types.go` の素の int →
    // `epg_services.remote_control_key_id` は NOT NULL → `internal/api/epg.go`
    // がそのまま 0 を返す）。「地上波 0」は存在しないリモコン番号なので、
    // 期待値をリテラルで固定して `> 0` を `>= 0` に反転すると落ちるようにする。
    const services = [
      service({ serviceId: 1024, channelType: 'GR', remoteControlKeyId: 0, channel: '27' }),
      service({ serviceId: 1025, channelType: 'GR', remoteControlKeyId: 0, channel: '95' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 ・ 27')
    expect(disambiguate(services[1])).toBe('地上波 ・ 95')
  })

  // (2) 以下 2 件が固定しているのは `channelType === 'GR'` の**偽側**
  // ---- BS/CS には番号を出さず種別だけを出すこと。BS/CS のサービスは
  // remoteControlKeyId が全件 0 なので（上のコメント参照）これが本番の主経路で、
  // 同名サブサービスが並ぶ族（issue #306 の報告そのもの）もここでしか描かれない。
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

  it('材料が空文字の段は区切りごと飛ばす', () => {
    // 今の API 契約では `channel` は required なので空文字は来ない（= 防御）。
    // 段の連結を「ここまでのラベルが空文字か」で代理すると、この入力で
    // `地上波 051 ・  ・ #3273601024` のように区切りだけが残る。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '' }),
      service({ serviceId: 1032, remoteControlKeyId: 5, channel: '' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 051 ・ #3273601024')
    expect(disambiguate(services[1])).toBe('地上波 051 ・ #3273601032')
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

    expect(disambiguate({ ...services[0] })).toBe('地上波 051')
    expect(disambiguate({ ...services[1] })).toBe('地上波 052')
  })

  it('3 局以上の重複でも全員が区別できる（issue #306 の実例）', () => {
    const services = [
      service({ serviceId: 5144, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 5145, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 5146, remoteControlKeyId: 5, channel: '27' }),
    ]
    const disambiguate = serviceDisambiguator(services)
    const labels = services.map((s) => disambiguate(s))

    expect(labels).toEqual(['地上波 051', '地上波 052', '地上波 053'])
  })

  it('同名グループのうち番組を持たない側だけ「番組なし」を足す', () => {
    // 主サービス（hasPrograms: true）とサブサービス（false）が
    // 同じリモコン番号・物理チャンネルで並ぶ族（issue #306 のレビュー指摘）。
    // 識別子だけでは「どちらが主サービスか」が伝わらないので、番組を
    // 持たない側にだけヒントを足す。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27', hasPrograms: true }),
      service({ serviceId: 1032, remoteControlKeyId: 5, channel: '27', hasPrograms: false }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 051 ・ 27 ・ #3273601024')
    expect(disambiguate(services[1])).toBe('地上波 051 ・ 27 ・ #3273601032 ・ 番組なし')
  })

  it('hasPrograms が両方 true なら「番組なし」は付かない', () => {
    // 反転させて検証する（片方だけ見ると反転しても気付かないため）。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27', hasPrograms: true }),
      service({ serviceId: 1032, remoteControlKeyId: 12, channel: '30', hasPrograms: true }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 051')
    expect(disambiguate(services[1])).toBe('地上波 121')
  })

  it('グループ内で一部だけ先に一意になっても段数を揃える', () => {
    // 3 本目（地上波 061）は段 1 だけで既に一意だが、残り 2 本（同じ networkId・
    // 同じリモコン番号で 3 桁番号も同じ）は段 2（物理チャンネル）まで進まないと
    // 区別できない。一意性判定はグループ全体（`new Set(labels).size ===
    // group.length`）で行うため、他の 2 本が段 2 まで進む間は 3 本目も同じ段数
    // まで足並みを揃える（3 本目だけ短い段 1 のラベルにはしない）。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27' }),
      service({ serviceId: 1032, remoteControlKeyId: 5, channel: '95' }),
      service({ serviceId: 2048, remoteControlKeyId: 6, channel: '30' }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(services.map((s) => disambiguate(s))).toEqual([
      '地上波 051 ・ 27',
      '地上波 051 ・ 95',
      '地上波 061 ・ 30',
    ])
  })

  it('同名グループ全員が番組を持たないなら「番組なし」は付かない', () => {
    // 「番組なし」は混在グループの false 側に付けるヒントなので、全員 false の
    // グループでは区別に何も寄与せず、どちらが主サービスかも言えない。
    // 全員 false は本番で起きる:
    // ① 初回 EPG 取得前は全サービスが hasPrograms: false になる
    //    （`openapi.yaml` の `Service.hasPrograms` の定義）
    // ② 同名のサブサービスが 2 本並び、どちらも番組を持たない族
    // 上乗せの条件から `group.some((s) => s.hasPrograms)` を落とすと（= 以前の
    // 「false なら全員に付ける」実装に戻すと）ここが落ちる。
    const services = [
      service({ serviceId: 1024, remoteControlKeyId: 5, channel: '27', hasPrograms: false }),
      service({ serviceId: 1025, remoteControlKeyId: 5, channel: '95', hasPrograms: false }),
    ]
    const disambiguate = serviceDisambiguator(services)

    expect(disambiguate(services[0])).toBe('地上波 051')
    expect(disambiguate(services[1])).toBe('地上波 052')
  })
})
