package worker

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// CheckEncodeProfileMeaning は、active な encoded を持つ名前のプロファイルが、
// 今の設定で過去と同じ意味（cut と container）かを検査し、食い違えばエラーを返す。
//
// 録画の desired はプロファイル名の集合として凍結され、encode は実行時に名前から
// 今の定義を引く。名前が成果物の意味の契約なので、同じ名前のまま意味を変えると
// 既存の encoded と食い違い、作り直し要求が満たされないまま原本を握り続ける。
// 設定から消すことは許す（満たせない desired は見え、原本は守られる）。
// active な encoded が 1 本も無い名前は過去の意味が無いので対象外。
//
// encode を実行するロール（RequiresEncodeTools が真の worker）の起動時に呼ぶ。
// config.Load は DB を読めず、api ロールは encode を実行しない。実行する側が
// 拒めば、食い違った設定で encode が走る経路が無い。
func CheckEncodeProfileMeaning(ctx context.Context, db sqlcgen.DBTX, enc config.EncodeConfig) error {
	rows, err := sqlcgen.New(db).ListActiveEncodedMeanings(ctx)
	if err != nil {
		return fmt.Errorf("listing active encoded meanings: %w", err)
	}
	for _, r := range rows {
		p, ok := enc.Profile(r.Profile)
		if !ok {
			continue
		}
		if p.Cut != r.Cut {
			return fmt.Errorf("encode profile %q: cut=%t but active encoded assets were made with cut=%t; "+
				"a name must keep its meaning, so give the changed profile a new name", r.Profile, p.Cut, r.Cut)
		}
		if p.Container != r.Container {
			return fmt.Errorf("encode profile %q: container=%q but active encoded assets use %q; "+
				"a name must keep its meaning, so give the changed profile a new name", r.Profile, p.Container, r.Container)
		}
	}
	return nil
}
