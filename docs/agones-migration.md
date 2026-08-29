# Agones からの移行

本ドキュメントは、Agones 上の専用ゲームサーバーを Arena へ移行する際の概念対応、SDK の互換範囲、設計差分、移行手順を説明する。

## 概念対応

主要な resource と機能の対応を、以下にまとめる。

| Agones | Arena | 差分 |
| --- | --- | --- |
| Kubernetes cluster | ECS cluster | Kubernetes API と node resource は使用しない |
| Fleet CRD | Arena Fleet | Protobuf API と ECS 形式の YAML manifest で管理する |
| GameServer CRD | Arena GameServer | Fleet からのみ作成する |
| GameServerSet | `generation` と `spec_hash` | 中間 resource を公開しない |
| GameServerAllocation | AllocationService | `idempotency_key` が必須 |
| FleetAutoscaler | `Fleet.spec.autoscaling` | Fleet 内の設定として保持する |
| Agones SDK sidecar | `arena-sidecar` | gRPC service 名と主要 RPC、REST route を実装する |
| Counters と Lists | Agones beta 互換 service | Sidecar memory を一次状態とする |
| Kubernetes RBAC | IAM token と Arena role | Namespace pattern を file で設定する |
| Multi-cluster allocation | `arena-router` | 静的な region policy で転送する |

## SDK 互換範囲

`arena-sidecar` は `agones.dev.sdk.SDK` と `agones.dev.sdk.beta.SDK` の protobuf service を localhost port 9357 で提供する。REST 互換 route は port 9358 で提供する。

実装済みの stable SDK RPC を、以下に示す。

- Ready
- Allocate
- Shutdown
- Health
- GetGameServer
- WatchGameServer
- SetLabel
- SetAnnotation
- Reserve

実装済みの beta SDK RPC を、以下に示す。

- GetCounter
- UpdateCounter
- GetList
- UpdateList
- AddListValue
- RemoveListValue

Player Tracking RPC は実装しない。GameServer の Agones state に直接対応しない Arena state は、Scheduled、Shutdown、Unhealthy など最も近い Agones state 名へ変換する。

> [!IMPORTANT]
> リポジトリのテストは Arena 内の生成 client と handler の互換性を検証する。各言語の公式 Agones SDK と実 ECS 環境を組み合わせた相互運用試験は、導入側で実施する必要がある。

## 接続先

公式 Agones SDK が参照する環境変数を Arena sidecar も使用する。

| 環境変数 | 既定値 |
| --- | --- |
| `AGONES_SDK_GRPC_PORT` | `9357` |
| `AGONES_SDK_HTTP_PORT` | `9358` |

Game process と sidecar を同じ ECS Task へ配置し、localhost で接続する。Game process から DynamoDB、Redis、Arena の control-plane credential は不要である。

## Allocation の差分

Arena の Allocation request には再送時の一意な `idempotency_key` が必要である。同一 key は同じ Allocation ID へ変換される。Matchmaker は timeout または一時 error の再送でも同じ key を使用する必要がある。

Arena は `fleet_name` または Fleet label の `fleet_selector` で対象 Fleet を選ぶ。GameServer の label、`id`、`spec_hash`、Counter capacity を selector として使用できる。Preferred selector は候補の順序だけを変え、候補を除外しない。

Allocation response の `address` と `ports` は、game client が直接接続する宛先である。Agones 環境で NodePort、LoadBalancer、Ingress、独自 proxy を前提にしていた場合は接続処理を変更する必要がある。

## Fleet manifest の差分

Arena manifest は Kubernetes object ではなく、ECS Task Definition と Service に近い field 名を持つ。`apiVersion`、`kind`、Pod template、container port policy などをそのまま移植できない。

変換時の主な対応は次のとおりである。

| Agones Fleet | Arena manifest |
| --- | --- |
| `metadata.name` | `name` |
| `metadata.namespace` | `namespace` |
| `spec.replicas` | `desiredCount` |
| `spec.template.metadata.labels` | `taskDefinition.tags` |
| Pod container image | `taskDefinition.containerDefinitions[].image` |
| Container port | `portMappings[]` |
| GameServer health | Game container の `healthCheck` |
| RollingUpdate | `strategy.rollingUpdate` |
| Allocation overflow | `allocationOverflow` |

完全な schema は、[arenactl/manifest.md](arenactl/manifest.md) を参照。

## Lifecycle の差分

Arena の GameServer は Scheduled、Starting、Ready、Reserved、Allocated、Draining、Unhealthy、Terminated を使用する。ECS Task の RUNNING event で Starting へ移り、SDK `Ready` で Ready へ移る。

Allocated から SDK `Ready` を呼ぶと Allocation を解放して同じ Task を再利用する。SDK `Shutdown` は Draining へ移し、controller が ECS Task を停止する。

Arena の sidecar は最初の SDK `Health` 呼び出しまで upstream heartbeat を送る。最初の呼び出し後に game process の Health が止まると upstream heartbeat も停止する。この挙動を前提に、game loop から継続して Health を呼ぶ必要がある。

## Counter と List の差分

Counter と List は Fleet template で事前宣言しない。Game process が SDK で作成または更新する。Sidecar の再起動では memory 上の値が失われるため、起動時に必要な capacity と value を設定する必要がある。

Arena は Counter を Allocation filter、priority、同一 GameServer への追加 Allocation、Fleet autoscaling に使用できる。List は sidecar と Redis へ同期するが、現行 Allocation API の filter には使用しない。

## 運用の差分

Kubernetes event に相当する履歴は EventService が提供する。保持期間は 7 日であり、best effort である。Log は CloudWatch Logs、metric は EMF と OpenMetrics を使用する。

Kubernetes RBAC は使用しない。Control-plane API は presigned STS token で IAM principal を確認し、YAML binding の `admin`、`fleet-editor`、`allocator`、`viewer` role へ対応付ける。

## 移行手順

移行は次の順序で行う。

1. GameServer の SDK 呼び出しを一覧化し、Arena の互換範囲に含まれることを確認する。
2. Game client の接続処理を、Allocation response の address と port への直接接続へ対応させる。
3. Fleet CRD を Arena manifest へ変換し、`arenactl diff` で server-side validation を実行する。
4. Game process の起動時に Counter と List を初期化し、Health を継続送信する。
5. Matchmaker の Allocation に安定した `idempotency_key` を追加する。
6. 検証 Fleet で Ready、Allocation、WatchGameServer、Release または Ready、Shutdown を確認する。
7. Spot interruption、Redis restart、controller failover、SDK Gateway reconnect を検証する。
8. 負荷試験で Allocation latency、Ready buffer、ECS Task 起動 rate を確認する。

## 非対応または制約

移行判断に影響する現行の制約は次のとおりである。

- Fleet に属さない単一 GameServer の作成 API はない
- Player Tracking はない
- Manifest は Passthrough port と TCPUDP を表現しない
- List による Allocation filter はない
- `arena-router` は Authorization header を region endpoint へ転送しない
- Terraform サンプルは本番環境へそのまま適用できない

AWS 配置の制約は、[arena/aws-resources.md](arena/aws-resources.md) を参照。
