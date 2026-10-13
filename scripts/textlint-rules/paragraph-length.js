// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// docs/workflow.md の「1 段落に主張を 4 つ以上乗せない」には判定手段が無い。主張の数は
// 機械で数えられないので、字数を代理指標にして明らかな詰め込みだけを拾う。
//
// textlint のルールにしたので、構文木は textlint が渡し、対象ファイルは docs:lint の
// グロブがそのまま決める。Markdown パーサを自分で読まないので依存は足していない。
//
// 数える単位: 段落ノード（Paragraph）1 つ。最上位の段落に加え、リスト項目と引用の中の
// 段落も 1 単位として数える。リスト項目 1 つに 400 字を詰めるのは、段落に詰めるのと同じ
// 読みにくさであり、除外すると「段落をリスト項目に変える」だけで判定をすり抜けられる。
// 見出し・表のセル・コードブロック・HTML ブロック・frontmatter は段落ノードにならないので
// 数えない。リスト項目が複数段落を持つときは段落ごとに数える。
// `<details>` の中は、開きタグの直後に空行を置いたときだけ段落になり数えられる。空行が
// 無いと中身ごと HTML ブロックになり、このルールも textlint の他のルールも見ない。
//
// 字数: 段落に見えている文字列の字数。数え方は表のセルと共有し、scripts/textlint-chars.js に
// ある。
//
// 上限 400 字: 2026-10-12 時点の対象文書（75 ファイル・3,939 単位）で、この単位の
// 中央値 131 字・p90 333 字・最大 2,065 字、400 字超は 226 単位だった。400 字は p90 の上に
// あり、textlint の sentence-length の上限（1 文 120 字）いっぱいの文でも 4 つ並ばないと
// 届かない。通常の段落は拾わず、詰め込みだけを拾う値として選んだ。
//
// 許容件数: 既存の超過はファイルごとの件数で scripts/paragraph-length.allow.json に置き、
// 実測と一致させる。方式と理由は scripts/textlint-allow.js の冒頭にある。
'use strict';

const ratchet = require('../textlint-allow.js');
const chars = require('../textlint-chars.js');

const MAX_CHARS = 400;
const check = ratchet('scripts/paragraph-length.allow.json', ` ${MAX_CHARS} 字超の段落`);

// overlong は木の中の 400 字超の段落を文書順に返す。段落の中に段落は無いので、段落で止まる。
const overlong = (n, out = []) => {
  if (n.type === 'Paragraph') {
    const c = chars(n);
    if (c > MAX_CHARS) {
      out.push({
        node: n,
        message: `段落が ${c} 字（上限 ${MAX_CHARS} 字）。小見出しか番号付きリストに割る。表のセルは一言（名前・型・値）に限り、長文をセルへ移さない`,
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
