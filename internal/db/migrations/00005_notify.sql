-- +goose Up

-- SSE (/api/events) のためのヒント配送。
--
-- 役割は「どのデータが変わったか」のトピック名を配るだけで、変更内容は載せない。
-- クライアントは受け取ったら該当クエリを invalidate し、真実は REST から取り直す
-- （レベルトリガー。issue #5 / docs/api.md）。取りこぼしは staleTime 経過後の
-- 再取得で自然回復するので、通知の欠落は致命的でない。
--
-- トリガーで送るのは「書き手が通知を忘れる」種類のバグを構造的に消すため。
-- 逆に重複・空振りの通知は起こりうる（updated_at だけが変わる UPDATE 等）が、
-- ヒントなので害はない。量が問題になる場合は api 側の Hub で合流させる。
--
-- epg_programs には意図的にトリガーを張らない。全量 upsert で 1 パス数千行に
-- なるため、同期ジョブが 1 パス完了時に明示的に 1 回だけ通知する。

-- +goose StatementBegin
CREATE FUNCTION rokuban_notify() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('rokuban', TG_ARGV[0]);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- トピック名はテーブル名ではなくクライアントの関心事に揃える。
-- media_assets / drop_stats の変更は録画一覧（サイズ・ドロップ統計）に現れるので
-- recordings トピックに寄せる。drop_stats は media_assets と同一トランザクションで
-- 書かれるため独自のトリガーは不要。

CREATE TRIGGER reservations_notify
    AFTER INSERT OR UPDATE OR DELETE ON reservations
    FOR EACH ROW EXECUTE FUNCTION rokuban_notify('reservations');

CREATE TRIGGER recordings_notify
    AFTER INSERT OR UPDATE OR DELETE ON recordings
    FOR EACH ROW EXECUTE FUNCTION rokuban_notify('recordings');

CREATE TRIGGER media_assets_notify
    AFTER INSERT OR UPDATE OR DELETE ON media_assets
    FOR EACH ROW EXECUTE FUNCTION rokuban_notify('recordings');

-- +goose Down
DROP TRIGGER IF EXISTS media_assets_notify ON media_assets;
DROP TRIGGER IF EXISTS recordings_notify ON recordings;
DROP TRIGGER IF EXISTS reservations_notify ON reservations;
DROP FUNCTION IF EXISTS rokuban_notify();
