// やること:
//   ② 開いたページの上で色を実測して合否を出す（機械判定）。**この PR が変えた
//      状態色ぜんぶ**を覆う --- 1 箇所でも判定の外に置くと、そこだけ既定値へ
//      静かに戻っても全部緑のまま通る:
//      - 面（body / ヘッダ / ナビ / 一覧の行）の地が無彩か
//      - サイドバーの現在地の塗りと、2px オフセットしたフォーカスリングがページ地と 3:1 以上か
//      - 共通 Button の focus-visible リングが実画素で --scanline と一致し、ページ地と 3:1 以上か
//      - 録画中バッジがタリーレッドの「塗り」か / 失敗バッジが destructive の淡い地か
//      - チューナー不足・ルールの「条件なし」が琥珀か
//      - 番組リストの時刻に信号色が付いて**いない**か
//      - 現在時刻の線と札がタリーレッドか / 容量超過の帯の罫線が琥珀か
//      - `bg-muted` 系の面（塗り / `/80` の sticky 見出し / `/50` の行 hover /
//        `/30` の詳細パネル）に乗る文字
//      - 上記すべての WCAG コントラスト（文字 4.5 / 面と線 3）
//   ③ 和文が実際に Noto Sans JP で、英数字が実際に Geist で描画されているか
//      （CDP `CSS.getPlatformFontsForNode`）と、和文まじりの文字列でも
//      tabular-nums が実際に等幅を作っているか（DOM の実測幅）

import {
  FIXED_NOW,
  SITE2,
  apiHandler,
  expectedVisibleLabelTexts,
  fixturePairs,
} from './design-fixtures.mjs'
import {
  URL_BASE,
  checkMissingStrings,
  chroma,
  computedOf,
  computedPseudoOf,
  computedVar,
  contrast,
  createOpen,
  desktop,
  homeDesktop,
  isAmber,
  isRed,
  mobile,
  oklchChroma,
  readColor,
  readScreenshotPixels,
  recordingDetailScreen,
  sameRgb,
  screenOf,
  themes,
} from './design-support.mjs'
import {
  finish,
  installApiStubs,
  launchBrowser,
  log,
  validateFixturesOrExit,
  verifyBundleMatchesOrExit,
} from './lib.mjs'

const ng = []

log('\n=== 契約検証: フィクスチャの zod parse ===')
await validateFixturesOrExit(fixturePairs, ng)

// ⓪ 配っている bundle が dist/ の現物と一致するか（web/e2e/README.md 参照）。
log('\n=== ⓪ 配っている bundle と dist/ の一致 ===')
await verifyBundleMatchesOrExit(URL_BASE, ng)

const browser = await launchBrowser()
const open = createOpen(browser, ng)

log(`URL      : ${URL_BASE}`)
log(`固定時刻 : ${FIXED_NOW.toISOString()} (Asia/Tokyo)`)

// --- ② 色の機械判定 ---
//
// 判定は「この PR が変えた状態色ぜんぶ」を覆う。1 箇所でも判定の外に置くと、
// そこだけ既定値へ静かに戻っても全部緑のまま通る。
log('\n=== ② 色の判定 ===')

/** 小さい文字（バッジ・ラベル）に要求する WCAG 比。 */
const minTextContrast = 4.5
/** 面・線（非テキスト）に要求する WCAG 比。 */
const minUiContrast = 3

/** contrasts は測ったコントラストを表として溜める（合否とは別に、数値を人に見せる）。 */
const contrasts = []
function checkContrast(theme, label, fg, measured, floor) {
  // measured は `readColor` の戻り。背景側は必ずその `backdrop` を使う
  if (measured.reachedOpaque === false) {
    ng.push(`[${theme}] ${label}: 不透明な面まで遡れず、比を測れていない`)
    return null
  }
  const bg = measured.backdrop
  const ratio = contrast(fg, bg)
  contrasts.push({ theme, label, ratio, floor })
  if (ratio < floor) {
    ng.push(`[${theme}] ${label} のコントラストが ${ratio.toFixed(2)}（下限 ${floor}）`)
  }
  return ratio
}

function checkPixelContrast(theme, label, foreground, background, floor) {
  const ratio = contrast([...foreground, 255], [...background, 255])
  contrasts.push({ theme, label, ratio, floor })
  log(`  [${theme}] ${label}: ${ratio.toFixed(2)}:1 (下限 ${floor}:1)`)
  if (ratio < floor) {
    ng.push(`[${theme}] ${label} の実画素コントラストが ${ratio.toFixed(2)}（下限 ${floor}）`)
  }
  return ratio
}

// サイドバーの現在地とフォーカスリングは、DOM の色指定ではなく描画済み PNG の
// 画素で測る。選択行の左端、行の右側にあるページ地、2px オフセットリング上端、
// その外側の地を採る。リングの座標を行の外に固定することで、オフセット無しや
// ブラウザ既定の outline へ戻る退行を拾う。
for (const theme of themes) {
  const { context, page } = await open(homeDesktop, theme, screenOf('programs'))
  const sideNav = page.locator('nav[aria-label="主ナビゲーション"]').first()
  const current = sideNav.locator('a[aria-current="page"]')
  const currentCount = await current.count()
  if (currentCount !== 1) {
    ng.push(`[${theme}] サイドバーの現在地リンクが 1 件でない（${currentCount} 件）`)
    await context.close()
    continue
  }

  const item = current.first()
  await item.focus()
  const focusStyle = await item.evaluate((el) => {
    const style = getComputedStyle(el)
    return {
      focusVisible: el.matches(':focus-visible'),
      boxShadow: style.boxShadow,
      outlineStyle: style.outlineStyle,
      rect: (() => {
        const rect = el.getBoundingClientRect()
        return { x: rect.x, y: rect.y, width: rect.width, height: rect.height }
      })(),
    }
  })
  if (!focusStyle.focusVisible) {
    ng.push(`[${theme}] サイドバーの現在地リンクに :focus-visible が付かない`)
  }
  if (focusStyle.boxShadow === 'none') {
    ng.push(`[${theme}] サイドバーの現在地リンクに明示フォーカスリングが出ない`)
  }
  if (focusStyle.outlineStyle !== 'none') {
    ng.push(`[${theme}] サイドバーの現在地リンクにブラウザ outline が残っている（${focusStyle.outlineStyle}）`)
  }

  const { x, y, width, height } = focusStyle.rect
  const centerY = Math.floor(y + height / 2)
  const centerX = Math.floor(x + width / 2)
  const png = await page.screenshot({ scale: 'css' })
  const [itemPixel, pagePixel, ringPixel, ringBackdropPixel] = await readScreenshotPixels(page, png, [
    [Math.floor(x + 6), centerY],
    [Math.floor(x + width + 4), centerY],
    [centerX, Math.floor(y - 3)],
    [centerX, Math.floor(y - 6)],
  ])
  const scanline = await computedVar(page.locator('html'), '--scanline')
  log(
    `  [${theme}] サイドバー現在地: 行=${itemPixel} / ページ地=${pagePixel} / ` +
      `リング=${ringPixel} / リング外=${ringBackdropPixel}`,
  )
  checkPixelContrast(theme, 'サイドバー現在地 / ページ地', itemPixel, pagePixel, minUiContrast)
  checkPixelContrast(theme, 'サイドバーのフォーカスリング / ページ地', ringPixel, ringBackdropPixel, minUiContrast)
  if (scanline === null || !sameRgb(ringPixel, scanline.rgba)) {
    ng.push(
      `[${theme}] サイドバーのフォーカスリングの実画素が --scanline と一致しない` +
        `（画素=${ringPixel} / --scanline=${scanline?.rgba}）`,
    )
  }
  await context.close()
}

// 非現在地リンクと畳んだサイドバーのリングも、現在地と同じく実画素で測る。
// 非現在地はオフセット無しなので、行の左端のリングとその 2px 外側の地を採る（上端は先頭項目で ul の overflow に切られる）。
// `ring-ring/50` へ戻ると合成後の画素が --scanline から外れ、3:1 も割る。
for (const theme of themes) {
  for (const collapsed of [false, true]) {
    const mode = collapsed ? '畳んだサイドバー' : 'サイドバーの非現在地'
    const { context, page } = await open(homeDesktop, theme, screenOf('programs'))
    const sideNav = page.locator('nav[aria-label="主ナビゲーション"]').first()
    if (collapsed) {
      // クリックだと以降の focus() が :focus-visible にならないので、キーボードで畳む
      await page.getByRole('button', { name: 'ナビゲーションを畳む' }).focus()
      await page.keyboard.press('Enter')
    }
    const link = sideNav.locator('a:not([aria-current])').first()
    if ((await link.count()) === 0) {
      ng.push(`[${theme}] ${mode}のリンクが見つからずリングを画素で測れない`)
      await context.close()
      continue
    }
    await link.focus()
    const focused = await link.evaluate((el) => {
      const rect = el.getBoundingClientRect()
      return {
        focusVisible: el.matches(':focus-visible'),
        boxShadow: getComputedStyle(el).boxShadow,
        rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
      }
    })
    if (!focused.focusVisible || focused.boxShadow === 'none') {
      ng.push(`[${theme}] ${mode}のリンクに明示フォーカスリングが出ない`)
    }
    const ringY = Math.floor(focused.rect.y + focused.rect.height / 2)
    const png = await page.screenshot({ scale: 'css' })
    const [ringPixel, backdropPixel] = await readScreenshotPixels(page, png, [
      [Math.floor(focused.rect.x - 1), ringY],
      [Math.floor(focused.rect.x - 4), ringY],
    ])
    const scanline = await computedVar(page.locator('html'), '--scanline')
    log(`  [${theme}] ${mode}のリング=${ringPixel} / 外側の地=${backdropPixel}`)
    checkPixelContrast(theme, `${mode}のフォーカスリング / 地`, ringPixel, backdropPixel, minUiContrast)
    if (scanline === null || !sameRgb(ringPixel, scanline.rgba)) {
      ng.push(`[${theme}] ${mode}のフォーカスリングの実画素が --scanline と一致しない（画素=${ringPixel} / --scanline=${scanline?.rgba}）`)
    }
    await context.close()
  }
}

// 共有 Button のリングも、計算済みの border-color ではなく PNG の実画素で測る。
// `ring-ring/50` へ戻ると border は不透明なままなので `checkExplicitFocusRing` の
// border 比較だけでは見逃す。ボタン上端のリングとその外側の面を採り、半透明化で
// 合成された実際のコントラストが 3:1 を割ることを確認する。
for (const theme of themes) {
  const { context, page } = await open(homeDesktop, theme, screenOf('search'))
  const button = page.getByRole('button', { name: '検索', exact: true }).first()
  if ((await button.count()) === 0) {
    ng.push(`[${theme}] 共通 Button が見つからずフォーカスリングを画素で測れない`)
    await context.close()
    continue
  }

  await button.focus()
  const focused = await button.evaluate((el) => {
    const style = getComputedStyle(el)
    const rect = el.getBoundingClientRect()
    return {
      focusVisible: el.matches(':focus-visible'),
      boxShadow: style.boxShadow,
      outlineStyle: style.outlineStyle,
      rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
    }
  })
  if (!focused.focusVisible) {
    ng.push(`[${theme}] 共通 Button に :focus-visible が付かない`)
  }
  if (focused.boxShadow === 'none') {
    ng.push(`[${theme}] 共通 Button の明示フォーカスリングがない`)
  }
  if (focused.outlineStyle !== 'none') {
    ng.push(`[${theme}] 共通 Button にブラウザ outline が残っている（${focused.outlineStyle}）`)
  }

  const ringX = Math.floor(focused.rect.x + focused.rect.width / 2)
  const png = await page.screenshot({ scale: 'css' })
  const [ringPixel, backdropPixel] = await readScreenshotPixels(page, png, [
    [ringX, Math.floor(focused.rect.y - 1)],
    [ringX, Math.floor(focused.rect.y - 4)],
  ])
  const scanline = await computedVar(page.locator('html'), '--scanline')
  log(`  [${theme}] 共通 Button のリング=${ringPixel} / 外側の面=${backdropPixel}`)
  checkPixelContrast(theme, '共通 Button のフォーカスリング / 背景', ringPixel, backdropPixel, minUiContrast)
  if (scanline === null || !sameRgb(ringPixel, scanline.rgba)) {
    ng.push(
      `[${theme}] 共通 Button のフォーカスリングの実画素が --scanline と一致しない` +
        `（画素=${ringPixel} / --scanline=${scanline?.rgba}）`,
    )
  }
  await context.close()
}

for (const theme of themes) {
  // --- 録画一覧: 録画中 = タリーの塗り / 失敗 = destructive の淡い地 / 地は無彩 ---
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    // 一覧の行の中に限る（状態フィルタのチップにも同じ文言があるため）
    const badge = page.locator('ul span', { hasText: /^録画中$/ })
    const bg = await computedOf(badge, 'background-color')
    const fg = await computedOf(badge, 'color')
    log(`  [${theme}] 録画中バッジ 地=${bg?.value} ${bg?.rgba} / 文字=${fg?.value} ${fg?.rgba}`)
    if (bg === null) ng.push(`[${theme}] 録画中バッジが見つからない`)
    else if (bg.rgba[3] < 200) {
      // 淡い地（destructive の流儀 `bg-*/10`）に戻されたらここで落ちる
      ng.push(`[${theme}] 録画中バッジが塗りでない（不透明度 ${bg.rgba[3]}/255。${bg.value}）`)
    } else if (!isRed(bg.rgba)) {
      ng.push(`[${theme}] 録画中バッジの地がタリーレッドでない（${bg.value} = ${bg.rgba}）`)
    }
    if (fg !== null && chroma(fg.rgba) > 30) {
      ng.push(`[${theme}] 録画中バッジの文字に色が付いている（塗り + 無彩の文字であるべき。${fg.value}）`)
    }
    if (bg !== null && fg !== null && bg.rgba[3] >= 200) {
      // 塗りは不透明なので `bg.rgba` でも同値になるが、**背景側は必ず
      // `fg.backdrop` を渡す**。「どこかは自分の背景、どこかは合成後」と
      // 混ざっていると、次に構造が変わったときにどちらが正しいか分からなくなる
      checkContrast(theme, '録画中バッジの文字 / タリーの塗り', fg.rgba, fg, minTextContrast)
    }

    // 失敗バッジは destructive の「文字 + 淡い地」のまま
    const failed = page.locator('ul span', { hasText: /^録画失敗$/ })
    const failedBg = await computedOf(failed, 'background-color')
    const failedFg = await computedOf(failed, 'color')
    if (failedFg === null || failedBg === null) {
      ng.push(`[${theme}] 失敗バッジが見つからない`)
    } else {
      if (failedBg.rgba[3] > 200) {
        ng.push(`[${theme}] 失敗バッジが塗りになっている（destructive は文字 + 淡い地。${failedBg.value}）`)
      }
      if (!isRed(failedFg.rgba)) {
        ng.push(`[${theme}] 失敗バッジの文字が赤でない（${failedFg.value} = ${failedFg.rgba}）`)
      }
      checkContrast(
        theme,
        '失敗バッジの文字 / destructive の淡い地',
        failedFg.rgba,
        failedFg,
        minTextContrast,
      )
    }

    // 「準備中」結論バッジ（issue #1112）。`bg-muted` の文字色は
    // `text-foreground` とし、地が塗り（不透明）であることと文字との
    // コントラストが下限を満たすことを測る。
    const preparing = page.locator('ul span', { hasText: /^準備中$/ })
    const preparingBg = await computedOf(preparing, 'background-color')
    const preparingFg = await computedOf(preparing, 'color')
    if (preparingFg === null || preparingBg === null) {
      ng.push(`[${theme}] 準備中バッジが見つからない`)
    } else {
      if (preparingBg.rgba[3] < 200) {
        ng.push(`[${theme}] 準備中バッジの地が塗りでない（不透明度 ${preparingBg.rgba[3]}/255。${preparingBg.value}）`)
      }
      checkContrast(theme, '準備中バッジの文字 / muted の塗り', preparingFg.rgba, preparingFg, minTextContrast)
    }

    // 地は無彩。body だけを見ると「body に bg-background が当たっているか」しか
    // 言えないので、実際に面を持つ要素を回す。
    //
    // **測れなかったら NG にする。** セレクタが 0 件・面が透明のときに黙って
    // continue すると、「4 面を見ている」と書いてあるのに実際は 2 面しか
    // 見ていない、という状態が緑のまま続く（実際そうなっていた）
    for (const [name, selector] of [
      ['body', 'body'],
      ['ヘッダ', 'header'],
      // ナビは 2 つある（モバイルのボトムタブと md 以上のサイドバー）。
      // `.first()` だと背景を持たない方を掴むので両方回す
      ['ナビ', 'nav[aria-label="主ナビゲーション"]'],
      ['一覧の行', 'li a, li > div'],
    ]) {
      const nodes = await page.locator(selector).all()
      const surfaces = []
      for (const node of nodes) {
        const measured = await node.evaluate(readColor, 'background-color')
        surfaces.push(measured.backdrop)
      }
      if (surfaces.length === 0) {
        ng.push(`[${theme}] ${name}（${selector}）が 0 件で、地を判定できていない`)
        continue
      }
      const worst = surfaces.reduce((a, b) => (chroma(a) >= chroma(b) ? a : b))
      log(`  [${theme}] ${name} の地（${surfaces.length} 件中いちばん彩度が高いもの）= ${worst}`)
      if (chroma(worst) > 8) {
        ng.push(`[${theme}] ${name} の地が無彩でない（チャンネル差 ${chroma(worst)}。${worst}）`)
      }
    }

    // --- 接続断バナー（ConnectionBanner、issue #456）: 地は無彩 ---
    //
    // apiHandler は /api/events に明示のスタブを持たず catch-all（200 json []）
    // に落ちる（apiHandler の doc コメント参照）。Content-Type が
    // text/event-stream でないので EventSource は即座に失敗し、追加のスタブ
    // なしで「切断中」を作れる。disconnectedBannerDelayMs（lib/events.ts と
    // 同じ値をリテラルで書く。10 秒）が経てば帯が出るので、実時間で待ってから
    // 地を測る（`page.clock` はここでは使わない --- `setTimeout` は本物の
    // タイマーのまま動く。open() の `clock.setFixedTime` は Date だけを固定する）。
    //
    // waitFor の失敗だけ try/catch で ng.push に落とす --- 素通しすると帯が出ない
    // 変異で未捕捉例外がスクリプトごと中断し、後続の判定（このテーマの残り・
    // 他のスクリーンショット）と finish() の集計・ブラウザ後始末を丸ごと飛ばす。
    // `computedOf` / `chroma` まで同じ try に入れると、そちらが投げたときの NG が
    // 「帯が出ない」という食い違ったメッセージになるので外に出す。
    {
      const banner = page.locator('[role="status"]', { hasText: '更新通知が止まっています' })
      let bannerAppeared = true
      try {
        await banner.waitFor({ timeout: 10_000 + 5_000 })
      } catch {
        bannerAppeared = false
        ng.push(`[${theme}] 接続断バナーが disconnectedBannerDelayMs + 5 秒待っても出ない`)
      }
      if (bannerAppeared) {
        const bg = await computedOf(banner, 'background-color')
        log(`  [${theme}] 接続断バナーの地 = ${bg?.value} ${bg?.backdrop}`)
        if (bg === null) {
          ng.push(`[${theme}] 接続断バナーが見つからない`)
        } else if (chroma(bg.backdrop) > 8) {
          ng.push(`[${theme}] 接続断バナーの地が無彩でない（チャンネル差 ${chroma(bg.backdrop)}。${bg.backdrop}）`)
        }
      }
    }
    await context.close()
  }

  // --- 録画一覧: site タグ・IngestBadge の合成後コントラスト（issue #308 の
  //     レビューで判明した穴） ---
  //
  // 上のブロックは結論の「準備中」バッジだけを測る。site タグと IngestBadge も
  // muted の地を使う。site タグ（`showSite` が真になる 2 サイト以上でしか出ない）と
  // IngestBadge（`ingest` フィールドを持つ録画がないと出ない）は既定のフィクスチャ
  // では描画されず、`text-muted-foreground` に戻す変異が入っても緑のまま通っていた。
  // `multiSite` + `extraRecording` で両方を描画させて測る。
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'), {
      multiSite: true,
      extraRecording: true,
    })

    const siteTag = page.locator('ul span', { hasText: new RegExp(`^${SITE2}$`) })
    const siteTagBg = await computedOf(siteTag, 'background-color')
    const siteTagFg = await computedOf(siteTag, 'color')
    if (siteTagFg === null || siteTagBg === null) {
      ng.push(`[${theme}] 録画一覧の site タグが見つからない（showSite が効いていない?）`)
    } else {
      if (siteTagBg.rgba[3] < 200) {
        ng.push(`[${theme}] 録画一覧の site タグの地が塗りでない（不透明度 ${siteTagBg.rgba[3]}/255。${siteTagBg.value}）`)
      }
      checkContrast(theme, '録画一覧の site タグの文字 / muted の塗り', siteTagFg.rgba, siteTagFg, minTextContrast)
    }

    // ingest.state = transferring かつ expectedBytes 有りなので文言は「取り込み中 NN%」
    const ingestBadge = page.locator('ul span', { hasText: /^取り込み中/ })
    const ingestBg = await computedOf(ingestBadge, 'background-color')
    const ingestFg = await computedOf(ingestBadge, 'color')
    if (ingestFg === null || ingestBg === null) {
      ng.push(`[${theme}] IngestBadge が見つからない（ingestDisplay が undefined を返している?）`)
    } else {
      if (ingestBg.rgba[3] < 200) {
        ng.push(`[${theme}] IngestBadge の地が塗りでない（不透明度 ${ingestBg.rgba[3]}/255。${ingestBg.value}）`)
      }
      checkContrast(theme, 'IngestBadge の文字 / muted の塗り', ingestFg.rgba, ingestFg, minTextContrast)
    }

    await context.close()
  }

  // --- 録画一覧: 行の hover 中の副情報（`hover:bg-muted/40` + `text-muted-foreground`） ---
  //
  // 一覧の行は hover で `bg-muted/40` を敷き、その上に副情報（放送局名・日時・尺）が
  // `text-muted-foreground` のまま乗る。**Lighthouse は hover を測らない**ので
  // 監査には出ないが、`bg-muted` + `text-muted-foreground` と同族の組み合わせで
  // あることは変わらないので、下限を割るかどうかは推測せず実測する。同じ組み方は
  // 予約一覧・ホーム・番組リストの行にもあるが、地・文字のトークンと不透明度が
  // 同一なので代表として録画一覧の行で 1 回測る。
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    const row = page.locator('li').filter({ hasText: 'クラシック音楽館' }).first()
    // 副情報のうち、明示的な文字色を持たない素の span（バッジは text-foreground を
    // 明示しているので別の組み合わせになる）。
    const sub = row.locator('span', { hasText: /^ＮＨＫＢＳ$/ }).first()
    if ((await sub.count()) === 0) {
      ng.push(`[${theme}] 録画一覧の行の副情報（放送局名）が見つからない`)
    } else {
      const before = await sub.evaluate(readColor, 'color')
      await row.hover()
      // hover の背景は `transition-colors` を持たないので即時に乗るが、
      // 合成後の画素を読む前に 1 フレーム待つ
      await page.waitForTimeout(150)
      const after = await sub.evaluate(readColor, 'color')
      log(`  [${theme}] 一覧の行の副情報 文字=${after.value} / 乗っている面 hover 前=${before.backdrop} → hover 中=${after.backdrop}`)
      // **hover が本当に効いていることをここで検査する。** 効いていなければ
      // 測っているのは通常時の面で、「hover を測った」と言えるのに数字は
      // 通常時のもの、という空虚な成功になる（design.md「判定を足したことと、
      // それが効いていることは別」と同じ形の穴）
      const changed = [0, 1, 2].some((i) => Math.abs(after.backdrop[i] - before.backdrop[i]) >= 1)
      if (!changed) {
        ng.push(
          `[${theme}] 録画一覧の行を hover しても副情報が乗る面が変わらない（${after.backdrop}）` +
            ' --- hover の淡い地が効いていないか、locator が行の外を掴んでいる',
        )
      } else {
        checkContrast(
          theme,
          '一覧の行の hover 中の副情報の文字 / muted の半透明地',
          after.rgba,
          after,
          minTextContrast,
        )
      }
    }
    await context.close()
  }

  // --- 録画一覧: 選択中の行の副情報（`bg-muted/40` + `text-muted-foreground`） ---
  //
  // 選択モードで選んだ行は hover と同じ `bg-muted/40` が乗る（レビュー指摘。
  // 選択中だけ `bg-muted/50` のままだと副情報のコントラストが下限すれすれになる）。
  // hover と違って**常時見えるので Lighthouse の監査対象**に入るため、
  // 下限を割るかどうかは推測せず実測する。
  {
    const { context, page } = await open(desktop, theme, screenOf('recordings'))
    const row = page.locator('li').filter({ hasText: 'クラシック音楽館' }).first()
    const sub = row.locator('span', { hasText: /^ＮＨＫＢＳ$/ }).first()
    if ((await sub.count()) === 0) {
      ng.push(`[${theme}] 録画一覧の行の副情報（放送局名）が見つからない（選択中の判定）`)
    } else {
      const before = await sub.evaluate(readColor, 'color')
      await page.getByRole('button', { name: '選択' }).click()
      await page.getByRole('checkbox', { name: 'クラシック音楽館を選択' }).click()
      // hover が乗る位置のままだと測っているものが hover の面になるので、
      // 行からマウスを離してから測る。
      await page.mouse.move(0, 0)
      await page.waitForTimeout(150)
      const after = await sub.evaluate(readColor, 'color')
      log(`  [${theme}] 一覧の行（選択中）の副情報 文字=${after.value} / 乗っている面 選択前=${before.backdrop} → 選択中=${after.backdrop}`)
      // **選択が本当に効いていることをここで検査する。** hover ブロックと同じ
      // 形の穴 --- 効いていなければ測っているのは通常時の面で、数字は
      // 空虚な成功になる（design.md「判定を足したことと、それが効いていることは別」）。
      const changed = [0, 1, 2].some((i) => Math.abs(after.backdrop[i] - before.backdrop[i]) >= 1)
      if (!changed) {
        ng.push(
          `[${theme}] 録画一覧の行を選択しても副情報が乗る面が変わらない（${after.backdrop}）` +
            ' --- 選択中の淡い地が効いていないか、locator が行の外を掴んでいる',
        )
      } else {
        checkContrast(
          theme,
          '一覧の行の選択中の副情報の文字 / muted の半透明地',
          after.rgba,
          after,
          minTextContrast,
        )
      }
    }
    await context.close()
  }

  // --- 録画詳細: muted の文字をページ地の上で測る ---
  // 詳細本体には独立した面を置かないため、`<dt>`「チャンネル」の実効背景が
  // body のページ地と一致することを確かめ、そのページ地に対してコントラストを測る。
  // 代表として `<dt>` を使う。説明文・品質イベントも同じ色トークンを使う。
  {
    const { context, page } = await open(desktop, theme, recordingDetailScreen)
    // `screens`（① のループ）に無い画面なので、明示的に掛けないと欠損文字列
    // 判定から漏れる。
    //
    // **`s.packets.toLocaleString()`（components/drop-stats-table.tsx の
    // DropStatsTable）はここでは撮れていない** --- それは別エンドポイント
    // （`/api/recordings/{id}/drop-stats`。`ListRecordingDropStatsResponseItem`）
    // が返す per-PID の値で、`dropSummary.packets` とは無関係。この判定は
    // このエンドポイントを常に `[]` にスタブしているため（:312）、
    // `DropStatsTable` の行は 1 件も描画されない（未検証の断言をしない。
    // CLAUDE.md「一度も真でなかった記述」）。
    await checkMissingStrings(ng, page, `recording-detail/${theme}`)
    // 番組の明細（dt）は「番組」タブの中にある。デスクトップの既定は「版」なので開いてから測る。
    await page.getByRole('tab', { name: '番組' }).click()
    const dt = page.locator('dt', { hasText: /^チャンネル$/ }).first()
    const fg = await computedOf(dt, 'color')
    log(`  [${theme}] 録画詳細の文字=${fg?.value} ${fg?.rgba} / 実効背景=${fg?.backdrop}`)
    if (fg === null) {
      ng.push(`[${theme}] 録画詳細の <dt> が見つからない`)
    } else {
      const ground = await computedOf(page.locator('body'), 'background-color')
      const sameAsGround =
        ground !== null && [0, 1, 2].every((i) => Math.abs(fg.backdrop[i] - ground.backdrop[i]) < 1)
      if (ground === null || !sameAsGround) {
        ng.push(
          `[${theme}] 録画詳細の文字がページの地に直接乗っていない` +
            `（文字の実効背景=${fg.backdrop}、ページ地=${ground?.backdrop ?? '取得できない'}）`,
        )
      } else {
        checkContrast(
          theme,
          '録画詳細の muted text / page ground',
          fg.rgba,
          ground,
          minTextContrast,
        )
      }
    }
    await context.close()

    // 360px では詳細欄に余計な面・内側余白を付けず、本文を16pxで折り返す。
    // open() の既定（pointer: 'coarse' なら isMobile: true）をこの呼び出しだけ false で上書きする。
    // isMobile: true では Chromium が layout viewport を内容幅へ広げ、innerWidth が scrollWidth に
    // 追従して横はみ出し判定が空虚に通る（下の本文 40rem の負の対照で documentWidth 656 /
    // innerWidth 656 を light/dark とも実測）。そのため false にする。
    const mobileContext = await open(mobile, theme, recordingDetailScreen, { pointer: 'coarse', isMobile: false })
    const mobilePage = mobileContext.page
    // PID 表は「記録」タブの中にある（異常がある録画だけ）。スマホの既定は「番組」。
    await mobilePage.getByRole('tab', { name: '記録' }).click()
    await mobilePage.getByRole('heading', { name: 'PID 別ドロップ統計' }).waitFor({ state: 'visible' })
    const mobileLayout = await mobilePage.evaluate(() => {
      const body = document.querySelector('[data-testid="recording-detail-body"]')
      const description = document.querySelector('[data-testid="recording-description"]')
      if (!(body instanceof HTMLElement) || !(description instanceof HTMLElement)) return null
      const bodyStyle = getComputedStyle(body)
      const descriptionStyle = getComputedStyle(description)
      const range = document.createRange()
      range.selectNodeContents(description)
      const descriptionLines = range.getClientRects().length
      const bodyRect = body.getBoundingClientRect()
      return {
        viewportWidth: window.innerWidth,
        documentWidth: document.documentElement.scrollWidth,
        bodyLeft: bodyRect.left,
        bodyRight: bodyRect.right,
        bodyBackground: bodyStyle.backgroundColor,
        paddingLeft: bodyStyle.paddingLeft,
        paddingRight: bodyStyle.paddingRight,
        bodyFontSize: bodyStyle.fontSize,
        descriptionFontSize: descriptionStyle.fontSize,
        descriptionLines,
        pointerCoarse: matchMedia('(pointer: coarse)').matches,
      }
    })
    if (mobileLayout === null) {
      ng.push(`[${theme}] 録画詳細/mobile: 本文か説明文が見つからない`)
    } else {
      if (mobileLayout.documentWidth > mobileLayout.viewportWidth) {
        ng.push(`[${theme}] 録画詳細/mobile: 横スクロールが発生（${mobileLayout.documentWidth}px > ${mobileLayout.viewportWidth}px）`)
      }
      if (mobileLayout.viewportWidth !== mobile.width || !mobileLayout.pointerCoarse) {
        ng.push(`[${theme}] 録画詳細/mobile: 360px/coarse の条件を満たさない（${JSON.stringify(mobileLayout)}）`)
      }
      if (mobileLayout.bodyLeft < 0 || mobileLayout.bodyRight > mobileLayout.viewportWidth) {
        ng.push(`[${theme}] 録画詳細/mobile: 本文が viewport 外にはみ出す（left=${mobileLayout.bodyLeft}, right=${mobileLayout.bodyRight}）`)
      }
      if (mobileLayout.bodyBackground !== 'rgba(0, 0, 0, 0)' || mobileLayout.paddingLeft !== '0px' || mobileLayout.paddingRight !== '0px') {
        ng.push(`[${theme}] 録画詳細/mobile: 本文に背景色または重複した左右余白がある（bg=${mobileLayout.bodyBackground}, padding=${mobileLayout.paddingLeft}/${mobileLayout.paddingRight}）`)
      }
      if (mobileLayout.bodyFontSize !== '14px' || mobileLayout.descriptionFontSize !== '16px') {
        ng.push(`[${theme}] 録画詳細/mobile: 文字サイズが想定外（本文=${mobileLayout.bodyFontSize}, 説明=${mobileLayout.descriptionFontSize}）`)
      }
      if (mobileLayout.descriptionLines < 2) {
        ng.push(`[${theme}] 録画詳細/mobile: 説明文が360px幅で折り返されない（${mobileLayout.descriptionLines}行）`)
      }
      log(`  [${theme}] 録画詳細/mobile 360px: ${JSON.stringify(mobileLayout)}`)

      const pidDetails = mobilePage.getByTestId('drop-stats-details')
      await pidDetails.locator('summary').click()
      const tableViewport = pidDetails.locator('.overflow-x-auto')
      if ((await tableViewport.count()) === 0) {
        ng.push(`[${theme}] 録画詳細/mobile: PID 表の .overflow-x-auto 容器が見つからない`)
      } else {
        const tableOverflow = await tableViewport.evaluate((el) => el.scrollWidth > el.clientWidth)
        if (!tableOverflow) {
          ng.push(`[${theme}] 録画詳細/mobile: はみ出す PID 表が局所スクロール領域に収まっていない`)
        }
        // 負の対照: 容器の overflow を無効化すると、表が自力でページ幅を押し広げることを確かめる。
        await tableViewport.evaluate((el) => {
          el.style.overflowX = 'visible'
        })
        const noGuard = await mobilePage.evaluate(() => document.documentElement.scrollWidth)
        await tableViewport.evaluate((el) => {
          el.style.overflowX = ''
        })
        if (noGuard <= mobileLayout.viewportWidth) {
          ng.push(`[${theme}] 録画詳細/mobile: PID 表容器の overflow 負の対照で横はみ出しを検知できない（${noGuard}px）`)
        } else {
          log(`  [${theme}] 録画詳細/mobile 負の対照 PID 表 overflow 無効: ${noGuard}px > ${mobileLayout.viewportWidth}px`)
        }
      }

      // 負の対照: 本文に 40rem の最小幅を一時設定し、ページ幅判定が実際に
      // 横はみ出しを拾うことを確かめてからスタイルを戻す。
      const detailBody = mobilePage.locator('[data-testid="recording-detail-body"]')
      const originalMinWidth = await detailBody.evaluate((el) => el.style.minWidth)
      await detailBody.evaluate((el) => {
        el.style.minWidth = '40rem'
      })
      const brokenWidth = await mobilePage.evaluate(() => ({
        documentWidth: document.documentElement.scrollWidth,
        viewportWidth: window.innerWidth,
      }))
      await detailBody.evaluate((el, value) => {
        el.style.minWidth = value
      }, originalMinWidth)
      if (brokenWidth.documentWidth <= brokenWidth.viewportWidth) {
        ng.push(`[${theme}] 録画詳細/mobile: 本文の40rem負の対照で横はみ出しを検知できない（${JSON.stringify(brokenWidth)}）`)
      } else {
        log(`  [${theme}] 録画詳細/mobile 負の対照 本文 40rem: ${brokenWidth.documentWidth}px > ${brokenWidth.viewportWidth}px`)
      }
    }
    await mobileContext.context.close()
  }

  // --- 予約一覧: 容量不足 = 琥珀（淡い地の上で読めるか） ---
  // 2 つのバッジを測る。シリーズ行の集約バッジ（件数つき。外側の `<a>` が地と文字を持つ）と、
  // 時間順の行（と展開した各回）が使う `CapacityShortfallBadge`（外側の `<a>` が淡い地、
  // 内側の `span[aria-hidden="true"]` が文字）。後者は集約バッジに判定を入れ替えたとき
  // 一度消えたので、時間順のページを別に開いて測り続ける。
  for (const variant of [
    {
      name: '容量不足バッジ（シリーズの集約）',
      grouping: null,
      wait: 'text=容量不足',
      badge: (page) =>
        page.locator('[data-testid="reservation-series-row"] a').filter({ hasText: /容量不足/ }).first(),
      label: (badge) => badge,
    },
    {
      name: 'チューナー不足バッジ（時間順の行）',
      grouping: 'time',
      wait: 'text=チューナー不足',
      // **淡い地を持つのは外側のバッジ、文字を持つのは内側の span。**
      // 内側だけを掴むと背景が透明になり、合成が恒等になって「地の上での比」を
      // 測ってしまう。外側から引いて、文字色は子から採る。外側は `<span>` ではなく `<a>`（`Link`）。
      badge: (page) => page.locator('ul a').filter({ hasText: /チューナー不足/ }).first(),
      label: (badge) => badge.locator('span[aria-hidden="true"]'),
    },
  ]) {
    const { context, page } = await open(desktop, theme, { ...screenOf('reservations'), wait: variant.wait }, {
      reservationGrouping: variant.grouping,
    })
    const badge = variant.badge(page)
    const bg = await computedOf(badge, 'background-color')
    const fg = await computedOf(variant.label(badge), 'color')
    log(`  [${theme}] ${variant.name} 文字=${fg?.value} ${fg?.rgba} / 乗っている面=${fg?.backdrop}`)
    if (fg === null || bg === null) {
      ng.push(`[${theme}] ${variant.name}が見つからない`)
    } else if (bg.rgba[3] <= 8) {
      // 外側を掴めていない = 合成が効いていない。素通りさせず落とす
      ng.push(`[${theme}] ${variant.name}の地が透明（淡い地を持つ要素を掴めていない）`)
    } else {
      if (!isAmber(fg.rgba)) {
        ng.push(`[${theme}] ${variant.name}が琥珀でない（${fg.value} = ${fg.rgba}）`)
      }
      // `backdrop` の遡りが本当に効いているかを検査する。遡りが 1 段で止まると
      // `backdrop` はページの地と一致し、比は甘い方へ 0.5〜0.7 動く。一致 = 判定が壊れている
      const ground = await computedOf(page.locator('body'), 'background-color')
      const sameAsGround =
        ground !== null && [0, 1, 2].every((i) => Math.abs(fg.backdrop[i] - ground.backdrop[i]) < 1)
      if (sameAsGround) {
        ng.push(
          `[${theme}] ${variant.name}の文字が乗る面がページの地と同じ（${fg.backdrop}）` +
            ' --- 淡い地の合成が効いていない',
        )
      }
      checkContrast(theme, `${variant.name}の文字 / 琥珀の淡い地`, fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }

  // --- 番組リスト: 放送中の行は色を使わない（太さだけ） ---
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const airing = page.locator('[data-testid="program-row-time"]')
    const count = await airing.count()
    let colored = 0
    for (let i = 0; i < count; i++) {
      const c = await airing.nth(i).evaluate(readColor, 'color')
      if (chroma(c.rgba) > 30) colored++
    }
    log(`  [${theme}] 番組リストの時刻 ${count} 件中、色付き ${colored} 件`)
    if (count === 0) ng.push(`[${theme}] 番組リストの時刻が見つからない`)
    // リストの ON AIR は希少ではない（チャンネル数ぶん同時に点く）ので、
    // タリーにしてはならない。`text-tally` に戻したらここで落ちる
    if (colored > 0) {
      ng.push(`[${theme}] 番組リストの時刻に信号色が付いている（${colored} 件。太さで示す規律）`)
    }
    await context.close()
  }

  // --- 番組リスト: sticky 日付見出し（issue #308） ---
  //
  // `bg-muted/80` の半透明地の上に文字が乗る（`components/program-list.tsx`）。
  // 半透明なので `computedOf` の `readColor` が祖先まで遡って合成した
  // `backdrop` を使わないと、地の上での比だけを見てしまい甘い数字が出る
  // （「コントラストは毎回測る」参照）。文字色は `text-foreground` に直した
  // ので、`text-muted-foreground` に戻す変異が入ったらここで落ちる。
  //
  // 引くのは `data-testid`（`program-row-time` と同じ流儀）。`h2` の 1 番目で
  // 引くと「/programs の既定ビューが list」「/programs 上の h2 が日付見出しの
  // 1 種類だけ」の 2 つに依存し、将来 PageHeader 等に h2 が入ったときに
  // **別の要素を測ったまま通る**。
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const heading = page.locator('[data-testid="program-list-date-heading"]').first()
    const fg = await computedOf(heading, 'color')
    log(`  [${theme}] 番組リストの日付見出し 文字=${fg?.value} ${fg?.rgba} / 乗っている面=${fg?.backdrop}`)
    if (fg === null) {
      ng.push(`[${theme}] 番組リストの日付見出しが見つからない`)
    } else {
      checkContrast(theme, '番組リストの日付見出しの文字 / muted の半透明地', fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }

  // --- 番組表グリッド: 現在時刻の線と札 = タリー / 容量超過の帯 = 琥珀 ---
  // （グリッドは `lg` 以上でしか出ないのでデスクトップのみ）
  {
    const { context, page } = await open(desktop, theme, screenOf('programs'))
    const grid = page.getByRole('button', { name: '番組表' })
    if ((await grid.count()) === 0) {
      ng.push(`[${theme}] 表示形式の切り替えが出ていないのでグリッドを判定できない`)
    } else {
      await grid.first().click()
      await page.locator('[data-testid="program-grid-now-line"]').waitFor({ timeout: 10000 })
      await page.waitForTimeout(400)

      const line = await computedOf(page.locator('[data-testid="program-grid-now-line"]'), 'border-top-color')
      log(`  [${theme}] 現在時刻線 = ${line?.value} ${line?.rgba}`)
      if (line === null) ng.push(`[${theme}] 現在時刻線が見つからない`)
      else if (!isRed(line.rgba)) {
        ng.push(`[${theme}] 現在時刻線がタリーレッドでない（${line.value} = ${line.rgba}）`)
      }

      // 現在時刻の札は「塗り」。11px の赤い文字はダークの地で AA に届かないので、
      // タリーは塗りにしか使わない（design.md「タリーは塗り」）
      const chip = page.locator('[data-testid="program-grid-now-label"] span')
      const chipBg = await computedOf(chip, 'background-color')
      const chipFg = await computedOf(chip, 'color')
      log(`  [${theme}] 現在時刻の札 地=${chipBg?.value} ${chipBg?.rgba} / 文字=${chipFg?.rgba}`)
      if (chipBg === null || chipFg === null) ng.push(`[${theme}] 現在時刻の札が見つからない`)
      else {
        if (chipBg.rgba[3] < 200 || !isRed(chipBg.rgba)) {
          ng.push(`[${theme}] 現在時刻の札がタリーの塗りでない（${chipBg.value}）`)
        } else {
          checkContrast(theme, '現在時刻の札の文字 / タリーの塗り', chipFg.rgba, chipFg, minTextContrast)
        }
      }

      // 放送終了セル。ジャンル淡色の上に muted/30 を重ね、foreground を半透明にする。
      // `::before` は通常の backdrop 探索では拾えないので、疑似要素の色をセルの
      // 実効面へ合成してから文字との比を測る。
      const endedCell = page.locator('[data-testid="program-grid-cell"][data-ended="true"]').first()
      const endedFg = await computedOf(endedCell, 'color')
      const endedOverlay = await computedPseudoOf(endedCell, 'background-color', '::before')
      if (endedFg === null || endedOverlay === null) {
        ng.push(`[${theme}] 放送終了セルが見つからずコントラストを判定できない`)
      } else if (endedOverlay.rgba[3] === 0) {
        ng.push(`[${theme}] 放送終了セルの減光面が透明`)
      } else {
        const alpha = endedOverlay.rgba[3] / 255
        const endedBackdrop = endedOverlay.rgba
          .slice(0, 3)
          .map((c, i) => c * alpha + endedOverlay.backdrop[i] * (1 - alpha))
        endedBackdrop.push(255)
        log(
          `  [${theme}] 放送終了セル 文字=${endedFg.value} ${endedFg.rgba}` +
            ` / 減光後の面=${endedBackdrop}`,
        )
        checkContrast(
          theme,
          '放送終了セルの文字 / ジャンル淡色と muted の合成面',
          endedFg.rgba,
          { ...endedFg, backdrop: endedBackdrop },
          minTextContrast,
        )
      }

      // 容量超過の帯。罫線が区間の境界を伝えるので、線が琥珀であることを見る。
      //
      // 帯が重なっているのは番組セル（ジャンル淡色）で、**セルは帯の祖先ではなく
      // 兄弟**なので `backdrop` では拾えない。そこでグリッドのセルを**全件**集めて、
      // 罫線に対していちばん不利な面に対して測る（どのセルと実際に重なっているかは
      // 判定しない --- 全件は安全側の過大集合で、見落とす方向には倒れない）。
      // 帯自身の `backdrop` も候補に入れる: `background-clip` の既定は `border-box`
      // なので、罫線は自分の淡い地の上に描かれる
      const band = page.locator('[data-testid="capacity-band"]')
      const bandBorder = await computedOf(band, 'border-top-color')
      log(`  [${theme}] 容量超過の帯の罫線 = ${bandBorder?.value} ${bandBorder?.rgba}`)
      if (bandBorder === null) ng.push(`[${theme}] 容量超過の帯が見つからない`)
      else {
        if (!isAmber(bandBorder.rgba)) {
          ng.push(`[${theme}] 容量超過の帯の罫線が琥珀でない（${bandBorder.value} = ${bandBorder.rgba}）`)
        }
        const cells = await page.locator('[data-testid="program-grid-cell"]').all()
        const surfaces = [bandBorder.backdrop]
        for (const cell of cells) {
          surfaces.push((await cell.evaluate(readColor, 'background-color')).backdrop)
        }
        // 罫線の色に対していちばんコントラストが低くなる面を選ぶ
        const worst = surfaces.reduce((a, b) =>
          contrast(bandBorder.rgba, a) <= contrast(bandBorder.rgba, b) ? a : b,
        )
        log(`  [${theme}] 帯が重なる面 ${surfaces.length} 種のうち最も不利なもの = ${worst}`)
        checkContrast(theme, '容量超過の帯の罫線 / 最も不利な面', bandBorder.rgba, { backdrop: worst }, minUiContrast)
      }

      // 帯ラベルとセルの時刻文字の重なり（issue #460）。帯の上端がセルの上端に
      // 近いと、見た目のラベル（「BS-1」等）とセルの時刻文字（「23:30」）が
      // 同じ px に描かれてどちらも読めなくなる（ライトの
      // `programs-grid-light-desktop.png` で実際に確認済み）。jsdom はレイアウトを
      // 計算しないのでここでしか測れない --- rect の非交差を機械判定する。
      const labelHandles = await page.locator('[data-testid="capacity-band-label"]').all()
      const labelBoxes = []
      for (const l of labelHandles) {
        const box = await l.boundingBox()
        if (box === null) continue
        // 幅の切れは実際に truncate が効く内側の要素（アイコン分の幅を除いた
        // テキスト span）で測る --- 外側の箱はアイコン込みで常に時間軸列いっぱい
        // に張るので、外側だけを見ると切れを見落とす。
        const overflow = await l.evaluate((el) => {
          const textEl = el.querySelector('[data-testid="capacity-band-label-text"]') ?? el
          return { clientWidth: textEl.clientWidth, scrollWidth: textEl.scrollWidth, text: el.textContent }
        })
        labelBoxes.push({ ...box, ...overflow })
      }
      const cellTimeBoxes = (
        await Promise.all(
          (await page.locator('[data-testid="program-grid-cell-time"]').all()).map((c) =>
            c.boundingBox(),
          ),
        )
      ).filter((b) => b !== null)
      log(`  [${theme}] 帯ラベル ${labelBoxes.length} 件 / セル時刻 ${cellTimeBoxes.length} 件`)
      if (labelBoxes.length === 0) {
        ng.push(`[${theme}] 容量超過の帯ラベルが見つからない`)
      }

      const rectsIntersect = (a, b) =>
        a.x < b.x + b.width &&
        a.x + a.width > b.x &&
        a.y < b.y + b.height &&
        a.y + a.height > b.y

      // レビュー should 1: フィクスチャ（`overages`）に隣接する（重ならない）
      // 帯を複数入れてある。同一サイト内の不足区間はサーバー側で重ならないと
      // 保証されている（`internal/capacity/capacity.go` の `Compute`）ので
      // 「同時刻に重なる帯」はフィクスチャとしても不適切 --- ここで見るのは
      // 「隣接する帯のラベルがそれぞれ独立に見えるか」だけ。
      //
      // 件数ではなく集合（`expectedVisibleLabelTexts`）で照合する。件数だけだと
      // 「隠しているはずの CS 帯が描かれ、同時に別の帯のラベルが消える」変異が
      // 合計件数の一致で素通りする（issue #460 再々レビュー）。
      const actualLabelTexts = [...labelBoxes.map((l) => l.text)].sort()
      const wantLabelTexts = [...expectedVisibleLabelTexts].sort()
      if (JSON.stringify(actualLabelTexts) !== JSON.stringify(wantLabelTexts)) {
        ng.push(
          `[${theme}] 見えるラベルの集合が期待と異なる` +
            `（期待 ${JSON.stringify(wantLabelTexts)} / 実際 ${JSON.stringify(actualLabelTexts)}）`,
        )
      }
      const labelOverlaps = []
      for (let i = 0; i < labelBoxes.length; i++) {
        for (let j = i + 1; j < labelBoxes.length; j++) {
          if (rectsIntersect(labelBoxes[i], labelBoxes[j])) {
            labelOverlaps.push([labelBoxes[i], labelBoxes[j]])
          }
        }
      }
      if (labelOverlaps.length > 0) {
        log(`  [${theme}] ラベル同士の重なり ${JSON.stringify(labelOverlaps[0])}`)
        ng.push(
          `[${theme}] 帯ラベル同士の rect が ${labelOverlaps.length} 件重なっている` +
            '（同時刻の帯が積まれず、片方が隠れている）',
        )
      }

      // レビュー blocker 1: 時間軸列（56px）に収まらず省略記号の外に文字が
      // 切れていないか。scrollWidth が clientWidth を超えていれば、見えている
      // 分の外に切れた文字がある（「BS-1」のような短い形のはずが「チューナ…」
      // まで切れて種別も本数も読めなくなった実例がレビューで見つかった）。
      for (const l of labelBoxes) {
        if (l.scrollWidth > l.clientWidth) {
          ng.push(
            `[${theme}] 帯ラベル「${l.text}」が時間軸列の幅で切れている` +
              `（clientWidth ${l.clientWidth} / scrollWidth ${l.scrollWidth}）`,
          )
        }
      }

      // レビュー blocker 2: ラベルが帯の全高を塗って時間軸の目盛りや現在時刻
      // チップを消していないか。ラベルは自分の内容ぶんの小さい高さしか
      // 持たないはず（旧実装は帯の高さぶん引き伸ばしていた --- 3 時間の帯なら
      // ラベルの高さも 3 時間ぶんの px になっていた）。
      const maxReasonableLabelHeightPx = 24
      for (const l of labelBoxes) {
        if (l.height > maxReasonableLabelHeightPx) {
          ng.push(
            `[${theme}] 帯ラベルの高さが ${l.height}px --- 帯の全高を塗って` +
              `目盛りを消している疑い（上限の目安 ${maxReasonableLabelHeightPx}px）`,
          )
        }
      }
      // 上の高さ判定を回避しても実際に目盛りが隠れていないかは直接見る ---
      // レビューの実測（`coveredTicks: ["00:00"]`）と同じものをここで測る。
      // 判定は rect の交差（`rectsIntersect`）にする --- 「全高を包含する
      // か」だと、ラベル（16px）が目盛り（実測 18.5px）より低い限り算術的に
      // 真になり得ず、判定として永久に発火しない（レビューで指摘）。
      const tickBoxes = (
        await Promise.all(
          (await page.locator('[data-testid="program-grid-tick"]').all()).map((t) =>
            t.boundingBox(),
          ),
        )
      ).filter((b) => b !== null)
      const coveredTicks = []
      for (const t of tickBoxes) {
        for (const l of labelBoxes) {
          if (rectsIntersect(l, t)) coveredTicks.push(t)
        }
      }
      if (coveredTicks.length > 0) {
        log(`  [${theme}] coveredTicks: ${coveredTicks.length} 件`)
        ng.push(`[${theme}] 帯ラベルが時間軸の目盛りと ${coveredTicks.length} 件重なっている`)
      }

      // レビュー should 2: 前提条件そのものを表明する。帯の上端付近にセルの
      // 時刻要素が無ければ、下の非交差判定は「そもそも重なりようがない」
      // だけで通ってしまい、`FIXED_NOW` や `gridPxPerHour` を変えただけで
      // 気付かず空虚に通るようになる。
      const nearPx = 20
      const hasAdjacentCell = labelBoxes.some((l) =>
        cellTimeBoxes.some((c) => Math.abs(c.y - l.y) <= nearPx),
      )
      if (!hasAdjacentCell) {
        ng.push(
          `[${theme}] 前提条件が崩れている: 帯ラベルの上端付近（${nearPx}px 以内）に` +
            'セルの時刻要素が無い。以降の非交差判定はこの回では何も検証していない',
        )
      }

      const overlaps = []
      for (const l of labelBoxes) {
        for (const c of cellTimeBoxes) {
          if (rectsIntersect(l, c)) overlaps.push({ label: l, cell: c })
        }
      }
      if (overlaps.length > 0) {
        log(`  [${theme}] 重なり ${JSON.stringify(overlaps[0])}`)
        ng.push(
          `[${theme}] 帯ラベルとセルの時刻文字の rect が ${overlaps.length} 件重なっている`,
        )
      }
    }
    await context.close()
  }

  // --- ルール一覧: 「条件なし」の警告 = 琥珀 ---
  {
    const { context, page } = await open(desktop, theme, screenOf('rules'))
    const warn = page.locator('span', { hasText: /^条件なし（すべての番組にマッチ）$/ })
    const fg = await computedOf(warn, 'color')
    log(`  [${theme}] ルールの「条件なし」= ${fg?.value} ${fg?.rgba}`)
    if (fg === null) ng.push(`[${theme}] ルールの「条件なし」警告が見つからない`)
    else {
      if (!isAmber(fg.rgba)) {
        ng.push(`[${theme}] ルールの「条件なし」が琥珀でない（${fg.value} = ${fg.rgba}）`)
      }
      checkContrast(theme, '「条件なし」の文字 / 乗っている面', fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }

  // --- 空状態: 読み込み中と異なる中立の地（EmptyState。components/page.tsx） ---
  {
    const { context, page } = await open(desktop, theme, screenOf('search'))
    const empty = page.getByText('条件を指定して検索してください', { exact: true })
    const fg = await computedOf(empty, 'color')
    const emptyStyle = fg === null ? null : await empty.evaluate((element) => {
      const container = element.closest('div')
      return {
        hasScanlines: element.closest('.scanlines') !== null,
        backgroundImage: container === null ? 'missing' : getComputedStyle(container).backgroundImage,
      }
    })
    log(`  [${theme}] EmptyState の地=${fg?.backdrop} / 文字=${fg?.value} / 縞=${emptyStyle?.hasScanlines}`)
    if (fg === null) {
      ng.push(`[${theme}] 検索の未検索 EmptyState が見つからない`)
    } else {
      if (emptyStyle?.hasScanlines || emptyStyle?.backgroundImage !== 'none') {
        ng.push(`[${theme}] EmptyState に読み込み中と同じ背景模様がある`)
      }
      checkContrast(theme, '空状態の文字 / ページ地', fg.rgba, fg, minTextContrast)
    }
    await context.close()
  }

  // --- 読み込み中: Skeleton / ListSkeleton の走査線（components/page.tsx） ---
  //
  // 文字は乗らないプレースホルダなので AA の対象ではない。ここで見るのは
  // 縞の 2 色（`--scan-gap` / `--scan-lit`）が両方とも不透明・無彩かという
  // 構造の存在確認。API が即座に返る作りだと遷移直後に解決してしまうので、
  // `/api/recordings` だけ遅延させて捕まえる
  {
    const context = await browser.newContext({
      viewport: { width: desktop.width, height: desktop.height },
      locale: 'ja-JP',
      timezoneId: 'Asia/Tokyo',
      colorScheme: theme,
      deviceScaleFactor: 2,
    })
    const page = await context.newPage()
    await page.clock.setFixedTime(FIXED_NOW)
    await installApiStubs(page, apiHandler({ delayPath: '/api/recordings', delayMs: 5000 }))
    await page.goto(URL_BASE + '/recordings', { waitUntil: 'domcontentloaded' })
    const skeleton = page.locator('.scanlines').first()
    await skeleton.waitFor({ timeout: 5000 }).catch(() => {
      ng.push(`[${theme}] 読み込み中の走査線（.scanlines）が出ない`)
    })
    const gap = await computedOf(skeleton, 'background-color')
    const lit = await computedVar(skeleton, '--scan-lit')
    // bgImage の NG（下記）は gap/lit が両方とも非 null（＝要素が存在した）の
    // 分岐でしか出さないので、ここでの null は要素消失ではなくスタイルそのものの
    // 欠如を指す。`.catch(() => null)` で飲んでよい。
    const bgImage = await skeleton
      .evaluate((el) => getComputedStyle(el).backgroundImage)
      .catch(() => null)
    log(
      `  [${theme}] 読み込み中の走査線 間隙=${gap?.value} ${gap?.rgba} / 輝線=${lit?.value} ${lit?.rgba} / ` +
        `background-image=${bgImage && bgImage !== 'none' ? 'あり' : 'なし'}`,
    )
    if (gap === null || lit === null) {
      ng.push(`[${theme}] 読み込み中（Skeleton）の走査線が見つからない`)
    } else {
      if (gap.rgba[3] < 200) {
        ng.push(`[${theme}] 読み込み中の走査線の間隙が不透明でない（${gap.value}）`)
      }
      if (lit.rgba[3] < 200) {
        ng.push(`[${theme}] 読み込み中の走査線の輝線が不透明でない（${lit.value}）`)
      }
      for (const [side, measured] of [
        ['間隙', gap],
        ['輝線', lit],
      ]) {
        const c = oklchChroma(measured.value)
        if (c === null || c > 0.02) {
          ng.push(`[${theme}] 読み込み中の走査線の${side}が無彩でない（oklch chroma ${c}。${measured.value}）`)
        }
      }
      if (bgImage === null || bgImage === 'none') {
        ng.push(`[${theme}] 読み込み中に走査線の輝線（background-image）が無い（${bgImage}）`)
      }
    }
    await context.close()
  }

  // --- ライブ: ON AIR バッジ = タリーの塗り + 走査線（pages/live.tsx OnAirBadge） ---
  //
  // 縞の 2 色（`--scan-gap` = `--tally` そのもの / `--scan-lit` = `--tally` を
  // 明度だけ落とした段。index.css の `.tally-scanlines` 参照）を両方測る。
  // タリーは既定で赤なので両方に `isRed` を掛け、文字とのコントラストも
  // 両方で見る --- 間隙だけでは「輝線を文字と同じ色にする」変異
  // （グリフの半分が塗りに溶ける）を見逃す
  {
    const { context, page } = await open(desktop, theme, screenOf('live'))
    const badge = page.locator('span.tally-scanlines', { hasText: /^ON AIR$/ }).first()
    const gap = await computedOf(badge, 'background-color')
    const lit = await computedVar(badge, '--scan-lit')
    const fg = await computedOf(badge, 'color')
    log(
      `  [${theme}] ON AIR バッジ 間隙=${gap?.value} ${gap?.rgba} / ` +
        `輝線=${lit?.value} ${lit?.rgba} / 文字=${fg?.value} ${fg?.rgba}`,
    )
    if (gap === null || lit === null || fg === null) {
      ng.push(`[${theme}] ON AIR バッジが見つからない（いま放送中の番組が無いスタブになっていないか）`)
    } else {
      if (chroma(fg.rgba) > 30) {
        ng.push(`[${theme}] ON AIR バッジの文字に色が付いている（塗り + 無彩の文字であるべき。${fg.value}）`)
      }
      for (const [side, measured] of [
        ['間隙', gap],
        ['輝線', lit],
      ]) {
        if (measured.rgba[3] < 200) {
          ng.push(`[${theme}] ON AIR バッジの${side}が塗りでない（不透明度 ${measured.rgba[3]}/255。${measured.value}）`)
          continue
        }
        if (!isRed(measured.rgba)) {
          ng.push(`[${theme}] ON AIR バッジの${side}がタリーレッドでない（マゼンタ等への色相ずれの疑い。${measured.value} = ${measured.rgba}）`)
        }
      }
      // **地に対する比だけを見ると甘い数字が出る。** 間隙（`--tally` そのもの。
      // 録画中バッジと同じ値）・輝線（`--tally` を明度だけ落とした段）の
      // 両方を文字色との比で見る。輝線は間隙より暗いので理屈のうえでは
      // 間隙側が不利なはずだが、**「そのはず」を判定の根拠にはしない** ---
      // 両方を実測して両方に下限を掛けることで、想定が外れても気付ける形にする
      if (gap.rgba[3] >= 200) {
        checkContrast(theme, 'ON AIR バッジの文字 / タリー走査線の間隙', fg.rgba, fg, minTextContrast)
      }
      if (lit.rgba[3] >= 200) {
        checkContrast(theme, 'ON AIR バッジの文字 / タリー走査線の輝線', fg.rgba, { backdrop: lit.rgba }, minTextContrast)
      }
      // この分岐は gap/lit/fg が全て非 null（＝バッジが存在した）ときにしか
      // 入らないので、ここでの null は要素消失ではなくスタイルの欠如を指す。
      // `.catch(() => null)` で飲んでよい。
      const bgImage = await badge
        .evaluate((el) => getComputedStyle(el).backgroundImage)
        .catch(() => null)
      if (bgImage === null || bgImage === 'none') {
        ng.push(`[${theme}] ON AIR バッジに走査線の輝線（background-image）が無い（${bgImage}）`)
      }
    }
    await context.close()
  }
}

// --- ③ フォントの実描画判定 ---
//
// **`getComputedStyle().fontFamily` は指定した文字列を返すだけで、ブラウザが
// 実際にどのフォントを選んで描画したかは別**（docs/frontend/stack.md「フォント
// は英数字と和文で 2 書体を使い分ける」）。CDP の `CSS.getPlatformFontsForNode`
// で実際に使われたフォントを見る。フォントファイルが unicode-range で分割
// されていて、かつ Noto Sans JP の import が消えても `--font-sans` の
// フォールバック先（システムフォント）が和文をレンダリングできてしまうため、
// **この判定を外すと「Noto Sans JP を削除して和文がシステムフォントに戻る」
// 事故がスクリーンショット上は気付かれないまま緑で通り続ける**。
log('\n=== ③ フォントの判定 ===')

/**
 * platformFontsOf は CDP 経由で selector に一致するノードの実使用フォントを返す。
 *
 * **`cdp` は呼び出し元が `DOM.enable` / `CSS.enable` を送信済みのセッションで
 * あること。** `CSS.enable` を呼ばずに `CSS.getPlatformFontsForNode` を呼ぶと
 * **空配列ではなく protocol error で throw する**
 * （`Protocol error (CSS.getPlatformFontsForNode): CSS agent was not enabled`。
 * 実測で確認済み）。セッションを作るタイミングはナビゲーションの前後どちらでも
 * 結果は変わらない（両方実測済み。以前このファイルに「ナビゲーション後に
 * セッションを作ると空配列が返る」という誤った記述があったが、それは次の罠を
 * 誤って帰属したものだった）。
 *
 * **本当の罠は selector の選び方。** `main` や `body` のような「直接はテキストを
 * 持たずブロック要素だけを子に持つ」要素を渡すと常に空配列が返る（throw ではない）。
 * `CSS.getPlatformFontsForNode` はノード自身のインラインレイアウト（実際に
 * テキストランを持つ層）に紐付いたフォント使用だけを返し、ブロックの子孫を
 * 再帰集約しない。実際にテキストを直接持つ要素（番組リストの行
 * `li[data-program-id]` 等）を渡す必要がある。
 */
async function platformFontsOf(cdp, selector) {
  const { root } = await cdp.send('DOM.getDocument')
  const { nodeId } = await cdp.send('DOM.querySelector', { nodeId: root.nodeId, selector })
  if (!nodeId) return null
  const { fonts } = await cdp.send('CSS.getPlatformFontsForNode', { nodeId })
  return fonts.map((f) => `${f.familyName} x${f.glyphCount}`)
}

{
  const { context, page } = await open(desktop, 'light', screenOf('programs'))
  // このブロックでしか CDP を使わないので、ここでセッションを作って有効化する
  // （`open()` は全画面 × テーマ × ビューポートで呼ばれるので、そちらに置くと
  // 使わない呼び出しでも毎回セッションを作ることになる）。
  const cdp = await context.newCDPSession(page)
  await cdp.send('DOM.enable')
  await cdp.send('CSS.enable')

  // 番組リストの行は時刻（Geist が担当）と番組名（Noto Sans JP が担当）を
  // 同じ行に持つので、1 要素で両方の実使用フォントが確認できる
  const fonts = await platformFontsOf(cdp, 'li[data-program-id]')
  log(`  実使用フォント（番組リストの行） = ${fonts?.join(', ') ?? '(取れず)'}`)
  if (fonts === null) {
    ng.push('フォント判定: li[data-program-id] が見つからない')
  } else {
    if (!fonts.some((f) => f.includes('Noto Sans JP'))) {
      ng.push(`和文が Noto Sans JP で描画されていない（実使用: ${fonts.join(', ')}）`)
    }
    if (!fonts.some((f) => f.includes('Geist'))) {
      ng.push(`英数字が Geist で描画されていない（実使用: ${fonts.join(', ')}）`)
    }
    // 「1 グリフだけ Noto Sans JP、残りはシステムフォント」という部分的な退行は
    // 上の `some` だけでは検出できない（1 件でも Noto があれば真になる）。
    // 和文システムフォント（--font-sans のフォールバック候補）が実使用に
    // 一切現れないことも見て、検出力を上げる。
    const systemJpFonts = fonts.filter((f) => /Hiragino|Yu Gothic|Meiryo/.test(f))
    if (systemJpFonts.length > 0) {
      ng.push(`和文の一部がシステムフォントに落ちている（実使用: ${fonts.join(', ')}）`)
    }
  }

  // tabular-nums が和文まじりの文字列でも実際に等幅を作っているか。
  // canvas 2D の `font` ショートハンドには font-variant-numeric を渡せないので、
  // 実要素を DOM に挿して getBoundingClientRect で幅を測る（実描画の幅そのもの）。
  // `normal` 側も測って、判定が「たまたま両方同じ幅」ではなく tabular-nums の
  // 効果そのものを見ていることを確認する。
  const widths = await page.evaluate(() => {
    function width(text, variant) {
      const el = document.createElement('span')
      el.style.position = 'absolute'
      el.style.visibility = 'hidden'
      el.style.whiteSpace = 'pre'
      el.style.fontVariantNumeric = variant
      el.textContent = text
      document.body.appendChild(el)
      const w = el.getBoundingClientRect().width
      el.remove()
      return w
    }
    return {
      tabularA: width('第11話', 'tabular-nums'),
      tabularB: width('第88話', 'tabular-nums'),
      normalA: width('第11話', 'normal'),
      normalB: width('第88話', 'normal'),
    }
  })
  log(
    `  第11話/第88話 幅（tabular-nums） = ${widths.tabularA.toFixed(2)} / ${widths.tabularB.toFixed(2)}`,
  )
  log(
    `  第11話/第88話 幅（normal）       = ${widths.normalA.toFixed(2)} / ${widths.normalB.toFixed(2)}`,
  )
  if (Math.abs(widths.tabularA - widths.tabularB) > 0.5) {
    ng.push(
      `tabular-nums が和文まじりの文字列で等幅を作っていない（${widths.tabularA.toFixed(2)} / ${widths.tabularB.toFixed(2)}）`,
    )
  }
  if (Math.abs(widths.normalA - widths.normalB) < 0.5) {
    ng.push(
      'tabular-nums 無指定でも同じ幅になっている（この判定が tabular-nums の効果を検出できていない）',
    )
  }

  await context.close()
}

// 数値は docs に転記しない（転記した瞬間に二重管理になる）。docs は
// 「ここで測る」とだけ言い、実際の数値はこの出力が権威。
log('\n=== 測ったコントラスト ===')
for (const { theme, label, ratio, floor } of contrasts) {
  const mark = ratio >= floor ? ' ' : '×'
  log(`  ${mark} [${theme}] ${label}: ${ratio.toFixed(2)}（下限 ${floor}）`)
}

await finish(ng, browser)
