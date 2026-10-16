package worker

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// CheckEncodeProfileMeaning は、過去の成果物の証拠（active な encoded と作り直し
// 要求の行）を持つ名前のプロファイルが、今の設定で同じ意味（cut と container）かを
// 検査し、食い違えばエラーを返す。
//
// 録画の desired はプロファイル名の集合として凍結され、encode は実行時に、凍結した
// 名前から今の定義を引く。同じ名前のまま中身を変えると、既存の成果物と食い違う
// （active な encoded は揃っているとみなすので、作り直されない）。
// 設定から消すことは許す（満たせない desired は見え、原本は守られる）。
// 証拠が 1 つも無い名前は過去の意味が無いので対象外。
//
// cut は rel_path の世代番号から読む（cutGeneration。media_asset_cuts は rescue で
// 戻らない）。作り直し要求の行は cut の証拠で、container は比べない。
//
// 呼び出すのは、cut の設定を読むロール（api と worker）の起動時で、HTTP の待ち受けと
// ジョブの購読より前である。api は自分の設定で cut かを判定して作り直し要求を書き、
// desired を凍結する。worker は ingest の凍結・encode_reconcile・encode で読む。
// 片方だけで拒むと、起動したもう片方が食い違った設定で要求行を書く。その行は cut の
// 証拠なので、設定を戻しても証拠が混ざって worker は起動できず、掃除の
// encode_reconcile も走らない。要求行がある間は until_encoded の原本を消さない
// （until_encoded_deletable_originals）。config.Load は DB を読めないので、DB に
// 繋いだ直後に置く。
//
// コスト: 起動のたびに active な encoded と要求行を全件読む。1 件消化モード
// （ScaledJob の --once）ではジョブ 1 件ごとに 1 回になる。件数に対する所要時間は
// 未計測。
func CheckEncodeProfileMeaning(ctx context.Context, db sqlcgen.DBTX, enc config.EncodeConfig) error {
	rows, err := sqlcgen.New(db).ListEncodedMeaningEvidence(ctx)
	if err != nil {
		return fmt.Errorf("listing encoded meaning evidence: %w", err)
	}
	for _, r := range rows {
		p, ok := enc.Profile(r.Profile)
		if !ok {
			continue
		}
		pastCut, container := true, ""
		if r.RelPath != "" {
			_, pastCut = cutGeneration(r.RelPath, r.Profile)
			container = strings.TrimPrefix(path.Ext(r.RelPath), ".")
		}
		if p.Cut != pastCut {
			return fmt.Errorf("encode profile %q: cut=%t but its past encoded assets or rebuild requests were made with cut=%t; "+
				"a name must keep its meaning, so give the changed profile a new name", r.Profile, p.Cut, pastCut)
		}
		if container != "" && p.Container != container {
			return fmt.Errorf("encode profile %q: container=%q but active encoded assets use %q; "+
				"a name must keep its meaning, so give the changed profile a new name", r.Profile, p.Container, container)
		}
	}
	return nil
}
