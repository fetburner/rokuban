// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// 小見出しの無い長い節は、どこに何の判断があるかを最後まで読まないと分からない。段落長の
// ルール（paragraph-length.js）は段落を 1 つずつ見るので、短い段落を 25 個並べた節を拾えない。
// 節の字数に上限を置き、小見出しに割らせる。
//
// 数える単位: 節 1 つ（割り方は scripts/textlint-sections.js。下位の見出しは含めない）。
//
// 字数: 節の最上位ブロックの字数の合計（数え方は scripts/textlint-chars.js）。見出し・
// コードブロック・`<details>` の中（根拠の置き場）は数えない。根拠を集めた節が長くなるのは
// 規則どおりなので、本文の長さだけを見る。
//
// 上限 3,000 字: 2026-10-13 時点の対象文書（105 ファイル・754 節）で、節の中央値 583 字・
// p90 2,312 字・p99 5,246 字・最大 10,859 字、3,000 字超は 40 節だった。3,000 字は p90 の上に
// あり、上限いっぱい（400 字）の段落でも 8 つ並ばないと届かない。通常の節は拾わず、
// 小見出しなしで続く節だけを拾う値として選んだ。
//
// 許容件数: 既存の超過はファイルごとの件数で scripts/section-length.allow.json に置き、
// 実測と一致させる。方式と理由は scripts/textlint-allow.js の冒頭にある。
'use strict';

const ratchet = require('../textlint-allow.js');
const chars = require('../textlint-chars.js');
const { sections, visible } = require('../textlint-sections.js');

const MAX_CHARS = 3000;
const check = ratchet('scripts/section-length.allow.json', ` ${MAX_CHARS} 字超の節`);

module.exports = function (context) {
  return {
    [context.Syntax.Document](node) {
      const found = [];
      for (const s of sections(node)) {
        const c = visible(s.blocks, context.getSource).reduce((sum, b) => sum + chars(b), 0);
        if (c > MAX_CHARS) {
          found.push({
            node: s.heading,
            message: `節が ${c} 字（上限 ${MAX_CHARS} 字、<details> とコードブロックを除く）。小見出しに割る`,
          });
        }
      }
      check(context, node, found);
    },
  };
};
