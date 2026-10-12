// textlint のローカルルール。docs:lint が `textlint --rulesdir scripts/textlint-rules` で読む。
//
// docs の価値は「必要な節だけを 1 回で読めること」にある。ファイルを丸ごと読む LLM には、
// 巨大なファイルはそれだけで読むコストになる。大きい doc は「索引 + 分割本文」にする
// （docs/workflow.md §分割 doc の形）が、分割すべき時期を判定する手段が無かったので上限を置く。
//
// 上限 60 KB: 巨大な doc を分割する作業が基準にした値と同じにする。基準と gate が別の値だと、
// 分割を終えたファイルが gate で落ちるか、gate を通ったファイルが分割の対象に残る。
// 単位はバイト（60 × 1024 = 61,440 バイト）。分割の対象に挙がった 3 ファイル
// （docs/frontend/recordings.md / docs/frontend/live.md / docs/api/media.md）はバイト数で
// 選んだ集合に入り、文字数（wc -m）では後の 2 つが 60,000 字を下回る。`wc -c` / `ls -l` で
// そのまま確かめられる値でもある。
//
// 許容件数: 既存の超過は scripts/file-size.allow.json に 1 件として置き、実測と一致させる。
// 分割して上限以下になったら、許容を下げる（キーを消す）指示で落ちる。方式と理由は
// scripts/textlint-allow.js の冒頭にある。
'use strict';

const fs = require('node:fs');
const ratchet = require('../textlint-allow.js');

const MAX_BYTES = 60 * 1024;
const check = ratchet('scripts/file-size.allow.json', ` ${MAX_BYTES} バイト超過`);

module.exports = function (context) {
  return {
    [context.Syntax.Document](node) {
      const bytes = fs.statSync(context.getFilePath()).size;
      const found =
        bytes > MAX_BYTES
          ? [{ node, message: `ファイルが ${bytes} バイト（上限 ${MAX_BYTES} バイト）。索引 + 分割本文に割る` }]
          : [];
      check(context, node, found);
    },
  };
};
