# 運用

本ドキュメントは、Arena の開発用起動、AWS への配置条件、通常運用、障害時の確認項目を説明する。

## ビルドと基本検証

Go toolchain だけで実行できる検証を、以下に示す。

```console
$ go build ./...
$ go test ./...
```

`make test` は `go test ./...` と同じである。単体テストは Docker service を必要としない。

## ローカル service

開発用の DynamoDB Local、Valkey、floci は、以下のコマンドで起動する。

```console
$ make compose-up
```

Host port と AWS SDK endpoint の対応を、以下にまとめる。

| Service | Host port | AWS SDK 設定 |
| --- | --- | --- |
| DynamoDB Local | `18000` | `AWS_ENDPOINT_URL_DYNAMODB=http://localhost:18000` |
| Valkey | `16379` | `-redis localhost:16379` |
| floci | `14566` | `AWS_ENDPOINT_URL_ECS`、`AWS_ENDPOINT_URL_SQS`、`AWS_ENDPOINT_URL_STS`、`AWS_ENDPOINT_URL_EC2` |

> [!IMPORTANT]
> Compose は DynamoDB table を作成しない。Standalone の schema 初期化 command も含まれない。Local backend を自動的に構築して検証する手順は `make test-integration` である。

Local service と volume を停止して削除する command を、以下に示す。

```console
$ make compose-down
```

`compose-down` は `docker compose down -v` を実行するため、local data は回復できない。

## 統合テスト

Docker daemon が利用できる環境では、以下の command が DynamoDB Local、Valkey、floci の一時 container を起動する。

```console
$ make test-integration
```

統合テストは `-race` と 10 分の timeout を使用する。Compose とは独立しており、`make compose-up` は不要である。検証範囲は、[../development/testing.md](../development/testing.md) を参照。

## AWS 配置条件

実行前に必要な設定を、以下にまとめる。

| 対象 | 必須条件 |
| --- | --- |
| DynamoDB | `fleets`、`gameservers`、`allocations`、`leases`、`events` の 5 table と必要な index、TTL |
| Redis | `arena-api` と `arena-controller` から到達可能な Redis 互換 endpoint |
| GameServer ECS cluster | `RunTask`、`StopTask`、Task event、ENI 参照が可能な cluster |
| GameServer network | Game client、SDK Gateway、CloudWatch Logs への必要な経路 |
| Images | `arena-api`、`arena-controller`、`arena-sidecar` とゲーム用 container image |
| Event route | ECS Task state change から SQS への EventBridge rule |
| IAM | Component ごとに必要な Task role と Task execution role |

AWS リソースの詳細は、[aws-resources.md](aws-resources.md) を参照。すべての起動 flag は、[config.md](config.md) を参照。

## 起動順序

依存関係に沿った起動順序は次のとおりである。

1. DynamoDB table と Redis を作成する。
2. GameServer ECS cluster、network、IAM role、SQS event route を作成する。
3. `arena-api` を起動し、Redis 接続、DynamoDB access、`/metrics` を確認する。
4. `arena-controller` を必要な launcher flag とともに起動し、leader lease の取得を確認する。
5. `arenactl` または FleetService で Fleet を作成する。
6. GameServer Task の起動、SDK Gateway 接続、Ready への遷移、Allocation を確認する。

`arena-api` と `arena-controller` は Redis の起動時同期に失敗すると終了する。DynamoDB table の存在は最初の API 操作または controller 処理で確認される。

## 本番用の安全設定

信頼できない network へ公開する `arena-api` には、`-authz-file`、`-server-id`、`-cluster` を設定する必要がある。TLS は `arena-api` 自体では終端しないため、load balancer または proxy で終端する。

Controller には `-queue-url` を設定することを推奨する。未設定時も 5 分ごとの resync と 30 秒ごとの health sweep は動作するが、Task state change の反映が遅くなる。

Controller を冗長化する場合は同じ `-shard-count` を全 process に設定する。値を変更すると Fleet ID と shard の対応が変わるため、全 process を同じ設定へ更新する必要がある。

## 終了処理

`arena-api` は SIGINT または SIGTERM で新規受付を停止し、最大 30 秒かけて in-flight request を終了する。Trace exporter の flush timeout は 5 秒である。

`arena-controller` は signal で処理 context を終了し、保持している lease を明示的に解放する。別 process は次の取得周期で leadership を引き継ぐ。

`arena-sidecar` は SIGTERM を受けると GameServer を Draining にし、自身は停止しない。ECS の stop timeout 中にゲーム側が退避処理を行うためである。SIGINT は開発用の即時終了として扱う。

## Fleet の更新と削除

適用予定の server-side diff は、以下の command で確認する。

```console
$ go run ./cmd/arenactl diff -f fleet.yaml -s https://arena.example.com -auth iam
```

`diff` は差分がある場合に exit code 2、差分がない場合に 0 を返す。適用方法と manifest は、[../arenactl/commands.md](../arenactl/commands.md) と [../arenactl/manifest.md](../arenactl/manifest.md) を参照。

Fleet は active GameServer が存在する間は削除できない。Autoscaling を無効にし、希望台数を 0 にして GameServer の終了を待ってから削除する必要がある。

## Allocation 障害

`RESOURCE_EXHAUSTED` が増加した場合は、Fleet の Ready 数、`PoolMiss`、autoscaling 設定を確認する。Ready 数が存在するのに miss する場合は、Redis 接続、pool epoch、controller の pool repair log を確認する。

`ABORTED` が増加した場合は同一候補への claim 競合が発生している。Client は同じ idempotency key で再送できる。継続する場合は Fleet の Ready capacity と selector の絞り込みを確認する。

## GameServer 起動障害

GameServer が Starting へ進まない場合は ECS `RunTask` failure、Task Definition、subnet、security group、execution role、image pull を確認する。Scheduled または Starting の状態が 5 分を超えると controller が Unhealthy にして代替を起動する。

Starting から Ready へ進まない場合はゲーム process の `Ready` 呼び出し、sidecar log、SDK Gateway への到達性、`arena-api -cluster` の identity 検証を確認する。

## Heartbeat 障害

Ready、Allocated、Reserved が Unhealthy になる場合は、ゲーム process の `Health` 周期、sidecar と SDK Gateway の接続、Redis の heartbeat key を確認する。Sidecar は最初の Health 以降 30 秒の無通知で heartbeat を止め、controller は 60 秒の遷移後猶予を適用する。

Redis が到達不能な間、controller は heartbeat 結果だけを理由に GameServer を Unhealthy にしない。Task event による停止検知は継続する。

## Redis 復旧

Controller は Redis の障害を検出した後、復旧から 20 秒待って pool epoch を更新する。その後、DynamoDB の Ready GameServer と Redis の heartbeat から Ready pool を再構築する。

復旧しない場合は controller log の `redis unreachable`、`redis recovered`、`pool epoch bumped`、`fleet pool rebuilt` を順に確認する。Controller を起動しただけでは、起動前から正常な Redis を障害復旧として扱わない。

## EC2 instance の drain

計画停止する ECS container instance 上の GameServer は、以下の command で drain する。

```console
$ go run ./cmd/arenactl drain -cluster arena-prod -timeout 30m -force -s https://arena.example.com -auth iam instance i-0123456789abcdef0
```

既定では Ready と Reserved を直ちに drain し、Allocated はセッション終了を待つ。`-wait=false` は Allocated も直ちに drain する。`-timeout` 後に `-force` がない場合は error、ある場合は残存する Allocated を drain する。

## Backup

DynamoDB table には Point-in-Time Recovery の有効化を推奨する。Redis は派生データだけを保持するため、Arena の復旧に snapshot は必要ない。ゲーム固有の Redis 利用を同じ cluster へ混在させる場合は、そのデータに応じた backup 方針を別途定める必要がある。
