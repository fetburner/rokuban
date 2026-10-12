#!/usr/bin/env node
// 文書の段落長を数え、字数の上限を超えた単位が --max-violations を超えたら exit 1 で落ちる。
//
//   node scripts/docs-paragraph-length.mjs --max-violations N '<glob>' ...
//
// docs/workflow.md の「1 段落に主張を 4 つ以上乗せない」には判定手段が無い。主張の数は
// 機械で数えられないので、字数を代理指標にして明らかな詰め込みだけを拾う。
//
// 対象グロブはここに書かず引数で受ける。権威は package.json の docs:lint で、textlint と
// 同じグロブを渡す。ここに別の一覧を持つと片方だけ直って静かにずれる。
//
// 数える単位: Markdown の段落ノード（Paragraph）1 つ。最上位の段落に加え、リスト項目と
// 引用の中の段落も 1 単位として数える。リスト項目 1 つに 400 字を詰めるのは、段落に
// 詰めるのと同じ読みにくさであり、除外すると「段落をリスト項目に変える」だけで判定を
// すり抜けられる。見出し・表のセル・コードブロック・HTML ブロック・frontmatter は
// 段落ノードにならないので数えない。リスト項目が複数段落を持つときは段落ごとに数える。
// `<details>` の中は、開きタグの直後に空行を置いたときだけ段落になり数えられる。空行が
// 無いと中身ごと HTML ブロックになり、このスクリプトも textlint も見ない。
//
// 字数: 段落に見えている文字列（地の文とインラインコード）の Unicode コードポイント数。
// リンクの URL・インライン HTML・改行とその前後の空白は数えない。全角も半角も 1 字。
//
// 上限 400 字: 2026-10-12 時点の対象文書（75 ファイル・3,939 単位）で、この単位の
// 中央値 131 字・p90 333 字・最大 2,065 字、400 字超は 226 単位だった。400 字は p90 の上に
// あり、textlint の sentence-length の上限（1 文 120 字）いっぱいの文でも 4 つ並ばないと
// 届かない。通常の段落は拾わず、詰め込みだけを拾う値として選んだ。
//
// パーサ: textlint が使う @textlint/markdown-to-ast をそのまま使う。package.json には
// 書いておらず、textlint の依存として node_modules の最上位に入っているものを読む（依存は
// 足していない。解決できなければ import で落ちる）。textlint と同じ木を見るので、どこが
// 段落でどこがコードかの判定が textlint とずれない。行単位の素朴な走査はリスト・引用・
// コードフェンスの境界を自前で再実装することになるので採らなかった。
import { globSync, readFileSync } from 'node:fs';
import { parse } from '@textlint/markdown-to-ast';

const MAX_CHARS = 400;

const args = process.argv.slice(2);
const i = args.indexOf('--max-violations');
const maxViolations = i >= 0 ? Number(args[i + 1]) : NaN;
if (!Number.isInteger(maxViolations) || maxViolations < 0) {
  console.error('usage: docs-paragraph-length.mjs --max-violations N <glob>...');
  process.exit(2);
}
const globs = args.filter((_, j) => j !== i && j !== i + 1);
if (globs.length === 0) {
  console.error('usage: docs-paragraph-length.mjs --max-violations N <glob>...');
  process.exit(2);
}

const files = new Set();
for (const g of globs) {
  const matched = globSync(g);
  // 一致 0 件のグロブは打ち間違いか移動であり、黙って検査対象から外れる。
  if (matched.length === 0) {
    console.error(`docs-paragraph-length: no files match ${g}`);
    process.exit(2);
  }
  for (const f of matched) files.add(f);
}

const text = (n) =>
  n.type === 'Html' ? '' : n.value ?? (n.children ?? []).map(text).join('');

const violations = [];
for (const file of [...files].sort()) {
  const walk = (n) => {
    if (n.type === 'Paragraph') {
      const chars = [...text(n).replace(/[ \t]*\r?\n[ \t]*/g, '')].length;
      if (chars > MAX_CHARS) violations.push(`${file}:${n.loc.start.line} ${chars}`);
      return;
    }
    for (const c of n.children ?? []) walk(c);
  };
  walk(parse(readFileSync(file, 'utf8')));
}

for (const v of violations) console.log(v);
const n = violations.length;
console.log(
  `docs-paragraph-length: ${n} paragraph(s) over ${MAX_CHARS} chars (max-violations ${maxViolations})`,
);
if (n > maxViolations) {
  console.error(
    `docs-paragraph-length: ${n} > ${maxViolations}. 段落を表か番号付きの短文に割る`,
  );
  process.exit(1);
}
if (n < maxViolations) {
  console.log(`docs-paragraph-length: package.json の --max-violations を ${n} に下げられる`);
}
