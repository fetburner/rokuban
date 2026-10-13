// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// 表の役目は、行を縦に流し読みして見比べることである。長文のセルが 1 つあると GitHub では
// その行が縦に伸び、前後の行を見比べられなくなる。セルの中には箇条書きも段落も書けないので、
// 値ごとの説明は太字と `---` でつないだ 1 行になる。段落長のルール（paragraph-length.js）は
// 表のセルを数えないので、長文を表へ移せば段落長の gate をすり抜けられる。それを塞ぐ。
// 表のセルは一言（名前・型・値）に限り、説明は小見出しか番号付きリストへ割る。
//
// 数える単位: 表のセル（TableCell）1 つ。見出し行のセルも数える。
//
// 字数: セルに見えている文字列の字数。数え方は段落と共有し、scripts/textlint-chars.js に
// ある。
//
// 上限 200 字: 2026-10-13 時点の対象文書（105 ファイル、うち表を持つのは 60 ファイル・
// 2,449 セル）で、セルの中央値 15 字・p90 57 字・p99 160 字・最大 938 字、200 字超は
// 18 セルだった。一言のセルに字数の上限は
// 機械で決められないので、p99 の上にあり段落の上限の半分にあたる値を選び、明らかに説明を
// 詰め込んだセルだけを拾う。
//
// 許容件数: 既存の超過はファイルごとの件数で scripts/table-cell-length.allow.json に置き、
// 実測と一致させる。方式と理由は scripts/textlint-allow.js の冒頭にある。
'use strict';

const ratchet = require('../textlint-allow.js');
const chars = require('../textlint-chars.js');

const MAX_CHARS = 200;
const check = ratchet('scripts/table-cell-length.allow.json', ` ${MAX_CHARS} 字超の表のセル`);

// overlong は木の中の 200 字超のセルを文書順に返す。セルの中にセルは無いので、セルで止まる。
const overlong = (n, out = []) => {
  if (n.type === 'TableCell') {
    const c = chars(n);
    if (c > MAX_CHARS) {
      out.push({
        node: n,
        message: `表のセルが ${c} 字（上限 ${MAX_CHARS} 字）。セルは一言（名前・型・値）にし、説明は小見出しか番号付きリストへ移す`,
      });
    }
  } else {
    for (const c of n.children ?? []) overlong(c, out);
  }
  return out;
};

module.exports = function (context) {
  return {
    [context.Syntax.Document](node) {
      check(context, node, overlong(node));
    },
  };
};
