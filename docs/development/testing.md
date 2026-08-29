# テスト

本ドキュメントは、Arena の unit test と integration test の範囲、実行方法、失敗時の確認を説明する。

## テストの分類

Test suite の性質を、以下にまとめる。

| 種別 | Command | Docker | 主な対象 |
| --- | --- | --- | --- |
| Unit test | `go test ./...` または `make test` | 不要 | Package logic、in-memory fake、miniredis、HTTP handler |
| Integration test | `make test-integration` | 必要 | DynamoDB Local、Valkey、floci を用いる backend behavior |

Integration test file には `integration` build tag があるため、通常の `go test ./...` には含まれない。

## Unit test

全 unit test の実行例を、以下に示す。

```console
$ make test
```

Race detector を追加する例を、以下に示す。

```console
$ go test -race ./...
```

Package と test 名を限定する例を、以下に示す。

```console
$ go test ./internal/controller -run TestFleetShard -v
```

各 package は必要な Store、Pool、AWS API を小さい interface で受け取り、test file 内の fake を使用する。Fake は DynamoDB と同じ condition failure、version conflict、state transition を返す必要がある。

## Integration test

全 integration test の command は次のとおりである。

```console
$ make test-integration
```

Make target は次の Go command を実行する。

```console
$ go test -tags integration -count=1 -race -timeout 10m ./test/integration/...
```

TestMain が起動する container を、以下に示す。

| Image | 用途 |
| --- | --- |
| `amazon/dynamodb-local:2.5.2` | Table、index、transaction、condition expression |
| `valkey/valkey:8.1` | Ready pool、heartbeat、epoch、Counter data |
| `floci/floci:latest` | ECS、SQS、STS、EC2 の local API |

floci の ECS は host Docker daemon で container を起動するため、`/var/run/docker.sock` へアクセスできる必要がある。

## Integration test の範囲

現行 suite が検証する behavior は次のとおりである。

- 並行 state transition の単一勝者
- GameServer claim と追加 Allocation の DynamoDB transaction
- Fleet index の state prefix query
- Fleet version conflict
- Controller leader lease
- 同じ idempotency key の並行 Allocation
- 競合候補の skip と異なる key の割り当て
- Release 後の Ready pool 復帰
- ECS launcher と sidecar identity verifier
- Redis pool epoch の再構築
- Controller の起動から終了までの lifecycle
- 複数 controller による Fleet shard 分担

実 AWS service、Application Load Balancer、CloudWatch EMF 抽出、各言語の公式 Agones SDK、Terraform deployment は integration test の範囲外である。

## 個別実行

Integration test を 1 個だけ実行する例を、以下に示す。

```console
$ go test -tags integration -count=1 -run TestLeaderLease ./test/integration/...
```

TestMain は個別実行でも 3 container をすべて起動する。

## 失敗時の確認

Container 起動前後の失敗では、Docker daemon、image pull、Docker socket の permission、利用可能な port と disk 容量を確認する。

DynamoDB の test failure では table 作成、GSI が ACTIVE になるまでの待機、condition expression を確認する。Valkey の failure では共有 instance に残る key と test ごとの一意な Fleet ID を確認する。floci の failure では対応する ECS または SQS API と host container の起動状態を確認する。

Test は終了時に container を削除する。失敗中の container log が必要な場合は、別 terminal で `docker ps` と `docker logs` を使用する。

## 生成物の確認

Protobuf 変更では、以下の command を追加で実行する。

```console
$ make gen-check
```

この target は生成を行った後、`git diff --exit-code gen/` で追跡済み生成物との差分を確認する。Buf と generator の install が必要である。
