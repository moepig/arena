# メトリクス

`arena-api` / `arena-controller` は同じメトリクスセットを 2 経路で配信します:
**CloudWatch EMF**(既定、構造化ログ経由)と **Prometheus / OpenMetrics**
(`/metrics` エンドポイント)。どちらも同じ観測値をそのまま流すだけで、
別々に集計ロジックを持ちません。

## 配信経路

### CloudWatch EMF(既定)

- 標準出力に [Embedded Metric Format](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch_Embedded_Metric_Format.html)
  の JSON を 1 メトリクス発行につき 1 行で書き出します(`awslogs` → CloudWatch
  Logs → メトリクス抽出)。`PutMetricData` の API コール課金は発生しません
- すべてのメトリクスに `FleetId` ディメンションが付与され、fleet 単位で
  集計・アラームできます
- 出力先(標準出力)への書き込みが失敗してもメトリクスを失うだけで、
  arena の正しさには影響しません(メトリクス配信は best-effort)

### Prometheus / OpenMetrics(`/metrics`)

| バイナリ | 提供元 | 既定 |
|---------|-------|------|
| `arena-api` | `-listen` と同一ポート(制御 API と同居) | `:8080` |
| `arena-controller` | `-metrics-listen`(空で無効化) | `:9090` |

- 外部クライアントライブラリ(`client_golang` 等)には依存しません。
  EMF に渡した値をそのままミラーする last-value のゲージレジストリで、
  Prometheus text exposition format(version 0.0.4)で応答します
- すべて `gauge` として公開されます。`ScaleUpEvents` のような発生回数系の
  メトリクスも「1 回の観測につき 0 か 1」を最終値として保持するだけで、
  リクエストをまたいで積算されるカウンタではありません。傾向を見る場合は
  scrape 間隔でのサンプル平均・出現頻度として扱ってください
- 命名規則: `arena_<namespace を snake_case>_<メトリクス名を snake_case>`
  (Duration 系は `_milliseconds` サフィックスが付きます)。例:
  `Arena/Allocation` の `AllocationLatency` →
  `arena_allocation_allocation_latency_milliseconds`
- ラベル: ディメンションキーを snake_case 化します(`FleetId` →
  `fleet_id="shooter-jp"`)

## メトリクス一覧

### Arena/Fleet(`arena-controller`、reconcile 1 パスごと)

fleet の GameServer を状態別に数えたスナップショットです。

| メトリクス | Unit | 説明 |
|-----------|------|------|
| `TotalGameServers` | Count | active な GameServer 総数(Scheduled+Starting+Ready+Allocated+Reserved。Draining/Unhealthy/Terminated は含まない) |
| `ReadyGameServers` | Count | Ready 状態の数 |
| `AllocatedGameServers` | Count | Allocated 状態の数 |
| `StartingGameServers` | Count | Scheduled + Starting 状態の数 |
| `ReservedGameServers` | Count | Reserved 状態の数 |
| `UpdatedGameServers` | Count | active のうち、fleet の現在の `spec_hash` に一致する数(ローリングアップデート中の新世代の進捗) |

### Arena/Controller(`arena-controller`)

| メトリクス | Unit | 説明 |
|-----------|------|------|
| `ReconcileDuration` | Milliseconds | fleet 1 つ分の reconcile 1 パスにかかった時間 |

### Arena/Health(`arena-controller`)

| メトリクス | Unit | 説明 |
|-----------|------|------|
| `HeartbeatTimeouts` | Count | ハートビート失効を検知した回数(検知のたびに 1) |
| `UnhealthyGameServers` | Count | Unhealthy 化して回収した GameServer の数(発生のたびに 1) |

### Arena/Autoscaler(`arena-controller`)

autoscaling 有効な fleet の reconcile ごとに発行されます。

| メトリクス | Unit | 説明 |
|-----------|------|------|
| `DesiredReplicas` | Count | autoscaler が計算した目標 replicas(min/max クランプ後) |
| `ScaleUpEvents` | Count | この回で desired が現在値より増えたら 1、それ以外は 0 |
| `ScaleDownEvents` | Count | この回で desired が現在値より減ったら 1、それ以外は 0 |

### Arena/Allocation(`arena-api`)

`Allocate` 1 回の試行ごとに発行されます。

| メトリクス | Unit | 説明 |
|-----------|------|------|
| `AllocationLatency` | Milliseconds | 割り当て試行にかかった時間(成功・失敗を問わず常に発行) |
| `PoolMiss` | Count | Ready 在庫なしで失敗したら 1、それ以外は 0 |
| `AllocationErrors` | Count | `PoolMiss` 以外の理由で失敗したら 1、それ以外は 0 |

## 推奨アラーム

- `Arena/Allocation` `AllocationLatency` の p99 が 500ms を超える
- `Arena/Allocation` `AllocationErrors` の合計が継続して増加する(60 秒あたり数件以上)
- `Arena/Allocation` `PoolMiss` が発生し続ける(Ready 在庫の枯渇)
- SQS `ApproximateAgeOfOldestMessage` が 30 秒を超える(Task イベントの滞留)
- controller のリーダー不在(`leases` テーブルの `holder_id` / `expires_at`、または
  リース更新失敗ログで検知。詳細は [operations.md#コントローラが動いていない](operations.md#コントローラが動いていない))

上記のうち CloudWatch ベースで機械的に検知できるものは
`deploy/terraform/modules/monitoring` で `aws_cloudwatch_metric_alarm` として
定義されています。

## トレーシング

RPC 単位の分散トレースは OTel(OTLP/gRPC)で別系統です。メトリクスとは異なる
仕組みなので [operations.md#トレーシング](operations.md#トレーシング) を参照してください。
