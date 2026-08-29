# 開発ワークフロー

本ドキュメントは、Arena の変更、protobuf 生成、確認作業の標準的な流れを説明する。

## 通常の変更

Go code の変更後に実行する基本的な確認を、以下に示す。

```console
$ gofmt -w path/to/changed.go
$ go vet ./...
$ go test ./...
$ go build ./...
```

Package を限定する場合は、以下のように実行する。

```console
$ go test ./internal/allocation/... -run TestName -v
```

変更した package の test を先に実行し、最後に `go test ./...` を実行すると良い。

## Protobuf の変更

公開 schema の変更手順は次のとおりである。

1. `api/proto` の対象 `.proto` を変更する。
2. 既存 field number を変更または再利用していないことを確認する。
3. `make gen` を実行する。
4. `gen` の Go code と `gen/openapi/arena.v1.yaml` の差分を確認する。
5. Handler、変換、保存、manifest、test、document を更新する。
6. `make gen-check` で生成物の差分が残らないことを確認する。

`make gen` が実行する処理を、以下に示す。

```console
$ buf lint
$ buf generate
```

`gen` は生成物であるため、直接編集してはいけない。`api/proto/agones` は Agones SDK の wire contract を表すため、Arena 独自の都合で field number または service 名を変更してはいけない。

## API field の追加

Fleet spec など永続化される field は、次の境界を確認する必要がある。

- Protobuf message と validation
- DynamoDB record または protobuf JSON field
- API と store の変換
- Controller、Allocation、Gateway、Sidecar の処理
- Manifest の decode と encode
- Unit test と必要な integration test
- API、manifest、運用 document

Manifest に field を追加する場合は、`arenactl get` の出力を同じ version の `arenactl apply` が読み戻せることを test する必要がある。

## DynamoDB schema の変更

Table、key、index、TTL を変更する場合は `internal/store/schema.go` と deployment 用 IaC の両方を更新する必要がある。`store.EnsureTables` は integration test と local setup 用であり、既存 production table の migration は実行しない。

Schema 変更では、既存 item の読み取り、条件付き書き込み、index backfill、rollback 方針を別途設計する必要がある。

## Redis data の変更

Redis へ追加するデータは、DynamoDB または sidecar から再構築できるものに限定する。Key format を変える場合は pool epoch、旧 key の残存、controller の rebuild 処理を確認する必要がある。

正しさを Redis の単一操作だけに依存させてはいけない。Allocation の最終確定と GameServer state は DynamoDB の条件付き書き込みで保護する。

## Local service

依存 service の起動と停止を、以下に示す。

```console
$ make compose-up
$ make compose-down
```

Compose は DynamoDB table を作成しない。Local backend を用いた自動検証には `make test-integration` を使用する。

## Terraform サンプル

Terraform file は `samples/terraform` にある。現行の `make tf-validate` は存在しない `deploy/terraform` を参照するため使用できない。

Terraform を install 済みの環境では、サンプル directory を直接指定して検証する。

```console
$ terraform -chdir=samples/terraform init -backend=false
$ terraform -chdir=samples/terraform validate
```

Backend block と provider download があるため、環境に応じた backend override と network access が必要になる場合がある。サンプルの未補完事項は、[../arena/aws-resources.md](../arena/aws-resources.md) を参照。

## 文書の更新

変更対象と文書の対応を、以下にまとめる。

| 変更 | 主な文書 |
| --- | --- |
| RPC、request、response、role | `docs/arena/api.md` |
| Binary flag、環境変数 | `docs/arena/config.md` |
| Game SDK、sidecar | `docs/arena/sdk.md` |
| Fleet YAML | `docs/arenactl/manifest.md` |
| CLI command | `docs/arenactl/commands.md` |
| AWS resource、IAM、network | `docs/arena/aws-resources.md` |
| Metric、trace、log | `docs/arena/monitoring.md` |
| Test command、coverage | `docs/development/testing.md` |

## 変更完了時の確認

変更完了時の確認項目は次のとおりである。

- `gofmt` 適用済みの Go code
- `go vet ./...`
- `go test ./...`
- `go build ./...`
- Backend 固有の変更に対する integration test
- Protobuf 変更に対する `make gen-check`
- Public behavior と文書の一致
