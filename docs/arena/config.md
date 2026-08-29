# 起動設定

本ドキュメントは、Arena の各バイナリが読み取る command-line flag と環境変数を説明する。Fleet の設定は、[../arenactl/manifest.md](../arenactl/manifest.md) を参照。

## AWS SDK の設定

`arena-api`、`arena-controller`、`arenactl` の AWS client は AWS SDK for Go v2 の標準 credential と region 解決を使用する。`AWS_REGION`、AWS profile、web identity、ECS Task role、`AWS_ENDPOINT_URL_<SERVICE>` などを利用できる。

Arena 独自の table 設定は個別 table 名ではなく `-table-prefix` で指定する。

## arena-api

`arena-api` の flag を、以下に示す。

| Flag | 既定値 | 用途 |
| --- | --- | --- |
| `-listen` | `:8080` | API と `/metrics` の listen address |
| `-redis` | `localhost:6379` | Redis endpoint |
| `-table-prefix` | `arena-` | DynamoDB table prefix |
| `-cluster` | 空 | Sidecar identity を検証する ECS cluster。空の場合は検証しない |
| `-authz-file` | 空 | IAM principal と role の binding file。空の場合は認証しない |
| `-server-id` | 空 | Token を束縛する公開 host。`-authz-file` と同時指定が必須 |
| `-otlp-endpoint` | 空 | OTLP/gRPC trace collector。空の場合は tracing を無効にする |

`arena-api` は起動時に Redis へ接続して pool epoch を同期する。Redis へ接続できない場合は起動に失敗する。

## arena-controller

`arena-controller` の flag を、以下に示す。

| Flag | 既定値 | 用途 |
| --- | --- | --- |
| `-redis` | `localhost:6379` | Redis endpoint |
| `-table-prefix` | `arena-` | DynamoDB table prefix |
| `-shard-count` | `1` | Fleet reconcile 用 lease の shard 数 |
| `-queue-url` | 空 | ECS Task event の SQS queue URL。空の場合は consumer を無効にする |
| `-metrics-listen` | `:9090` | `/metrics` の listen address。空の場合は HTTP server を無効にする |
| `-cluster` | `arena` | GameServer Task を実行する ECS cluster |
| `-subnets` | 空 | GameServer Task の subnet ID。Comma 区切り |
| `-security-groups` | 空 | GameServer Task の security group ID。Comma 区切り |
| `-assign-public-ip` | `true` | GameServer Task へ public IP を割り当てるか |
| `-execution-role-arn` | 空 | GameServer の Task execution role ARN |
| `-task-role-arn` | 空 | GameServer の Task role ARN |
| `-sidecar-image` | 空 | 注入する `arena-sidecar` image |
| `-gateway-endpoint` | 空 | Sidecar に渡す SDK Gateway URL |
| `-log-group` | 空 | GameServer container の CloudWatch Logs group |
| `-run-tasks-per-second` | `5` | ECS `RunTask` の rate limit |

Subnet、security group、role、sidecar image、gateway endpoint を設定しない場合も process 自体は起動するが、GameServer Task の起動は正常に完了しない。

## arena-sidecar

`arena-sidecar` の flag と対応する環境変数を、以下に示す。

| Flag | 既定値 | 用途 |
| --- | --- | --- |
| `-listen` | `localhost:$AGONES_SDK_GRPC_PORT`、未設定時 `localhost:9357` | Arena と Agones gRPC SDK |
| `-listen-http` | `localhost:$AGONES_SDK_HTTP_PORT`、未設定時 `localhost:9358` | Agones REST 互換 API |
| `-gateway` | `$ARENA_GATEWAY_ENDPOINT` | SDK Gateway URL。必須 |
| `-gameserver-id` | `$ARENA_GAMESERVER_ID` | GameServer ID。必須 |

ECS は `ECS_CONTAINER_METADATA_URI_V4` を自動設定する。Sidecar はこの endpoint から Task ARN を取得する。ECS 外では Task ARN を空のまま接続するため、`arena-api -cluster` を使用する環境には接続できない。

## arena-router

`arena-router` の flag を、以下に示す。

| Flag | 既定値 | 用途 |
| --- | --- | --- |
| `-listen` | `:8090` | AllocationService の listen address |
| `-config` | `$ARENA_ROUTER_CONFIG` | Region policy JSON の path。必須 |

Region policy の形式を、以下に示す。

```json
[
  {
    "name": "ap-northeast-1",
    "endpoint": "https://arena-ap-northeast-1.example.com",
    "priority": 0,
    "weight": 3
  },
  {
    "name": "us-west-2",
    "endpoint": "https://arena-us-west-2.example.com",
    "priority": 1,
    "weight": 1
  }
]
```

小さい `priority` を先に試す。同じ priority 内では `weight` に基づく無作為な順序で全 region を試す。`Allocate` は `RESOURCE_EXHAUSTED`、`Release` と `GetAllocation` は `NOT_FOUND` の場合だけ次の region へ進む。

> [!IMPORTANT]
> 現行の `arena-router` は受信した Authorization header を region client へ転送せず、router 自身にも認証 interceptor がない。認証を有効にした `arena-api` への接続には、credential forwarding または region client interceptor の実装が別途必要である。

## arenactl

すべての command で利用できる接続設定を、以下に示す。

| Flag | 既定値 | 用途 |
| --- | --- | --- |
| `-s`、`--server` | `$ARENA_SERVER`、未設定時 `http://localhost:8080` | `arena-api` endpoint |
| `-auth` | `$ARENA_AUTH`、未設定時 `none` | `none` または `iam` |
| `-f` | なし | Manifest file または directory。対応 command では反復指定可 |

Command ごとの flag は、[../arenactl/commands.md](../arenactl/commands.md) を参照。

## Go SDK

`pkg/sdk.New` は `ARENA_SDK_ADDRESS` を読み、未設定時は `http://localhost:9357` を使用する。明示的な endpoint は `sdk.NewForAddress` へ渡す。
