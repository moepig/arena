# AWS リソース

本ドキュメントは、Arena の実装が使用する AWS リソースと、`samples/terraform` が示す範囲を説明する。

## 実行時の依存関係

実装が使用する AWS service と用途を、以下にまとめる。

| AWS service | 用途 |
| --- | --- |
| ECS | コントロールプレーンと GameServer Task の実行、Task Definition の登録 |
| DynamoDB | Fleet、GameServer、Allocation、Event、controller lease の保存 |
| ElastiCache または互換 Redis | Ready pool、heartbeat、通知、Counter と List の派生データ |
| EventBridge | ECS Task state change event の抽出 |
| SQS | Task event の controller への配送と再送 |
| EC2 API | GameServer ENI の public IP 解決 |
| CloudWatch Logs | アプリケーションログと EMF metric の取り込み |
| AWS STS | コントロールプレーン API の IAM principal 検証 |

## DynamoDB table

`internal/store` が要求する table を、以下に示す。既定の table prefix は `arena-` である。

| suffix | 主な key と index | TTL |
| --- | --- | --- |
| `fleets` | `fleet_id`、`namespace-name-index` | なし |
| `gameservers` | `gameserver_id`、`fleet-index` | 終了済み record |
| `allocations` | `allocation_id`、`session-index`、`gameserver-index` | 解放済み record |
| `leases` | `lease_name` | なし |
| `events` | `resource` と `ts` | 7 日後 |

Table は On-Demand capacity を前提とする。`gameservers`、`allocations`、`events` では `ttl` attribute を有効にする必要がある。

## ネットワーク

`arena-api` と `arena-controller` は DynamoDB、Redis、ECS、SQS、EC2 API へ到達できる必要がある。GameServer Task は SDK Gateway と CloudWatch Logs へ到達できる必要がある。

Public IP を割り当てる構成では、GameServer の ENI に設定した security group でゲーム用 TCP または UDP port を許可する。Private address を返す構成では、ゲームクライアントからその address への経路を別途用意する必要がある。

コントロールプレーン API を Application Load Balancer の後ろに配置する場合は、Connect と gRPC stream を転送できる HTTP/2 構成が必要である。

## IAM

必要な権限の境界を、以下にまとめる。

| Task role | 主な権限 |
| --- | --- |
| `arena-api` | DynamoDB 読み書き、sidecar 検証用 `ecs:DescribeTasks`、STS token 検証時の通信 |
| `arena-controller` | DynamoDB 読み書き、ECS Task の起動と停止、Task Definition 登録、`iam:PassRole`、SQS 消費、ENI 参照 |
| GameServer | ゲーム自体が必要とする権限。Arena の DynamoDB、Redis、ECS 権限は不要 |
| Task execution role | image pull、secret 取得、CloudWatch Logs への出力 |

`iam:PassRole` は GameServer の Task role と Task execution role に限定する必要がある。

## Terraform サンプル

`samples/terraform` は VPC、ECS cluster、DynamoDB、ElastiCache、EventBridge、SQS、IAM、CloudWatch alarm の構成例を示す。設計の参照には利用できるが、現行バイナリをそのまま起動する構成ではない。

未補完の事項は次のとおりである。

- `events` DynamoDB table が定義されていない
- `arena-api` と `arena-controller` の command-line flag が Task Definition に設定されていない
- `arena-api` に存在しない `/healthz` が ALB health check に指定されている
- `arena-sidecar` の image、controller の subnet、security group、role、gateway、log group の設定が接続されていない
- 認証用 `-authz-file` と `-server-id` が設定されていない
- Container image の build と publish は含まれない

> [!WARNING]
> `samples/terraform` を本番環境へそのまま適用してはいけない。上記の不足を補い、`terraform plan`、`terraform validate`、実 AWS 環境での接続試験を実施する必要がある。

## ローカル代替

`compose.yaml` が起動する代替 service を、以下に示す。

| 対象 | Local service | Host port |
| --- | --- | --- |
| DynamoDB | DynamoDB Local | `18000` |
| Redis | Valkey | `16379` |
| ECS、SQS、STS、EC2 | floci | `14566` |

Compose は table を作成しない。table schema は `internal/store.EnsureTables` に実装されているが、これを実行する standalone command はリポジトリに含まれない。バックエンドを含む検証には、table を自動作成する統合テストの利用を推奨する。
