-- +goose Up

-- パラメータの上書きを program_intents から分離する（issue #18 の案 A の続き、
-- M2-4。docs/recording.md §4.2「overrides は program_intents とは別の表に置く」）。
--
-- ユーザーが番組について主張しうることは 2 つあり、独立している:
--   ①録る / 録るな（program_intents.action）
--   ②パラメータの上書き（本マイグレーションで program_overrides に分離）
--
-- 00008 で両方を program_intents 1 表に同居させたが、action が NOT NULL である
-- ために「パラメータだけ上書きした。録る録らないについては意見なし」を表現
-- できなかった。priority を 1 つ変えるだけで action='record'（= 録れ）を
-- 主張させられてしまう。その結果、行が空になったとき「この行はもともと
-- 何を主張していたのか」が行自身から読めず、reservations.rule_id
-- （別表の、しかも直近 ruler パスのスナップショット）に問い合わせる必要が
-- 出ていた。それは次の場合に誤答する:
--
--   手動予約（intent{record, {priority:7}}）に後からルールがマッチして
--   rule_id が埋まった状態で「ルールに戻す」を押す → rule_id IS NOT NULL
--   なので意図の行が消える → 手動予約だったものが純粋なルール由来になり、
--   その後ルールを編集して外れるとユーザーの手動予約が消える。
--
-- 表を分けると「ルールに戻す」は program_overrides の行を DELETE するだけになり、
-- program_intents には一切触れないので、上記の経路が構造的に存在しなくなる。

CREATE TABLE program_overrides (
    site       text   NOT NULL,
    program_id bigint NOT NULL,
    -- 上書きしたキーのみを持つ疎なドキュメント。program_overrides 自身の
    -- ロジックは中身を一切使わない不透明なペイロードなので、CHECK は置かない
    -- （下記コメント参照）。マージは SQL ではなく Go 側で db.ReservationOptions の
    -- 型付きフィールドとして行う（internal/api/reservations_overrides.go）。
    overrides  jsonb  NOT NULL,
    -- GC 用のスナップショット。program_intents と同じ理由（EPG プロジェクションの
    -- 刈り取りと独立させる）で 3 箇所目の重複になるが、ruler は reservations 側
    -- だけを延長に追従させるため既にドリフトしている（epg.retention_grace の
    -- 24h が吸収する）。program_snapshots への抽出は別タスク（docs/schema.md §3.5）。
    program_start_at    timestamptz NOT NULL,
    program_duration_ms bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);

-- GC（番組終了後）の走査用。program_intents と同じ規約。
CREATE INDEX ON program_overrides (program_start_at);

-- 既存の overrides を移設する。program_intents.overrides のうち空でない行
-- （= 実際に何かを上書きしている行）だけが対象。中身を検査するのはこの
-- 一度きりの移設のためで、program_overrides 自身のロジックが今後内容を
-- 検査することはない（下記「overrides に CHECK を置かない」参照）。
INSERT INTO program_overrides (
    site, program_id, overrides, program_start_at, program_duration_ms, created_at, updated_at
)
SELECT site, program_id, overrides, program_start_at, program_duration_ms, created_at, updated_at
FROM program_intents
WHERE overrides <> '{}'::jsonb;

ALTER TABLE program_intents DROP COLUMN overrides;

-- SSE ヒント。00008 と同じ規約で reservations トピックに寄せる（意図・上書きの
-- 変更は予約一覧・番組表の両方に現れる）。
CREATE TRIGGER program_overrides_notify
    AFTER INSERT OR UPDATE OR DELETE ON program_overrides
    FOR EACH ROW EXECUTE FUNCTION rokuban_notify('reservations');

-- overrides に CHECK を置かない理由（docs/recording.md §4.2「jsonb を許す条件」）:
--
-- overrides に jsonb を許すのは、それが program_overrides 自身のロジックでは
-- 一切使われない不透明なペイロードだからである。予約のパラメータを上書きする
-- ためだけに存在し、内容でクエリも制約もしない。
--
-- したがって CHECK (jsonb_strip_nulls(overrides) <> '{}') のような内容を検査
-- する制約は置かない。技術的には可能（jsonb_strip_nulls は IMMUTABLE なので
-- CHECK に書ける）だが、「クエリはしないが制約はする」という中途半端な状態が
-- 一番悪い。不透明なペイロードなら不透明に扱う。空の上書きは「行が無い」
-- （DELETE）で表現し、api 側がその不変条件を保証する
-- （internal/api/reservations_overrides.go の isEmptyOverridesJSON）。

-- +goose Down

DROP TRIGGER IF EXISTS program_overrides_notify ON program_overrides;

ALTER TABLE program_intents
    ADD COLUMN overrides jsonb NOT NULL DEFAULT '{}';

-- 上書きを program_intents へ書き戻す。program_overrides の行に対応する
-- program_intents 行が無い場合（= ルール由来予約への上書きのみで意図が
-- 存在しないケース。M2-4 で新たに可能になった状態）は書き戻し先が無いため
-- 情報が落ちる片道になる（00008 の Down と同じ性質の非対称性）。
UPDATE program_intents i
SET overrides = o.overrides
FROM program_overrides o
WHERE o.site = i.site AND o.program_id = i.program_id;

DROP TABLE IF EXISTS program_overrides;
