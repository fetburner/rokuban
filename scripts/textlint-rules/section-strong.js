// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// 太字は、節で最も重い 1 文を示すときだけ強調として効く（docs/workflow.md §残す文章の種類）。
// 数百字おきに太字があると、どれも強調に見えなくなる。
//
// 数える単位: 節 1 つ（割り方は scripts/textlint-sections.js。下位の見出しは含めない）。
// 太字（Strong ノード）が 1 個を超える節を 1 件と数える。見出しの中と、コードブロック・
// `<details>` の中（根拠の置き場）は数えない。表のセルとリスト項目の中は数える。
//
// 上限 1 個: 2026-10-13 時点の対象文書（105 ファイル・754 節）で、節あたりの太字は中央値 2・
// p90 10・最大 63 個、1 個を超える節は 392 節だった。字数あたりでは 1,000 字に 4.0 個で、
// yomiyasu（日本語推敲のプラグイン）の技術文書向けの目安（2 個以下、3 個超で警告）を
// 超えている。字数のルールは主張の数の代理指標なので上限に余裕を持たせたが、太字は数そのものを
// 規則（節で 1 文だけ）と比べられる。規則と gate の値をずらす理由が無いので 1 にした。
//
// 許容件数: 既存の超過はファイルごとの件数で scripts/section-strong.allow.json に置き、
// 実測と一致させる。方式と理由は scripts/textlint-allow.js の冒頭にある。
'use strict';

const ratchet = require('../textlint-allow.js');
const { sections, visible } = require('../textlint-sections.js');

const MAX_STRONG = 1;
const check = ratchet('scripts/section-strong.allow.json', `太字が ${MAX_STRONG} 個を超える節`);

const strongs = (n) =>
  (n.type === 'Strong' ? 1 : 0) + (n.children ?? []).reduce((sum, c) => sum + strongs(c), 0);

module.exports = function (context) {
  return {
    [context.Syntax.Document](node) {
      const found = [];
      for (const s of sections(node)) {
        const k = visible(s.blocks, context.getSource).reduce((sum, b) => sum + strongs(b), 0);
        if (k > MAX_STRONG) {
          found.push({
            node: s.heading,
            message: `節の太字が ${k} 個（上限 ${MAX_STRONG} 個）。太字は節で最も重い 1 文だけにする`,
          });
        }
      }
      check(context, node, found);
    },
  };
};
