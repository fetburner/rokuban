// ファビコン（黒枠のテレビに映る「イ」）を生成する。
//
// 高柳健次郎が 1926 年にブラウン管へ映した日本初のテレビ映像が「イ」だった。
// 元画像 https://dndi.jp/20-tani/images/09110901.jpg
//
//   node scripts/gen-favicon.mjs
//
// ラスタ版は npm 依存にない外部ツールで作るため自動化していない。
// 変更したら以下も再生成する（web/ で実行）。
//
//   rsvg-convert -w 180 -h 180 public/favicon.svg -o public/apple-touch-icon.png
//   for s in 16 32; do rsvg-convert -w $s -h $s public/favicon.svg -o /tmp/ico_$s.png; done
//   magick /tmp/ico_16.png /tmp/ico_32.png -define icon:auto-resize=32,16 public/favicon.ico

import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const INK = "#18181b"; // 墨
const LIT = "#fafafa"; // 輝線
const GAP = "#71717a"; // 輝線の間隙
const BEZEL = "#18181b"; // 筐体

// --- 字形 -----------------------------------------------------------------
// Noto Sans JP Black の「イ」の輪郭。
//
// `<text>` を使わずパスで持つのは、ファビコンの SVG がレンダリング環境の
// フォントに依存し、字形と太さがプラットフォームで揃わないため。
//
// ゴシックである必要がある。「イ」の二画目は止めだが、明朝は縦画が下へ向かって
// 細り末端が尖る。ゴシックは縦画が等幅で末端が平らに止まる。
// Black を選んだのは 16px での可読性で、Bold では縦画が灰色に沈む。
//
// ライセンス: 輪郭を製品に同梱するので、再配布が許諾されている書体を選ぶ。
// Noto Sans JP は SIL OFL 1.1（Copyright 2014-2021 Adobe）。
// 生成される SVG にも著作権表示を入れている。
// ヒラギノで作った版もあったが、Apple のシステムフォントには輪郭を
// 同梱する権利がないため使えない。
//
// 再生成の手順:
//
//   curl -sSLO https://raw.githubusercontent.com/notofonts/noto-cjk/main/Sans/SubsetOTF/JP/NotoSansJP-Black.otf
//   magick -background white -fill black -font NotoSansJP-Black.otf -pointsize 1000 \
//     label:イ -colorspace gray -threshold 50% -trim +repage /tmp/i.pbm
//   potrace -s --alphamax 1.0 -o /tmp/i.traced.svg /tmp/i.pbm
//
// rsvg-convert はフォントファイルの直接指定ができないので magick で描く。
// potrace は y 軸を反転して 10 倍の座標で吐くので、その g transform を
// そのまま内側に持つ。box は trim 後の実寸（= 字形の外接矩形）。

const GLYPH = {
  box: [853, 878],
  // potrace の座標系をアイコンの座標系に戻す
  flip: "translate(0 878) scale(0.1 -0.1)",
  d:
    "M7099 8627 c-150 -171 -614 -640 -819 -828 -531 -487 -1083 -927 -1605 -1281 -515 -349 " +
    "-1273 -752 -2015 -1071 -736 -315 -1527 -585 -2414 -822 -131 -35 -240 -65 -241 -67 -3 " +
    "-4 734 -1510 744 -1520 4 -4 97 19 206 53 917 277 1942 685 2911 1159 l264 128 0 -1576 " +
    "c0 -1484 -5 -1816 -30 -2242 -12 -189 -37 -475 -46 -527 l-6 -33 962 0 c930 0 961 1 955 " +
    "18 -24 79 -56 333 -76 622 -9 116 -13 848 -16 2457 l-4 2291 173 118 c578 394 1189 862 " +
    "1723 1319 315 270 765 685 765 706 0 4 -292 287 -649 628 l-649 620 -133 -152z",
};

// --- 走査領域と配置 -------------------------------------------------------
// 走査領域の境界を偶数に取り、32px 描画でも輝線が画素境界に乗るようにする。
// 16px では輝線が 0.5px になり縞は溶けるが、明るい画面として残る。

const SCREEN = { x: 4, y: 4, w: 24, h: 24 };
const SCREEN_PAD = 0.8;
const SCAN_PERIOD = 2; // 輝線 1 + 間隙 1

function scanlines() {
  const rows = [];
  for (let i = 0; i < SCREEN.h / SCAN_PERIOD; i++) {
    const y = SCREEN.y + i * SCAN_PERIOD;
    rows.push(
      `  <rect x="${SCREEN.x}" y="${y}" width="${SCREEN.w}" height="1" fill="${LIT}"/>`,
    );
  }
  return rows.join("\n");
}

function glyph() {
  const [gw, gh] = GLYPH.box;
  const s = Math.min(
    (SCREEN.w - 2 * SCREEN_PAD) / gw,
    (SCREEN.h - 2 * SCREEN_PAD) / gh,
  );
  const dx = SCREEN.x + (SCREEN.w - gw * s) / 2;
  const dy = SCREEN.y + (SCREEN.h - gh * s) / 2;
  return (
    `  <g fill="${INK}" transform="translate(${dx.toFixed(2)} ${dy.toFixed(2)}) ` +
    `scale(${s.toFixed(5)}) ${GLYPH.flip}">\n    <path d="${GLYPH.d}"/>\n  </g>`
  );
}

const svg = `<!-- 高柳健次郎が 1926 年にブラウン管へ映した日本初のテレビ映像「イ」。
     いろは順の最初の文字（ABC の A に相当）が選ばれた。
     web/scripts/gen-favicon.mjs の生成物。直接編集しない。

     「イ」の字形は Noto Sans JP Black より。
     Copyright 2014-2021 Adobe (http://www.adobe.com/).
     SIL Open Font License 1.1 (http://scripts.sil.org/OFL) -->
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">
  <rect width="32" height="32" fill="${BEZEL}"/>
  <rect x="${SCREEN.x}" y="${SCREEN.y}" width="${SCREEN.w}" height="${SCREEN.h}" fill="${GAP}"/>
${scanlines()}
${glyph()}
</svg>
`;

const out = join(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "public",
  "favicon.svg",
);
writeFileSync(out, svg);
console.log(`wrote ${out}`);
