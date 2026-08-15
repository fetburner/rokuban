-- +goose Up

-- issue #159（脊椎・衛星原則 #156 の最初の適用）: recordings.keep_original /
-- encode_profiles は「この録画の望ましい最終状態」（desired state）で、書き手も
-- 脊椎の書き手（watcher / reconciler）ではなく ingest worker（凍結）と api（#133
-- の事後追加）。recordings に別の状態機械が間借りしている形になっており、さらに
-- 列の既定値 ('always', '{}') が「まだ凍結されていない」と「空として凍結された」を
-- 区別できない（不変条件 10「意味を持たない行を作らない」が列に対して破れている）。
--
-- recording_encode_policy 衛星表に切り出し、凍結 = 行の INSERT にする（コミット =
-- DB 行。不変条件 3 とそのまま揃う）。行が無い = まだ凍結されていない。行がある =
-- 凍結済み（空プロファイルの凍結は encode_profiles = '{}' の行）。

CREATE TABLE recording_encode_policy (
    recording_id    bigint PRIMARY KEY REFERENCES recordings (id),
    keep_original   text   NOT NULL CHECK (keep_original IN ('always', 'until_encoded')),
    encode_profiles text[] NOT NULL,
    -- 00020_recordings_until_encoded_requires_profiles.sql から移設。
    -- until_encoded かつ encode_profiles = '{}' は「全称量化された条件が空集合に
    -- 対して自明に真になる」罠（issue #103 / #104）を再現するので表現不可能にする。
    CHECK (keep_original <> 'until_encoded' OR cardinality(encode_profiles) > 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- backfill: 「凍結済み」は導出できる --- kind='original' の media_assets 行を
-- 持つ録画がちょうど ingest コミット済みの集合（原本削除済み・deleting の録画も
-- 含む。state を問わず「行が存在するか」だけを見る --- until_encoded で原本が
-- 物理削除された録画も、ingest 完了時点では確実に凍結されている）。この集合に
-- 限って現在の 2 列の値をそのままコピーする。既定値のまま（'always' /
-- '{}'）でも、原本を持つならそれは「凍結された結果がたまたま既定値と同じ
-- だった」であって「未凍結」ではないので、判定に列の値そのものは使わない
-- （不変条件 9: 導出できる値を判定の入力に混ぜない）。原本の無い録画
-- （never-scheduled / mirakc 障害等）は行を作らず、未凍結のまま残す。
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, created_at, updated_at)
SELECT r.id, r.keep_original, r.encode_profiles, r.created_at, r.updated_at
FROM recordings r
WHERE EXISTS (
    SELECT 1 FROM media_assets a
    WHERE a.recording_id = r.id AND a.kind = 'original'
);

-- until_encoded_deletable_originals（00029_delete_reconcile_predicates.sql,
-- issue #160）は recordings.keep_original / encode_profiles を直接参照していた。
-- 列を削除する前に、この衛星表への JOIN に置き換える。JOIN（LEFT ではない）に
-- することで「policy 行が無い（未凍結）録画は対象外」が自然に落ちる ---
-- 削除エンジンにとって「未凍結」は「keep_original = always と同じ」
-- （原本を消す根拠が無い）という意味論（docs/storage.md §6）に一致する。
CREATE OR REPLACE VIEW until_encoded_deletable_originals AS
SELECT
    a.id           AS asset_id,
    a.recording_id AS recording_id,
    a.rel_path     AS rel_path,
    a.size_bytes   AS size_bytes,
    a.state        AS state
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN recording_encode_policy p ON p.recording_id = r.id
WHERE a.kind = 'original'
  AND p.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND cardinality(p.encode_profiles) > 0
  -- p.encode_profiles（desired）はここで現在の設定（encode.profiles）と
  -- 突き合わせない。これは絞り忘れではなく安全側の仕様として確定している ---
  -- 突き合わせを入れると、設定ファイルでプロファイル名を改名 / 削除しただけで、
  -- 意図したエンコードが 1 つも存在しない録画の原本が一斉に削除可能になる。
  -- config の 1 行の編集が原本ファイルという不可逆な喪失を引き起こす経路を
  -- 開けるより、容量が回収されない（可逆 --- プロファイル名を戻せば次の
  -- reconcile で揃う）方を選ぶ。したがって「凍結済み desired に含まれる、
  -- 現在の設定に存在しないプロファイルについて、その名前の active な encoded が
  -- まだ無い」録画は、このビューにとって永久に「揃っていない」ため原本を保持し
  -- 続ける。トリガーするのは「消えたプロファイルが 1 つも存在しない」ではなく
  -- 「消えたプロファイルのうち 1 つでも active な encoded が無い」こと ---
  -- 例えば desired={h264,gone} で現在の設定が {h264} のとき、h264 の active な
  -- encoded が既にあっても gone の encoded が無ければ原本は保持される。該当件数は
  -- ListUnsatisfiableEncodeProfiles（internal/db/queries/encode_reconcile.sql、
  -- 同じく active な encoded の NOT EXISTS を条件に持つ）が可視化する
  -- （docs/storage/retention.md §保持ポリシー）。
  --
  -- 同じ「desired が全部揃っているか」の述語は
  -- ListRecordingsMissingEncodes / ListUnsatisfiableEncodeProfiles にもあるが、
  -- そちらは現在の設定で絞る。この非対称は意図的で、揃えない理由は
  -- encode_reconcile.sql のヘッダに書いてある。
  AND NOT EXISTS (
    SELECT 1 FROM unnest(p.encode_profiles) AS want(profile)
    WHERE NOT EXISTS (
      SELECT 1 FROM media_assets e
      WHERE e.recording_id = r.id
        AND e.kind = 'encoded'
        AND e.state = 'active'
        AND e.profile = want.profile
    )
  )
  AND EXISTS (
    SELECT 1 FROM media_assets t
    WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
  );

ALTER TABLE recordings
    DROP CONSTRAINT IF EXISTS recordings_until_encoded_requires_profiles;

ALTER TABLE recordings
    DROP COLUMN keep_original,
    DROP COLUMN encode_profiles;

-- +goose Down

ALTER TABLE recordings
    ADD COLUMN keep_original text NOT NULL DEFAULT 'always'
        CHECK (keep_original IN ('always', 'until_encoded')),
    ADD COLUMN encode_profiles text[] NOT NULL DEFAULT '{}';

UPDATE recordings r
SET keep_original   = p.keep_original,
    encode_profiles = p.encode_profiles
FROM recording_encode_policy p
WHERE p.recording_id = r.id;

ALTER TABLE recordings
    ADD CONSTRAINT recordings_until_encoded_requires_profiles
        CHECK (keep_original <> 'until_encoded' OR cardinality(encode_profiles) > 0);

-- 00029 の元の定義に戻す（recordings の列を直接参照する形）。
CREATE OR REPLACE VIEW until_encoded_deletable_originals AS
SELECT
    a.id           AS asset_id,
    a.recording_id AS recording_id,
    a.rel_path     AS rel_path,
    a.size_bytes   AS size_bytes,
    a.state        AS state
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.kind = 'original'
  AND r.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND cardinality(r.encode_profiles) > 0
  AND NOT EXISTS (
    SELECT 1 FROM unnest(r.encode_profiles) AS want(profile)
    WHERE NOT EXISTS (
      SELECT 1 FROM media_assets e
      WHERE e.recording_id = r.id
        AND e.kind = 'encoded'
        AND e.state = 'active'
        AND e.profile = want.profile
    )
  )
  AND EXISTS (
    SELECT 1 FROM media_assets t
    WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
  );

DROP TABLE recording_encode_policy;
