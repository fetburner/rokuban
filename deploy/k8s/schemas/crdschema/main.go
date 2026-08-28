// Command crdschema は CustomResourceDefinition の openAPIV3Schema を
// kubeconform に渡せる JSON スキーマに変換する。
//
// `deploy/k8s/schemas/` に置くファイルの生成器である。**生成物をコミットする**
// （CI がネットワークから CRD を取りに行くと、リポジトリ側の変更ゼロで検査内容が
// 上流に引きずられる。deploy/k8s/schemas/README.md）。
//
//	go run ./deploy/k8s/schemas/crdschema keda-2.20.2.yaml scaledjobs.keda.sh v1alpha1 \
//	  > deploy/k8s/schemas/scaledjob_v1alpha1.json
//
// **ファイル名は小文字にする**（kubeconform の `{{.ResourceKind}}` は小文字化
// した名前を埋めるので、大文字にすると macOS だけで通る）。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/goccy/go-yaml"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: crdschema <crd-manifest.yaml> <crd-name> <version>")
		os.Exit(2)
	}
	schema, err := extract(os.Args[1], os.Args[2], os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := json.MarshalIndent(strict(schema), "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

// extract は manifest の中から CRD 1 件の openAPIV3Schema を取り出す。
func extract(path, crdName, version string) (any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decoding %s: %w", path, err)
		}
		if doc["kind"] != "CustomResourceDefinition" {
			continue
		}
		md, _ := doc["metadata"].(map[string]any)
		if name, _ := md["name"].(string); name != crdName {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		versions, _ := spec["versions"].([]any)
		for _, v := range versions {
			ver, _ := v.(map[string]any)
			if ver["name"] != version {
				continue
			}
			schema, _ := ver["schema"].(map[string]any)
			if schema["openAPIV3Schema"] == nil {
				return nil, fmt.Errorf("%s/%s has no openAPIV3Schema", crdName, version)
			}
			return schema["openAPIV3Schema"], nil
		}
		return nil, fmt.Errorf("%s has no version %q", crdName, version)
	}
	return nil, fmt.Errorf("%s not found in %s", crdName, path)
}

// strict は `properties` を持つオブジェクトに `additionalProperties: false` を
// 入れる。
//
// **これが無いと `-strict` が効かない。** kubeconform の `-strict` は
// 「`-strict` サフィックス付きのスキーマファイルを取りに行く」仕組みでしか
// なく、`-schema-location` で渡したファイルの中身は厳しくしない（実測:
// `pollingIntervall` という typo が Valid で通った）。
//
// `x-kubernetes-preserve-unknown-fields` が立っている枝は素通しする ---
// そこは CRD 自身が「未知のキーを許す」と宣言している場所なので、閉じると
// 正しいマニフェストが落ちる。
func strict(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = strict(e)
		}
		if _, hasProps := t["properties"]; !hasProps {
			return t
		}
		if t["x-kubernetes-preserve-unknown-fields"] == true {
			return t
		}
		if _, ok := t["additionalProperties"]; !ok {
			t["additionalProperties"] = false
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = strict(e)
		}
		return t
	default:
		return v
	}
}
