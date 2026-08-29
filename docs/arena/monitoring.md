# 監視

本ドキュメントは、Arena が出力する metric、OpenMetrics endpoint、trace、log の性質を説明する。

## Metric の出力経路

`arena-api` と `arena-controller` は、同じ観測値を CloudWatch Embedded Metric Format と process 内の OpenMetrics exporter へ渡す。

EMF document は標準出力へ JSON 1 行として書き出す。CloudWatch Logs の EMF 取り込みを使用する場合は、`PutMetricData` を呼び出さない。すべての Arena metric には `FleetId` dimension が付く。

OpenMetrics endpoint を、以下にまとめる。

| Process | Endpoint |
| --- | --- |
| `arena-api` | `-listen` と同じ address の `/metrics` |
| `arena-controller` | `-metrics-listen` の `/metrics`。空を指定すると無効 |

OpenMetrics exporter は metric と `FleetId` の組み合わせごとに最後の値を保持し、すべてを gauge として公開する。Event 数を表す metric も process 内で累積しない。Rate または合計が必要な場合は、収集側または CloudWatch の期間集計を使用する。

## Fleet metric

`Arena/Fleet` namespace の metric を、以下に示す。

| Metric | Unit | 内容 |
| --- | --- | --- |
| `TotalGameServers` | Count | Active GameServer の総数 |
| `ReadyGameServers` | Count | Ready 数 |
| `AllocatedGameServers` | Count | Allocated 数 |
| `StartingGameServers` | Count | Scheduled と Starting の合計 |
| `ReservedGameServers` | Count | Reserved 数 |
| `UpdatedGameServers` | Count | 現在の `spec_hash` に属する active 数 |

これらは Fleet reconcile ごとに出力する。

## Controller metric

Controller が出力する metric を、以下に示す。

| Namespace | Metric | Unit | 内容 |
| --- | --- | --- | --- |
| `Arena/Controller` | `ReconcileDuration` | Milliseconds | Fleet reconcile の所要時間 |
| `Arena/Health` | `HeartbeatTimeouts` | Count | Heartbeat timeout の検出 |
| `Arena/Health` | `UnhealthyGameServers` | Count | Unhealthy へ移行した GameServer |
| `Arena/Autoscaler` | `DesiredReplicas` | Count | Autoscaler が算出した希望台数 |
| `Arena/Autoscaler` | `ScaleUpEvents` | Count | 算出値が現在値を上回る場合は 1、それ以外は 0 |
| `Arena/Autoscaler` | `ScaleDownEvents` | Count | 算出値が現在値を下回る場合は 1、それ以外は 0 |

## Allocation metric

`arena-api` が `Arena/Allocation` namespace に出力する metric を、以下に示す。

| Metric | Unit | 内容 |
| --- | --- | --- |
| `AllocationLatency` | Milliseconds | Allocation 処理時間 |
| `PoolMiss` | Count | Ready 候補がない場合は 1、それ以外は 0 |
| `AllocationErrors` | Count | Pool miss 以外の失敗は 1、それ以外は 0 |

## OpenMetrics 名

OpenMetrics 名は namespace と metric 名を snake case で結合する。Milliseconds の metric には `_milliseconds` suffix を付ける。

代表的な出力名を、以下に示す。

| EMF | OpenMetrics |
| --- | --- |
| `Arena/Fleet` / `ReadyGameServers` | `arena_fleet_ready_game_servers` |
| `Arena/Controller` / `ReconcileDuration` | `arena_controller_reconcile_duration_milliseconds` |
| `Arena/Allocation` / `AllocationLatency` | `arena_allocation_allocation_latency_milliseconds` |
| `Arena/Health` / `HeartbeatTimeouts` | `arena_health_heartbeat_timeouts` |

`FleetId` dimension は `fleet_id` label となる。

## 推奨する監視条件

最低限の監視対象は次のとおりである。

- `PoolMiss` の期間合計
- Allocation latency の分位点
- `UnhealthyGameServers` と `HeartbeatTimeouts` の期間合計
- Fleet ごとの Ready 数と希望台数
- `UpdatedGameServers` と `TotalGameServers` の差が継続する更新
- SQS の `ApproximateAgeOfOldestMessage`
- DynamoDB throttle と error
- Redis 接続 error
- Controller process 数と lease 取得 log

`samples/terraform/modules/monitoring` には CloudWatch alarm の例がある。ただし `LeaderLeaseHeld` は現行実装が出力しないため、その alarm はそのままでは常に metric 欠損となる。

## Tracing

`arena-api -otlp-endpoint host:port` を指定すると、Connect handler の trace を OTLP/gRPC で送信する。Transport は平文である。Collector との network を信頼できる範囲へ限定する必要がある。

Trace の service name は `arena-api` である。W3C Trace Context と Baggage を伝播する。`arena-controller`、`arena-sidecar`、`arena-router` は現行実装で trace を出力しない。

## Log

各 binary は標準出力へ JSON 形式の `slog` を出力する。EMF document も同じ標準出力を使用する。Log collector は通常の application log と `_aws.CloudWatchMetrics` を含む EMF document の両方を取り込む必要がある。

状態変化の履歴は EventService からも参照できる。ただし event は best effort であり 7 日後に削除されるため、監査 log の代替ではない。認証済みの mutation RPC は `arena-api` が principal、procedure、namespace を audit log として出力する。
