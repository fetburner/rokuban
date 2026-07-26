-- +goose Up

-- ユーザーの意図を導出行から切り離す（issue #18 の案 A）。
--
-- これまで overrides は reservations に載っていた。ruler が base だけを書くので
-- 上書きが消える事故は防げていたが、1 行が「ユーザー意図の永続記録」と
-- 「ruler の導出結果」を兼ねるため、次の複雑さが派生していた。
--
--   - 昇格: manual 行にルールがマッチしたとき effective を保存する必要があり、
--     skip:false を焼き付ける細工が要る
--   - 取消: 再生成者がいるかで DELETE と skip override を分岐する必要がある
--   - 削除: 「overrides があれば消さず detached にする」という例外が要る
--
-- 意図を別表に置くと 3 つとも消える。取消は「intent{skip} を書く」だけになり、
-- 導出行は消えても意図は残る。
--
-- 本マイグレーションのスコープは overrides の移設に限る。source / state は
-- 据え置き（状態機械の簡略化は ruler 実装後に判断する。#18 の案 B）。

CREATE TABLE program_intents (
    site       text   NOT NULL,
    program_id bigint NOT NULL,
    -- record: 録れ（手動予約 / ルール由来の予約に対する上書き）
    -- skip:   録るな（番組単位の除外。どのルール経由でも一貫して効く）
    action     text   NOT NULL CHECK (action IN ('record', 'skip')),
    -- ユーザーが上書きしたキーのみを持つ疎なドキュメント。ruler は触らない
    overrides  jsonb  NOT NULL DEFAULT '{}',
    -- GC 用のスナップショット。EPG プロジェクションの刈り取りと独立させる
    program_start_at    timestamptz NOT NULL,
    program_duration_ms bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);

-- GC（番組終了後）の走査用
CREATE INDEX ON program_intents (program_start_at);

-- 既存予約の移設。M1 は全行が manual（全フィールドが overrides）なので全行が対象。
-- skip は action に昇格させ、overrides からは落とす。
INSERT INTO program_intents (
    site, program_id, action, overrides,
    program_start_at, program_duration_ms, created_at, updated_at
)
SELECT site, program_id,
       CASE WHEN coalesce((overrides->>'skip')::boolean, false) THEN 'skip' ELSE 'record' END,
       overrides - 'skip',
       program_start_at, program_duration_ms, created_at, updated_at
FROM reservations
ON CONFLICT (site, program_id) DO NOTHING;

-- skip された番組は導出行を持たない（これが案 A の核心）
DELETE FROM reservations
WHERE coalesce((overrides->>'skip')::boolean, false);

ALTER TABLE reservations DROP COLUMN overrides;

-- SSE ヒント。意図の変更は予約一覧・番組表の両方に現れるので reservations トピックに寄せる
CREATE TRIGGER program_intents_notify
    AFTER INSERT OR UPDATE OR DELETE ON program_intents
    FOR EACH ROW EXECUTE FUNCTION rokuban_notify('reservations');

-- +goose Down

DROP TRIGGER IF EXISTS program_intents_notify ON program_intents;

ALTER TABLE reservations
    ADD COLUMN overrides jsonb NOT NULL DEFAULT '{}';

-- 意図を予約行へ戻す（skip 意図は行がないので復元できない = 情報が落ちる片道）
UPDATE reservations r
SET overrides = i.overrides
FROM program_intents i
WHERE i.site = r.site AND i.program_id = r.program_id AND i.action = 'record';

DROP TABLE IF EXISTS program_intents;
