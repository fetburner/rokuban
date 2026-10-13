// scripts/textlint-rules/ のルールが共有する「節」の割り方。節の冒頭（section-lead.js）・
// 節の大きさ（section-length.js）・節の太字（section-strong.js）が別々の節を数えると、同じ
// 節が規則ごとに違う範囲になるので、ここに 1 つだけ置く。--rulesdir がディレクトリ内のどの
// ファイルを読むかはドキュメントに無いので、ルールのディレクトリの外に置く
// （scripts/textlint-allow.js と同じ）。
//
// 節: 見出し（深さを問わない）1 つから、次の見出し（深さを問わない）の直前まで。下位の見出しを
// 含めないのは、含めると `##` の大きさが配下の `###` の合計になり、小見出しに割っても字数が
// 減らないからである。大きさの上限は「小見出しに割れ」という指示として効かせたい。
// 最初の見出しより前（分割ファイル先頭の breadcrumb の引用など）はどの節にも入らない。
// 見るのは Document の最上位の子だけである（リストや引用の中の見出しは節を作らない）。
// `<details>` の中の見出しも節を作り、その節の中身は除外されない（2026-10-13 時点で 0 件）。
'use strict';

// sections は文書 doc を節に割り、{ heading, blocks } の配列を文書順に返す。blocks は節の
// 最上位ブロックで、`<details>` もコードブロックも含む。
const sections = (doc) => {
  const out = [];
  for (const c of doc.children) {
    if (c.type === 'Header') out.push({ heading: c, blocks: [] });
    else if (out.length > 0) out.at(-1).blocks.push(c);
  }
  return out;
};

// visible は節の blocks から根拠の置き場（`<details>`）とコードブロックを除いたものを返す。
// `<details>` の開きタグの直後に空行が無いと中身ごと 1 つの Html ノードになり、空行があると
// 開きタグの Html・中身の段落やリスト・閉じタグの Html が兄弟として並ぶ。どちらも、`<details`
// を含む Html から `</details>` を含む Html までの兄弟を除く（evidence-placement.js と同じ
// 数え方）。リスト項目の中の `<details>` は見ない（2026-10-13 時点で 0 件）。
const visible = (blocks, getSource) => {
  const out = [];
  let depth = 0;
  for (const c of blocks) {
    if (c.type === 'Html') {
      const src = getSource(c);
      const open = (src.match(/<details\b/gi) ?? []).length;
      const close = (src.match(/<\/details>/gi) ?? []).length;
      depth = Math.max(0, depth + open - close);
    } else if (depth === 0 && c.type !== 'CodeBlock') {
      out.push(c);
    }
  }
  return out;
};

module.exports = { sections, visible };
