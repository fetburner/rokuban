-- ruler が読み取りと書き込みの間に入力が変わったかを照合する版。

-- name: GetRulerInputRevision :one
SELECT COALESCE(
    (SELECT revision FROM ruler_input_revisions WHERE site = $1),
    0
)::bigint AS revision;

-- name: BumpRulerInputRevision :one
INSERT INTO ruler_input_revisions (site, revision)
VALUES ($1, 1)
ON CONFLICT (site) DO UPDATE SET revision = ruler_input_revisions.revision + 1
RETURNING revision;

-- name: LockRulerInputRevision :one
INSERT INTO ruler_input_revisions (site, revision)
VALUES ($1, 0)
ON CONFLICT (site) DO UPDATE SET revision = ruler_input_revisions.revision
RETURNING revision;
