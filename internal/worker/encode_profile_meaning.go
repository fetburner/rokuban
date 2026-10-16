package worker

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// CheckEncodeProfileMeaning は、過去の成果物の証拠（active な encoded と作り直し
// 要求の行）を持つ名前のプロファイルが、今の設定で同じ意味（cut と container）かを
// 検査し、食い違えばエラーを返す。
//
// 録画の desired はプロファイル名の集合として凍結され、encode は実行時に名前から
// 今の定義を引く。名前が成果物の意味の契約なので、同じ名前のまま意味を変えると
// 既存の証拠と食い違い、作り直し要求が満たされないまま原本を握り続ける。
// 設定から消すことは許す（満たせない desired は見え、原本は守られる）。
// 証拠が 1 つも無い名前は過去の意味が無いので対象外。
//
// cut は rel_path の世代番号から読む（media_asset_cuts は rescue で戻らない）。
// 作り直し要求の行は cut の証拠で、container は比べない。
//
// encode を実行するロール（RequiresEncodeTools が真の worker）の起動時に呼ぶ。
// config.Load は DB を読めず、api ロールは encode を実行しない。実行する側が
// 拒めば、食い違った設定で encode が走る経路が無い。
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
			pastCut = isCutRelPath(r.RelPath, r.Profile)
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

// isCutRelPath は rel_path が `..._{profile}.g{n}.{container}`（n >= 1）の形か
// を返す。EncodedRelPath が世代番号を付けるのは cut のときだけである。
// プロファイル名まで含めて照合するので、名前に `.g<数字>` を含む cut でない
// プロファイルを取り違えない。
func isCutRelPath(relPath, profile string) bool {
	safe, err := sanitizeProfileForPath(profile)
	if err != nil {
		return false
	}
	stem := strings.TrimSuffix(path.Base(relPath), path.Ext(relPath))
	i := strings.LastIndex(stem, "_"+safe+".g")
	if i < 0 {
		return false
	}
	rest := stem[i+len("_"+safe+".g"):]
	n, err := strconv.Atoi(rest)
	return err == nil && n >= 1 && rest == strconv.Itoa(n)
}
