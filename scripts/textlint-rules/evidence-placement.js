// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// 根拠（関数名・パス・テスト名など）は消さずに、節の末尾の `<details><summary>根拠</summary>`
// に集める（docs/workflow.md §分割 doc の形）。本文に根拠が混ざると、主張を読むたびに
// 識別子を読み飛ばすことになる。このルールは根拠が「どこに置かれているか」を数える。
// 識別子がコードに実在するかは検査しない（workflow.md が退けた存在検査 lint とは別物）。
//
// 対象をテスト名（`Test[A-Z]` で始まる語）に絞る: 関数名やパスは本文の主張にも正当に出て
// くるので、根拠かどうかを機械で区別できない。テスト名は「測っていない挙動を断言しない」
// （CLAUDE.md）の併記として書かれるので、本文に出てくれば根拠が本文に混ざった印になる。
// インラインコードの中のテスト名も数える。根拠はほぼ `TestFoo` の形で書かれるので、除けば
// 何も数えなくなる。
//
// 数える単位: 行。1 行にテスト名が何個あっても 1 と数える。根拠を並べた 1 行を 1 件と
// 数えたいからである。除外するのはコードブロック（CodeBlock ノード）の行と、`<details` で
// 始まる Html ノードから `</details>` を含む Html ノードまで（同じ親の兄弟の範囲）の行である。
// 見出し・表・リンク・その他の Html の行は数える。
//
// 許容件数: 既存の該当行はファイルごとの件数で scripts/evidence-placement.allow.json に置き、
// 実測と一致させる。方式と理由は scripts/textlint-allow.js の冒頭にある。
'use strict';

const ratchet = require('../textlint-allow.js');

const TEST_NAME = /\bTest[A-Z]\w+/;
const check = ratchet(
  'scripts/evidence-placement.allow.json',
  '根拠の置き場（<details> とコードブロック）の外でテスト名を含む行',
);

// excluded は数えない範囲（文書内の [start, end) オフセット）を集める。
const excluded = (n, getSource, out = []) => {
  let depth = 0;
  for (const c of n.children ?? []) {
    if (c.type === 'Html') {
      const src = getSource(c);
      const open = (src.match(/<details\b/gi) ?? []).length;
      const close = (src.match(/<\/details>/gi) ?? []).length;
      if (depth > 0 || open > 0) out.push(c.range);
      depth = Math.max(0, depth + open - close);
    } else if (depth > 0 || c.type === 'CodeBlock') {
      out.push(c.range);
    } else {
      excluded(c, getSource, out);
    }
  }
  return out;
};

module.exports = function (context) {
  const { Syntax, getSource, locator } = context;
  return {
    [Syntax.Document](doc) {
      const src = getSource(doc);
      const base = doc.range[0];
      const skip = excluded(doc, getSource);
      const found = [];
      let start = 0;
      for (const line of src.split('\n')) {
        const s = base + start;
        const e = s + line.length;
        start += line.length + 1;
        if (!TEST_NAME.test(line)) continue;
        if (skip.some(([a, b]) => a < e && s < b)) continue;
        found.push({
          node: doc,
          padding: locator.range([s - base, e - base]),
          message: `テスト名が根拠の置き場の外にある。節末の <details><summary>根拠</summary> に移す`,
        });
      }
      check(context, doc, found);
    },
  };
};
