package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/epgimport"
	"github.com/fetburner/rokuban/internal/epgstation"
)

// newImportCmd は `rokuban import` サブコマンドを作る（issue #72 / M3-10）。
// 現在は epgstation の 1 つだけを持つが、将来の移行元が増えても cobra の
// サブコマンド構造がそのまま拡張の置き場所になる。
func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "他システムからの移行コマンド",
	}
	cmd.AddCommand(newImportEPGStationCmd())
	return cmd
}

// newImportEPGStationCmd は `rokuban import epgstation` サブコマンドを作る。
//
// EPGStation のルール・ライブラリを rokuban の永続資産（rules /
// recordings / media_assets）へ取り込む恒久コマンド（移行専用の使い捨て
// スクリプトにしない）。--rules / --library-json は独立に指定でき、
// 両方同時も可能。冪等・途中再開可能（各インポート関数の doc コメント参照）。
//
// RecordedHistory（EPGStation 側の再放送重複排除の種）の取り込みは
// このコマンドに含まない: rokuban の重複排除（internal/ruler/dedupe.go）は
// recordings.rule_id が一致する行だけを比較対象にするが、in-place 登録
// （internal/inplace.Register）は rule_id を書く列を持たず、
// RecordedHistory 自体にも ruleId が無いため、取り込んでも重複排除には
// 一切効かない。internal/ruler 側の対応と合わせて別途取り込む
// （issue #72 のコメント参照）。
func newImportEPGStationCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "epgstation",
		Short: "EPGStation からルール・ライブラリを取り込む",
		Long: `EPGStation のルール（--rules）・ライブラリ（--library-json）を
rokuban に取り込む。冪等: 再実行しても行は増えない。

--rules は EPGStation の REST API（GET /api/rules）を叩く。
--library-json は EPGStation の DB（Recorded/VideoFile/Thumbnail）から
運用者が事前に書き出した JSON を読む —— EPGStation の REST API は実ファイルの
相対パスを公開していないため（internal/epgimport/library.go の doc コメント
参照）。JSON の形・書き出し方は docs/runbook/import-epgstation.md 参照。

ARE（Postgres 正規表現）非互換の正規表現、%CHNAME% 等の未対応テンプレート
変数、時刻指定ルール、条件が 1 つも残らなかったルールはいずれも警告として
出力し、安全側にスキップ/縮退する（黙って空文字にしたり EPG 全体を録る
ルールを有効なまま作ったりしない）。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			doRules, err := cmd.Flags().GetBool("rules")
			if err != nil {
				return err
			}
			libraryPath, err := cmd.Flags().GetString("library-json")
			if err != nil {
				return err
			}
			epgstationURL, err := cmd.Flags().GetString("epgstation-url")
			if err != nil {
				return err
			}
			if !doRules && libraryPath == "" {
				return fmt.Errorf("at least one of --rules, --library-json is required")
			}
			if doRules && epgstationURL == "" {
				return fmt.Errorf("--epgstation-url is required with --rules")
			}

			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			// 他の単発 CLI コマンド（rescue/shadow-diff）と同じく、多サイトの
			// 意味論を決める書き手がまだいないので単一サイトに限定する
			// （不変条件 11）。
			site, err := requireSingleSite(cfg.Registry(), "import epgstation")
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			pool, err := db.NewPool(ctx, cfg.DB, nil, 0)
			if err != nil {
				return err
			}
			defer pool.Close()

			out := cmd.OutOrStdout()

			if doRules {
				client := epgstation.NewClient(epgstationURL, nil)
				result, err := runImportRules(ctx, pool, client, site.Site)
				if err != nil {
					return fmt.Errorf("importing rules: %w", err)
				}
				printRuleImportResult(out, result)
			}

			if libraryPath != "" {
				items, err := readJSONFile[epgimport.LibraryItem](libraryPath)
				if err != nil {
					return fmt.Errorf("reading --library-json: %w", err)
				}
				result, err := epgimport.ImportLibrary(ctx, pool, cfg.Storage.MediaDir, site.Site, items)
				if err != nil {
					return fmt.Errorf("importing library: %w", err)
				}
				printLibraryImportResult(out, result)
			}

			return nil
		},
	}

	cmd.Flags().Bool("rules", false, "EPGStation のルールを取り込む（--epgstation-url が必要）")
	cmd.Flags().String("epgstation-url", "", "EPGStation の base URL（例: http://localhost:8888。--rules 用）")
	cmd.Flags().String("library-json", "", "EPGStation のライブラリを表す JSON ファイルのパス（形は docs/runbook/import-epgstation.md 参照）")

	return cmd
}

// runImportRules は EPGStation から /api/rules を取得して ImportRules に渡す。
// cobra の RunE は配線に留める既存の流儀（runRescue/runShadowDiff）に倣う。
func runImportRules(ctx context.Context, pool *pgxpool.Pool, client *epgstation.Client, site string) (epgimport.RuleImportResult, error) {
	rules, err := client.ListRules(ctx)
	if err != nil {
		return epgimport.RuleImportResult{}, fmt.Errorf("listing EPGStation rules: %w", err)
	}
	return epgimport.ImportRules(ctx, pool, site, rules)
}

// readJSONFile はトップレベルが配列の JSON ファイルを読む小さなヘルパー。
func readJSONFile[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var items []T
	if err := json.NewDecoder(f).Decode(&items); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	return items, nil
}

func printRuleImportResult(out io.Writer, r epgimport.RuleImportResult) {
	_, _ = fmt.Fprintln(out, "-- rules --")
	_, _ = fmt.Fprintf(out, "  created: %d\n", r.Created)
	_, _ = fmt.Fprintf(out, "  updated: %d\n", r.Updated)
	_, _ = fmt.Fprintf(out, "  skipped: %d\n", r.Skipped)
	for _, w := range r.Warnings {
		_, _ = fmt.Fprintf(out, "  warning (epgstation rule %d): %s\n", w.EpgstationRuleID, w.Message)
	}
}

func printLibraryImportResult(out io.Writer, r epgimport.LibraryImportResult) {
	_, _ = fmt.Fprintln(out, "-- library --")
	_, _ = fmt.Fprintf(out, "  registered: %d\n", r.Registered)
	_, _ = fmt.Fprintf(out, "  skipped:    %d\n", r.Skipped)
	for _, w := range r.Warnings {
		_, _ = fmt.Fprintf(out, "  warning: %s\n", w)
	}
}
