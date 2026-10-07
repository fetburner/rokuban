import { installApiStubs, launchBrowser, sseKeepAlive } from './lib.mjs'
const OUT = process.env.OUT_DIR ?? 'h3-mocks/'
const which = process.argv[2]
const names = [['GR',32736,1024,'NHK総合',1],['GR',32737,1032,'Eテレ',2],['GR',32738,1040,'日テレ',4],['GR',32742,1048,'テレビ朝日',5],['GR',32739,1056,'TBS',6],['GR',32741,1064,'テレビ東京',7],['GR',32740,1072,'フジテレビ',8],
['BS',4,101,'NHK BS',0],['BS',6,141,'BS日テレ',0],['BS',6,151,'BS朝日',0],['BS',6,161,'BS-TBS',0],['BS',6,171,'BSテレ東',0],['BS',6,181,'BSフジ',0],['BS',6,191,'WOWOWプライム',0],
['CS',7,308,'AT-X',0],['CS',7,293,'スカパー!プロモ',0]]
const services = names.map(([t,n,s,name,k])=>({id:n*100000+s,networkId:n,serviceId:s,name,channelType:t,channel:String(s),remoteControlKeyId:k,hasLogoData:false,hasPrograms:true}))
const rec = (id,title)=>({id,site:'default',source:'manual',serviceName:'NHK総合',channelType:'GR',channel:'27',networkId:32736,serviceId:1024,eventId:id,title,startAt:'2026-10-05T12:00:00Z',durationMs:1800000,status:'finished',keepOriginal:'always',cmDetection:{state:'disabled'},createdAt:'2026-10-05T12:30:00Z'})
const handler = async ({path,json,route}) => {
  if (path==='/api/sites') return json(['default'])
  if (path==='/api/capabilities') return json({live:true})
  if (path==='/api/encode-queue') return json({queued:0,running:0})
  if (path==='/api/events') return sseKeepAlive(route)
  if (path==='/api/sites/default/services') return json(services)
  if (path==='/api/recordings') return json([rec(1,'ニュース7'),rec(2,'おかあさんといっしょ')])
  if (path.startsWith('/api/media')) return route.fulfill({status:404})
  return json([])
}
const browser = await launchBrowser()
const ctx = await browser.newContext({viewport:{width:390,height:844},deviceScaleFactor:2,hasTouch:true,isMobile:true,colorScheme:'light',locale:'ja-JP',timezoneId:'Asia/Tokyo'})
const page = await ctx.newPage()
page.on('pageerror',e=>console.log('pageerror',e.message))
await page.clock.setFixedTime(new Date('2026-10-07T12:00:00+09:00'))
await installApiStubs(page, handler)
const shot = (n)=>page.screenshot({path:OUT+n+'.png'})
const pick = async (...labels)=>{ await page.getByRole('checkbox',{name:'すべて',exact:true}).tap(); for (const l of labels) { await page.getByRole('checkbox',{name:new RegExp('(^|\\s)'+l+'$')}).tap(); await page.waitForTimeout(100) } }
const toRec = async()=>{ await page.goto('http://localhost:4391/recordings',{waitUntil:'domcontentloaded'}); await page.getByText('ニュース7').first().waitFor({timeout:15000}) }
const openFilter = async()=>{ await page.getByRole('button',{name:'絞り込み',exact:true}).tap(); await page.waitForTimeout(500) }
const trigger = ()=>page.getByRole('button',{name:/チャンネル/}).first()
if (which==='current') {
  await toRec()
  await page.getByRole('button',{name:'その他'}).tap(); await page.waitForTimeout(500); await shot('more-current')
  await page.keyboard.press('Escape'); await page.reload(); await page.getByText('ニュース7').first().waitFor()
  await openFilter(); await trigger().tap(); await page.waitForTimeout(600); await shot('channel-current')
} else {
  await page.addInitScript((m)=>{window.__mock=m}, which)
  if (which==='more') { await toRec(); await page.getByRole('button',{name:'その他'}).tap(); await page.waitForTimeout(600); await shot('more-sheet') }
  if (which==='A') { await toRec(); await openFilter(); await trigger().tap(); await page.waitForTimeout(600); await pick('NHK総合','日テレ','BS朝日'); await shot('channel-A-stacked') }
  if (which==='B') { await toRec(); await openFilter(); await shot('channel-B-push-before'); await trigger().tap(); await page.waitForTimeout(500); await pick('NHK総合','日テレ','BS朝日'); await shot('channel-B-push-after') }
  if (which==='C') { await toRec(); await openFilter(); await trigger().tap(); await page.waitForTimeout(500); await pick('NHK総合','日テレ','BS朝日'); await page.evaluate(()=>{document.querySelector('[data-testid=toolbar-sheet-body]').scrollTop=0}); await page.waitForTimeout(300); await shot('channel-C-inline') }
  if (which==='P') { await page.goto('http://localhost:4391/programs',{waitUntil:'domcontentloaded'}); await page.waitForTimeout(1500); await trigger().tap(); await page.waitForTimeout(600); await shot('programs-channel-sheet') }
}
await browser.close()
