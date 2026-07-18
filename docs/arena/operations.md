# 運用ガイド

## ローカル実行

`compose.yaml` が AWS 相当のローカル環境を起動します
(対応表は [aws-resources.md](aws-resources.md#ローカルでの代替)):

| サービス | イメージ | ホストポート |
|---------|---------|-------------|
| DynamoDB | amazon/dynamodb-local | 18000 |
| Redis 互換 | valkey/valkey | 16379 |
| SQS / ECS / STS / EC2 | floci/floci(ECS は実 Docker 実行) | 14566 |

```console
$ make compose-up

# テーブル作成(store.EnsureTables を呼ぶ任意の方法で。統合テストは自動作成する)

$ AWS_ENDPOINT_URL_DYNAMODB=http://localhost:18000 \
  AWS_ENDPOINT_URL_ECS=http://localhost:14566 \
  AWS_ENDPOINT_URL_SQS=http://localhost:14566 \
  AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1 \
  go run ./cmd/arena-api -redis localhost:16379 -table-prefix arena-local-

$ make compose-down   # 停止・状態破棄
```

## バイナリと主なフラグ

### arena-api

| フラグ | 既定値 | 説明 |
|-------|-------|------|
| `-listen` | `:8080` | listen アドレス(ALB の背後、h2c) |
| `-redis` | `localhost:6379` | Redis アドレス |
| `-table-prefix` | `arena-` | DynamoDB テーブル名プレフィックス |
| `-cluster` | (空) | sidecar 検証用 ECS クラスタ。**空だと sidecar 検証なし(dev のみ)** |
| `-authz-file` | (空) | 認可バインディング YAML。**空だと API 認証なし(dev のみ)** |
| `-server-id` | (空) | トークン束縛用の公開ホスト名(`-authz-file` 使用時必須) |
| `-otlp-endpoint` | (空) | OTLP/gRPC collector(例 `localhost:4317`)。空でトレース無効 |

### arena-controller

| フラグ | 既定値 | 説明 |
|-------|-------|------|
| `-redis` / `-table-prefix` | 同上 | |
| `-queue-url` | (空) | EventBridge→SQS のキュー URL。空だと level トリガのみで動作 |
| `-cluster` | `arena` | GameServer Task を起動する ECS クラスタ |
| `-subnets` / `-security-groups` | (空) | GameServer Task のネットワーク(カンマ区切り) |
| `-assign-public-ip` | `true` | クライアント直結用パブリック IP |
| `-execution-role-arn` / `-task-role-arn` | (空) | GameServer Task のロール |
| `-sidecar-image` | (空) | 注入する arena-sidecar イメージ |
| `-gateway-endpoint` | (空) | sidecar に渡す arena-api の URL |
| `-run-tasks-per-second` | `5` | RunTask レート制限(トークンバケット) |

### arenactl / arena-sidecar

[arenactl/commands.md](../arenactl/commands.md) / [sdk.md](sdk.md#sidecar-の設定) を参照。

## デプロイ

- **arena-api**: rolling update(minimumHealthyPercent=100)。sidecar ストリームは
  再接続で無停止。graceful shutdown は受付停止 → in-flight 完了待ち(30 秒)
- **arena-controller**: rolling update。シャットダウン時に**リースを明示解放**するため
  スタンバイが即昇格する。reconcile は冪等なので重複・空白期間とも安全
- **arena-sidecar**: Fleet の template 更新(= spec_hash 変化)として
  ローリングアップデート(maxSurge/maxUnavailable、または Recreate)に乗る。
  `rollingUpdate.drainTimeoutSeconds` を設定すると、旧世代の Allocated server を
  期限で強制的に Draining へ移す

## メトリクス

CloudWatch EMF / Prometheus 双方で配信されるメトリクスの一覧・命名規則・推奨
アラームは [monitoring.md](monitoring.md) を参照してください。

## トレーシング

`-otlp-endpoint` を指定すると RPC 単位のスパンが OTLP/gRPC で出力されます
(ADOT Collector sidecar → X-Ray 等)。未指定なら完全に無効です。

## Runbook

### Allocation が遅い / 失敗する

1. `Arena/Allocation PoolMiss` を確認 → 在庫不足なら autoscaler の buffer を増やす
2. `ABORTED`(claim 競合)率を確認 → `HeartbeatTimeouts` 多発と相関していないか
3. SQS 滞留(EventLag)→ controller の健全性・リーダー有無を確認

### GameServer が Ready にならない

1. STOPPED イベントの stoppedReason(ECS console / gameservers レコード)
2. sidecar ログ: SDK Gateway への接続可否(SG / arena-api の健全性)
3. ゲームサーバーが `Ready()` を呼んでいるか(SDK 統合ミス)

### コントローラが動いていない

1. `leases` テーブルの holder / expires_at を確認
2. スタンバイが昇格しない場合は controller サービスの Desired Count とヘルスを確認

### Redis 障害後に割り当てが回復しない

1. controller ログの "pool rebuild" を確認(回復検知 → 20 秒待ち → epoch bump → 再構築)
2. 手動で再構築が必要な場合は controller の再起動でも同じ経路を通る
3. プールは派生データなので**データロスはない**。Ready 在庫は再構築で復元される

### EC2 ノードの計画停止(AMI 更新・ASG instance refresh)

`arenactl drain instance` はセッションを強制的に打ち切らず、Allocated な
GameServer はセッションの自然終了を待ってからドレインします
(`-timeout`/`-force` で上限を設定可能。[arenactl/commands.md](../arenactl/commands.md#drain-instance) 参照)。
手動実行:

```console
$ arenactl drain instance i-0123456789abcdef0 -cluster arena-prod -timeout 30m -force
```

ASG の instance refresh / termination lifecycle hook と自動連携させる場合の
レシピ(具体的な Lambda 実装はデプロイ環境依存のため同梱していません):

1. ASG に `autoscaling:EC2_INSTANCE_TERMINATING` のライフサイクルフックを設定し、
   ハートビートタイムアウトをゲームの最大セッション長 + マージンに設定する
2. フックのターゲット(Lambda 等)で `arenactl drain instance <instance-id>
   -cluster <cluster> -wait -timeout <ハートビート残り時間> -force` を実行する
   (`-force` はハートビート期限に必ず収める安全弁。ハートビートを延長できる
   構成なら `-force` なしでセッション終了を無期限に待ってもよい)
3. 完了後 `aws autoscaling complete-lifecycle-action` を呼んでインスタンスの
   終了を進める(タイムアウトすると ASG が既定アクションにフォールバックする
   ため、フック側にも必ずタイムアウトを設定する)

## テスト

```console
$ make test              # 単体テスト(docker 不要)
$ make test-integration  # 統合テスト: testcontainers が DynamoDB Local /
                         # Valkey / floci を起動して破棄する(-race 付き)
$ make gen-check         # proto 生成物の鮮度チェック(CI 用)
$ make tf-validate       # Terraform validate
```

統合テスト(`test/integration`、build tag `integration`)がカバーする範囲:

- 実 DynamoDB 条件付き書き込み: 状態遷移の単一勝者、割り当てトランザクションの原子性、
  楽観ロック、リーダーリース
- 実 Redis wire: 冪等キー並行 Allocate の収束、Unhealthy 競合、epoch プール再構築
- 実 ECS API(floci): RunTask / startedBy / sidecar 検証
- controller のフルライフサイクル e2e(scale-up → RUNNING → Ready → Allocate →
  STOPPED → Terminated → 補充)

## バックアップ

- DynamoDB: PITR 有効化 + 日次 On-Demand Backup
- Redis: スナップショット**不要**(全データが DynamoDB から再構築可能)
