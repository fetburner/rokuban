-- +goose Up

-- 「この番組にユーザーの投資があるか」という述語に名前を与える（issue #162）。
--
-- この述語は削除ガードの核（ruler.sql の導出削除、rules.sql のルール削除時の
-- detached 判定）でありながら、スキーマ上の名前を持たず 4 箇所に散在していた。
-- しかも 2 つの形が併存していた:
--
--   ruler.sql の導出削除ガード（DeleteReservationsBySiteAndProgramIDs）
--     program_intents は action = 'record' に限定
--   rules.sql の DeleteReservationsByRuleWithoutIntent / CountReservationsByRuleWithIntent
--     program_intents は action を限定しない
--
-- action を問わず EXISTS すると action='skip' の予約行（そもそも desired に
-- 入らない設計 = issue #18 の案 A）が保護されてしまい、「取消した予約が消えない」
-- リグレッションになる。この理由づけは ruler 側だけで明文化されていたが、
-- rules 側も同じ理由で record 限定に揃える（#162 の判断）。揃えないと
-- intent{skip} だけの予約行がルール削除時に「detached として残る」と数えられるが、
-- 直後の ruler パス（DeleteRule が同一 tx でヒントを投入するので数秒後）で
-- 導出削除され、ユーザーに見せる内訳が数秒で消える行を「detached になった」と
-- 数える不整合を生んでいた。
--
-- program_overrides 側は中身を問わず行の存在だけで desired/detached に残る設計
-- （docs/recording/reservation-model.md §4.2「ruler から見た load-bearing な行」）
-- なので、両クエリとも action と無関係に EXISTS のままでよい。
CREATE VIEW program_investments AS
SELECT site, program_id FROM program_intents WHERE action = 'record'
UNION
SELECT site, program_id FROM program_overrides;

-- +goose Down

DROP VIEW IF EXISTS program_investments;
