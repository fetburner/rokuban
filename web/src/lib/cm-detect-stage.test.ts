import { describe, expect, it } from 'vitest'
import { cmDetectStageMessage, isStationFixableCMStage } from './cm-detect-stage'

describe('cmDetectStageMessage', () => {
  it('area と logo は枠・ロゴで直せる失敗として言い分ける', () => {
    expect(cmDetectStageMessage('area')).toBe('教えた枠が録画の解像度と合わないため、枠を使えませんでした。')
    expect(cmDetectStageMessage('logo')).toBe('ロゴを見つけられず、CM を検出できませんでした。')
  })

  it('resolution と match はロゴ側の失敗として言い分ける', () => {
    expect(cmDetectStageMessage('resolution')).toBe(
      '覚えたロゴは別の解像度の録画から作られたため、この録画には使えませんでした。',
    )
    expect(cmDetectStageMessage('match')).toBe(
      '覚えたロゴがこの録画にほとんど映っておらず、CM を検出できませんでした。局のロゴが変わった可能性があります。',
    )
  })

  it('adopt は局の判断待ちとして言い分ける', () => {
    expect(cmDetectStageMessage('adopt')).toBe('この局はロゴの採用待ちです。局の画面で候補を確かめて採用してください。')
  })

  it('それ以外の工程は枠では直せない失敗に畳む', () => {
    expect(cmDetectStageMessage('chapter')).toBe('CM 検出の処理が失敗しました。ロゴの枠では直せない失敗です。')
  })

  it('NULL・空・未知の値は古い試行として扱い、生の値を出さない', () => {
    for (const stage of [null, undefined, '', 'future-stage']) {
      expect(cmDetectStageMessage(stage)).toBe('失敗の種類が記録されていない古い試行です。')
    }
  })
})

describe('isStationFixableCMStage', () => {
  it.each(['logo', 'area', 'match', 'resolution', 'adopt'])('%s は局の CM ロゴ画面で直せる', (stage) => {
    expect(isStationFixableCMStage(stage)).toBe(true)
  })

  it.each(['setup', 'probe', 'chapter', 'join', 'parse', 'save', 'stopped', null, undefined, '', 'future-stage'])(
    '%s は局の CM ロゴ画面では直せない',
    (stage) => {
      expect(isStationFixableCMStage(stage)).toBe(false)
    },
  )
})
