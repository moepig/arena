# Agones からの移行ガイド

arena は [Agones](https://agones.dev/) が Kubernetes 上で提供する機能を、
Kubernetes クラスタなしで **AWS ECS 上に**実現するシステムです。本書は
Agones を使っているゲームサーバー・マッチメイカーを arena へ移行する際の
対応表と、既知の差分をまとめたものです。

## 1. 概念の対応表

| Agones | arena | 備考 |
|--------|-------|------|
| Kubernetes クラスタ | ECS クラスタ(Fargate / Fargate Spot / EC2) | ノード管理が要らない場合は Fargate を推奨 |
| `Fleet` CRD | `arena.v1.Fleet`(DynamoDB レコード) | `arenactl apply -f fleet.yaml` で宣言的に管理(kubectl 相当) |
| `GameServer` CRD | `arena.v1.GameServer`(DynamoDB レコード) | 個々の CRD ではなく Fleet 経由でのみ生成される(単発 GameServer は非対応) |
| `GameServerSet` | なし | spec_hash によるローリングアップデートの世代管理が同じ役割を、中間リソースなしで果たす |
| `FleetAutoscaler` CRD | `Fleet.spec.autoscaling` | Buffer / Schedule / Webhook / Counter / Chain の全ポリシータイプに対応 |
| `GameServerAllocation` API | `AllocationService.Allocate` RPC | フィールドはほぼ 1:1(§2 参照) |
| `GameServerAllocationPolicy` CRD + allocator service | `arena-router`(`internal/router` / `cmd/arena-router`) | mTLS の代わりに IAM 認証。静的ポリシーファイルで `{region, endpoint, priority, weight}` を宣言 |
| SDK sidecar(`agones.dev/sdk`) | `arena-sidecar` | **ワイヤ互換**(§3 参照)。公式 SDK をそのまま使える |
| Counters/Lists | Counters/Lists | ほぼ同一設計。SoT がゲームプロセス自身である点も同じ |
| `kubectl apply/get/describe/delete` | `arenactl apply/get/describe/delete` | ほぼ同じ体験。CRD ではなく arena 独自の flat YAML(ECS 語彙) |
| RBAC(RoleBinding 等) | IAM ベースの authn/z | K8s RBAC の API リソース化はしていない(§7) |
| Prometheus ServiceMonitor | `arena-api` / `arena-controller -metrics-listen` の `/metrics` | 素の OpenMetrics エンドポイント。メトリクス一覧は [arena/monitoring.md](arena/monitoring.md) を参照 |

## 2. Allocation の移行

`GameServerAllocation` のリクエスト形は `AllocateRequest` にほぼそのまま持ち込めます。

| Agones `GameServerAllocation.spec` | arena `AllocateRequest` | 対応状況 |
|---|---|---|
| `selectors[]`(label/field/counter/list selector のフォールバック列) | `selectors[]`(`match_labels` / `match_fields` / `required` / `preferred`) | ✅ 先頭から順に試すフォールバック |
| `selectors[].gameServerState` (Ready/Allocated) | `allow_allocated`(bool) + `counter_filters` | ✅ Allocated を対象にする場合は高密度再割り当て(§5.3)として実装。要 `counter_filters` |
| `selectors[].counters` / `lists` フィルタ | `counter_filters[]` | ✅ |
| `priorities[]` | `priorities[]`(Counter の available capacity 昇順/降順) | ✅ |
| `metadata.labels` / `.annotations` | `game_server_metadata.labels` / `.annotations` | ✅ 割り当てと同一トランザクションで GameServer に反映 |
| Fleet 名の直接指定 | `fleet_name` | ✅(namespace + name) |
| ラベルによる複数 Fleet 横断 | `fleet_selector` | ✅ 最大 8 Fleet、60 秒キャッシュ、`fleet_name` と排他 |
| マルチクラスタ(`GameServerAllocationPolicy`) | arena-router 経由でのリクエスト転送 | ✅(§1 の対応表参照) |

Idempotency: Agones にはリクエストの冪等キーという概念自体がありませんが、
arena の `AllocateRequest.idempotency_key` は**必須**です。マッチメイカーの
リトライ処理を持ち込む際は、リトライごとに同じキーを再送するようにしてください
(同じ結果が返るだけで、二重に割り当てられることはありません)。

## 3. SDK の移行(公式クライアントはそのまま使える)

**これが最も重要な点です**: `arena-sidecar` は arena 独自の SDK に加えて、
本物の `agones.dev.sdk.SDK` サービスを `:9357`(gRPC)で、`agones.dev.sdk.beta.SDK`
(Counters/Lists)も同じポートで提供します。さらに `:9358` に手書きの REST
(HTTP+JSON)互換エンドポイントもあります。

つまり **Unity / Unreal / C# / C++ / Rust / Node など公式 Agones SDK を
コード変更なしに接続できます**。ゲームサーバー側からは Kubernetes で動く
Agones の sidecar と区別がつきません。

| 環境変数 | 意味 |
|---------|------|
| `AGONES_SDK_GRPC_PORT` | 公式 SDK が読む gRPC ポート。arena-sidecar もこれを尊重(既定 9357) |
| `AGONES_SDK_HTTP_PORT` | 公式 SDK 実装の一部が読む REST ポート。arena-sidecar もこれを尊重(既定 9358) |

移行時にゲーム側のコードを変える必要があるのは、次の 2 点だけです:

1. **`GameServer.status.address` の意味**: Agones では通常ロードバランサ/NodePort
   経由ですが、arena は**クライアントが直接 IP:Port に接続する**モデルです
   (`docs/arena/architecture.md` 参照)。ロードバランサを前提にしたコードが
   あれば取り除いてください
2. **ECS 固有の情報**: `arena.dev/gameserver-id` / `arena.dev/fleet-id` の
   annotation で、GameServer の ECS 上の素性を確認できます(Kubernetes の
   `metadata.name`/`namespace` に相当する情報が同じ形で載っています)

Counters/Lists を使っている場合は「Counter の暗黙的な作成」に注意してください —
arena は Fleet spec に Counter を事前宣言する仕組みを持たないため、初回
`UpdateCounter` 呼び出しの順序に依存する初期化ロジックがあれば見直しが必要です。

## 4. Fleet 定義の移行

Agones の Fleet CRD(YAML)と arena の `arenactl` manifest は語彙が異なります
(Kubernetes Pod template 風 vs. ECS タスク定義風)。フィールド単位の詳細は
[arenactl/manifest.md](arenactl/manifest.md) を参照してください。要点:

- ローリングアップデート戦略(`maxSurge`/`maxUnavailable`/`Recreate`/
  `drainTimeoutSeconds`)、`allocationOverflow`、autoscaler(Buffer/Schedule/
  Webhook/Counter/Chain)、複数コンテナ・volumes・secrets・command/args・
  Capacity Provider・ネットワークオーバーライドは、いずれも
  `arenactl apply`/`get` の YAML からそのまま宣言・エクスポートできます
  (詳細は [arenactl/manifest.md](arenactl/manifest.md) を参照)

## 5. 意図的に対応しない機能

以下は Agones にあって arena が意図的に持たない機能です:

- `GameServerSet` 相当の中間リソース(spec_hash 世代管理が同じ問題を解く)
- Player Tracking(alpha)— Agones 自身が Counters/Lists への移行を推奨しており、対応する SDK RPC は arena でも Counters/Lists 経由になる
- Dynamic port policy(awsvpc は Task 専有 ENI でポート衝突がないため不要)
- Feature Gates 機構、K8s RBAC の API リソース化(需要が出るまで見送り)
- Fleet に属さない単発 GameServer

## 6. 移行前のチェックリスト

- [ ] マッチメイカーの `AllocateRequest` に `idempotency_key` を必ず付与する
- [ ] クライアントの接続方式がロードバランサ前提でないか確認する(直結モデルへの対応)
- [ ] Counter/List を使うゲームは、Fleet 側の事前宣言なしに動く初期化順序になっているか確認する
- [ ] マルチリージョン運用が必要な場合は `arena-router` の設定(`{region, endpoint, priority, weight}`)を用意する
- [ ] ノード更新・Spot 中断への耐性が必要なゲームは `drainGraceSeconds` / SIGTERM ハンドリングを実装する(sidecar が `Shutdown` 相当の通知を出す)
- [ ] 公式 Agones SDK を使う場合、`AGONES_SDK_GRPC_PORT` / `AGONES_SDK_HTTP_PORT` が正しく渡っているか確認する
- [x] `api/proto/agones/**/*.proto` のフィールド番号は公式 agones.dev リポジトリ(`main` ブランチ)と突き合わせ済み(2026-07-18)。ただし実 SDK クライアントとのバイト列レベルの相互運用は実機未検証
- [ ] 実 AWS 環境での負荷試験・E2E 検証を別途実施する(本リポジトリの自動テストは DynamoDB Local / Valkey 上の testcontainers までで、実 AWS では未検証)
