# 開発者向け情報

本ドキュメントは、Arena 自体を変更するための開発環境、リポジトリ構成、関連文書を説明する。

## 必要な tool

開発内容ごとの tool を、以下にまとめる。

| Tool | 必要となる作業 |
| --- | --- |
| Go 1.26 以上 | Build、unit test、すべての Go 変更 |
| Docker | Integration test、local service の起動 |
| Buf と protobuf plugin | `.proto` と生成物の更新 |
| Terraform | `samples/terraform` の検証 |

`make tools` は Buf、`protoc-gen-go`、`protoc-gen-connect-go`、`protoc-gen-connect-openapi` が `$GOPATH/bin` にない場合、`@latest` で install する。Network access が必要であり、version は lock されていない。

## リポジトリ構成

主要な path を、以下にまとめる。

| Path | 内容 |
| --- | --- |
| `api/proto/arena/v1` | 公開 control-plane API と local SDK |
| `api/proto/arena/gateway/v1` | Sidecar と `arena-api` の内部 stream protocol |
| `api/proto/agones` | Agones 互換 SDK protobuf |
| `gen` | `buf generate` による Go と OpenAPI の生成物 |
| `cmd` | 5 個の server、sidecar、CLI entry point |
| `internal/api` | Control-plane RPC handler |
| `internal/allocation` | Allocation algorithm |
| `internal/auth` | IAM token と role authorization |
| `internal/controller` | Reconcile、autoscaling、event、lease、Redis rebuild |
| `internal/ecs` | ECS Task Definition、RunTask、StopTask、identity 検証 |
| `internal/gateway` | SDK Gateway |
| `internal/manifest` | Fleet YAML の decode と encode |
| `internal/pool` | Redis 派生データ |
| `internal/router` | Multi-region Allocation routing |
| `internal/sidecar` | Arena SDK と Agones 互換 SDK |
| `internal/store` | DynamoDB record と operation |
| `internal/telemetry` | EMF、OpenMetrics、OpenTelemetry |
| `pkg/sdk` | 外部 import を想定する Arena Go SDK |
| `test/integration` | Build tag `integration` の backend 統合 test |
| `samples/terraform` | AWS resource 構成例 |
| `docs` | 利用者、運用、開発向け文書 |

`internal` 以下は module 外から import できない。外部の game server code が import する package は `pkg/sdk` である。

## 開発文書

通常の変更手順、protobuf 生成、文書更新は、[workflow.md](workflow.md) を参照。

Unit test と integration test の分担は、[testing.md](testing.md) を参照。

System のデータ境界と処理の関係は、[../arena/architecture.md](../arena/architecture.md) を参照。
