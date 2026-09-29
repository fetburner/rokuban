-- +goose Up

-- チャプターの所有モデル。自動の検出結果は毎回作り直せる導出値、ユーザーの
-- 修正は二度と再取得できない事実である（不変条件 9）。同じ表に置くと再検出が修正を
-- 上書きするので、2 表に割る。
--
--   recording_cm_detections  = 自動層（導出値。何度でも書き直される）
--   recording_chapter_ownership = 行があること = ユーザーが所有（確認済み）
--   recording_chapter_spans  = ユーザー層。所有の行の下にだけ存在する
--
-- 所有の行がある録画では自動層を読まない。境界の修正は「その時点の検出結果に対する
-- 差分」なので、時刻で自動層の上に重ね塗りすると再検出で境界が動いた瞬間に意味を
-- 失う（重ね塗り案を採らなかった理由）。

-- int8range の重なり判定（EXCLUDE ... WITH &&）を GiST に載せるために要る。
-- bigint の等値演算子を GiST が知らないため。
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

-- 行があること = ユーザーが所有（= 確認済み）。行の不在 = 自動層を使う
-- （不変条件 10）。adopted_at は判定に使わない（判定は行の存在だけ）が、
-- 「いつ確認したか」は rescue の往復で失ってはならない事実なので列を持つ。
CREATE TABLE recording_chapter_ownership (
    recording_id bigint PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    adopted_at   timestamptz NOT NULL DEFAULT now()
);

-- ユーザー層。span は原本の最初の映像フレームを 0 とする ms の半開区間
-- int8range [start, end)。単位は JLSE のフレーム番号から
-- ms = round(frame × 1001 / 30) で作る（フレームレートは 30000/1001 で固定）。
--
-- 本編は行を持たない。区間の隙間が本編である（API は隙間を返さず、クライアントが
-- 再生中の <video>.duration で閉じる）。そのため「ラベルも無く cut でもない行」
-- は意味を持たない（不変条件 10）ので CHECK で表現不可能にする。
--
-- 自動チャプターの ID を宛先にはしない（不変条件 9 の identity）。行の同一性は
-- 区間そのもので、PUT はタイムライン全体を置き換える。
CREATE TABLE recording_chapter_spans (
    recording_id bigint NOT NULL REFERENCES recording_chapter_ownership (recording_id) ON DELETE CASCADE,
    span         int8range NOT NULL CHECK (NOT isempty(span)),
    label        text,
    cut          boolean NOT NULL,
    CHECK (label IS NOT NULL OR cut),
    -- 重なる区間は「どの区間がその時刻を占めるか」を決められない。アプリ側の
    -- 検証と二重に持つ（素の INSERT が直接来ても壊れない）。
    EXCLUDE USING gist (recording_id WITH =, span WITH &&)
);

-- +goose Down

DROP TABLE recording_chapter_spans;
DROP TABLE recording_chapter_ownership;

-- btree_gist は他用途でも使う可能性があるので Down でも残す。
