# kubeconform 用の CRD スキーマ（vendoring）

`kubeconform` は CRD のスキーマを知らない。`deploy/k8s/base` が KEDA の
`ScaledJob` を出荷するようになったので、**CI の `manifests` ジョブが未知の kind で
落ちる**。選択肢は 2 つあり、こちらを採った。

- `-ignore-missing-schemas` --- **採らない。** 未知の kind を黙って飛ばすので、
  `kind: ScaledJobb` のような typo も一緒に飛ばす。「検査した」と「飛ばした」が
  出力で区別できるとしても、区別するのは人間になる
- **スキーマをリポジトリに置いて `-schema-location` で渡す** --- こちら。
  ネットワーク依存も上流 drift も入らない

CRDs-catalog を CI 時に fetch する形も採っていない（同じ理由）。

## 何が入っているか

| ファイル | 出どころ |
|---|---|
| `scaledjob_v1alpha1.json` | KEDA **v2.20.2** の `keda-2.20.2.yaml` に入っている `scaledjobs.keda.sh` CRD の `spec.versions[v1alpha1].schema.openAPIV3Schema` |

**ファイル名は小文字**である。`kubeconform` の `{{.ResourceKind}}` は小文字化した
名前を埋めるので、`ScaledJob_v1alpha1.json` にすると **macOS では通って Linux の
CI だけが落ちる**（大文字小文字を区別するファイルシステムかどうかの差。実測）。

## `-strict` で効かせるための加工

**素の CRD スキーマをそのまま置くと、`-strict` を付けても未知のキーを弾かない。**
`kubeconform` の `-strict` は「`-strict` サフィックス付きのスキーマファイルを
取りに行く」だけの仕組みで、こちらが渡したファイルの中身を厳しくはしない
（実測: `pollingIntervall` の typo が Valid で通った）。

そこで、**`properties` を持つオブジェクトに `additionalProperties: false` を
入れた版**を置いてある（`x-kubernetes-preserve-unknown-fields` が立っている枝は
素通し）。この加工を入れた後は、同じ typo が
`additional properties 'pollingIntervall' not allowed` で落ちる（実測）。

## 版を上げるとき

KEDA の版は 3 か所にある。**同じ PR で揃えること。**

- `deploy/k8s/e2e/lib/env.sh` の `E2E_KEDA_VERSION`（ハーネスが入れる版）
- このファイルの表（スキーマの出どころ）
- 生成したスキーマそのもの

生成手順（生成器は `crdschema/` にある。**手順を口伝にしない** --- 版を上げる
人が加工の中身を再発明することになる）:

```sh
curl -sSfLO https://github.com/kedacore/keda/releases/download/v2.20.2/keda-2.20.2.yaml
go run ./deploy/k8s/schemas/crdschema keda-2.20.2.yaml scaledjobs.keda.sh v1alpha1 \
  > deploy/k8s/schemas/scaledjob_v1alpha1.json
```
