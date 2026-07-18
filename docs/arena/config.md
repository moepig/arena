# アプリケーション設定

各バイナリのコマンドラインフラグ・環境変数を一覧します。バイナリの起動手順や
デプロイ時の代表値は [operations.md](operations.md) を、Fleet manifest(YAML)の
フィールドは [arenactl/manifest.md](../arenactl/manifest.md) を参照してください。

## 設定の基本方針

- 設定はすべて起動時のコマンドラインフラグで与える。一部のフラグは環境変数を既定値として
  読む(例: `-config`(既定値 `$ARENA_ROUTER_CONFIG`))。フラグを指定すればそちらが優先し、
  未指定のときだけ環境変数にフォールバックする
- 設定ファイルを使うのは `-authz-file`(認可バインディング YAML、[api.md](api.md#認可ロールマッピングrbac-lite)参照)と
  arena-router の `-config`(リージョンポリシー JSON、[後述](#リージョンポリシーファイル-arena-router))の 2 つだけ
- AWS の認証情報・リージョン・(ローカル実行時の)スタブエンドポイントはバイナリ固有の
  フラグではなく、`AWS_REGION` / `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` /
  `AWS_ENDPOINT_URL_*` など AWS SDK 標準の環境変数で制御する(`aws-sdk-go-v2` の
  `config.LoadDefaultConfig` が解決する既定チェーン)。ローカル実行の具体例は
  [operations.md](operations.md#ローカル実行)を参照

## arena-api

Fleet CRUD・GameServer 参照・Allocation・SDK Gateway を提供するステートレスな
control-plane API サーバー。

| フラグ | 既定値 | 用途 / 振る舞い |
|-------|-------|----------------|
| `-listen` | `:8080` | listen アドレス。ALB の背後で h2c(平文 HTTP/2)として待ち受ける |
| `-redis` | `localhost:6379` | Ready Pool・ハートビートなど派生データを置く Redis(互換)のアドレス |
| `-table-prefix` | `arena-` | DynamoDB テーブル名のプレフィックス。同一アカウント内で環境を分離する用途 |
| `-cluster` | (空) | sidecar のなりすまし検証に使う ECS クラスタ名。**空だと検証をスキップし全セッションを受理する(開発専用)** |
| `-authz-file` | (空) | 認可バインディング YAML のパス。**空だと API 認証そのものが無効(開発専用)** |
| `-server-id` | (空) | presigned STS トークンの署名束縛先となる、この API の公開ホスト名。`-authz-file` 指定時は必須 |
| `-otlp-endpoint` | (空) | OTLP/gRPC トレースコレクタのエンドポイント(例 `localhost:4317`)。**空だとトレーシングは完全に無効** |

`/metrics` は `-listen` と同じアドレス上で常時公開される(OpenMetrics 形式。専用フラグはない)。

## arena-controller

Fleet reconciler・Health sweep・Autoscale・EventBridge→SQS イベントコンシューマ・
プール再構築を実行するリーダー選出型のコントローラ。

| フラグ | 既定値 | 用途 / 振る舞い |
|-------|-------|----------------|
| `-redis` | `localhost:6379` | arena-api と同じ Redis(互換)アドレス |
| `-table-prefix` | `arena-` | arena-api と同じ DynamoDB テーブルプレフィックス |
| `-shard-count` | `1` | fleet reconcile を分割するシャード数。`1` は単一リーダーのまま、`2` 以上にすると fleet をシャード数ぶんに分割し、各シャードを独立にリースした複数の controller プロセスが並列に reconcile する |
| `-queue-url` | (空) | ECS タスク状態変化イベントを受け取る SQS キューの URL。**空だとイベントコンシューマを起動せず、level トリガ(resync)のみで動作する** |
| `-metrics-listen` | `:9090` | OpenMetrics `/metrics` の listen アドレス。**空だと metrics サーバー自体を起動しない** |
| `-cluster` | `arena` | GameServer Task を起動する ECS クラスタ |
| `-subnets` | (空) | GameServer Task に割り当てるサブネット ID(カンマ区切り) |
| `-security-groups` | (空) | GameServer Task に割り当てるセキュリティグループ ID(カンマ区切り) |
| `-assign-public-ip` | `true` | GameServer Task にパブリック IP を割り当てるか。arena はゲームトラフィックにロードバランサを使わずクライアントが IP:Port に直結するため、既定で有効 |
| `-execution-role-arn` | (空) | GameServer Task の ECS execution role |
| `-task-role-arn` | (空) | GameServer Task の task role(CloudWatch Logs 書き込みのみに使用) |
| `-sidecar-image` | (空) | Task Definition に自動注入する arena-sidecar のコンテナイメージ |
| `-gateway-endpoint` | (空) | sidecar に渡す arena-api の SDK Gateway URL |
| `-log-group` | (空) | GameServer Task の CloudWatch Logs ロググループ |
| `-run-tasks-per-second` | `5` | ECS `RunTask` 呼び出しのレート制限(トークンバケット)。ECS API のスロットリングを避けるための上限 |

## arena-router

複数リージョンの arena-api へ `AllocationService` を転送するステートレスな
ルーター。arena-router 自身は状態を持たない。

| フラグ | 既定値 | 用途 / 振る舞い |
|-------|-------|----------------|
| `-listen` | `:8090` | listen アドレス |
| `-config` | `$ARENA_ROUTER_CONFIG` | リージョンポリシー JSON ファイルのパス。**必須**(未指定かつ環境変数も空だと起動時にエラー) |

### リージョンポリシーファイル(arena-router)

`-config` が指すのは、リージョンごとの転送先を並べた JSON 配列。

```json
[
  { "name": "us-east-1", "endpoint": "https://arena-us-east-1.example.internal", "priority": 0, "weight": 3 },
  { "name": "us-west-2", "endpoint": "https://arena-us-west-2.example.internal", "priority": 0, "weight": 1 },
  { "name": "ap-northeast-1", "endpoint": "https://arena-ap-northeast-1.example.internal", "priority": 1, "weight": 1 }
]
```

| フィールド | 説明 |
|-----------|------|
| `name` | ログ・戻り値で使うリージョン名(必須) |
| `endpoint` | 転送先 arena-api の URL(必須。`https://` 以外は h2c 平文として扱う) |
| `priority` | 優先度グループ。**値が小さいグループから順に試す**。あるグループ内の全リージョンが `RESOURCE_EXHAUSTED`(Allocate)/`NOT_FOUND`(Release / GetAllocation)を返して初めて次のグループへフォールバックする |
| `weight` | 同一 `priority` グループ内での重み付きランダム順。大きいほど先に・高頻度で選ばれる。`0` 以下でも選ばれなくなることはない(常に極小の確率を残す) |

## arena-sidecar

GameServer Task に同居し、Agones 互換 SDK を localhost で提供する sidecar。
通常は controller が Task Definition に自動注入するため、下記フラグは手動実行時のみ意識する。

| フラグ | 環境変数(フラグ未指定時の既定値) | 用途 / 振る舞い |
|-------|--------------------------------|----------------|
| `-listen` | `AGONES_SDK_GRPC_PORT`(既定 `9357`) | ローカル SDK の listen アドレス(gRPC)。arena / agones.dev 両方の SDK サービスをこのポートで提供する |
| `-listen-http` | `AGONES_SDK_HTTP_PORT`(既定 `9358`) | ローカル Agones 互換 REST の listen アドレス |
| `-gateway` | `ARENA_GATEWAY_ENDPOINT` | arena-api の SDK Gateway エンドポイント。**必須**(どちらも空だと起動時にエラー) |
| `-gameserver-id` | `ARENA_GAMESERVER_ID` | この sidecar が代表する GameServer の ID。**必須**。通常は controller が Task 起動時に注入する |

`AGONES_SDK_GRPC_PORT` / `AGONES_SDK_HTTP_PORT` は公式 Agones SDK も読む変数のため、
Agones との互換性のために同じ名前をそのまま採用している。

そのほか `ECS_CONTAINER_METADATA_URI_V4`(ECS が Task に自動注入する環境変数)を読み、
Task ARN を自己検出して gateway でのなりすまし検証に使う。ユーザーが設定する項目ではなく、
未設定(= ECS 外での実行)の場合は Task ARN なしで動作する。

## arenactl

Fleet manifest を扱う宣言的管理 CLI。すべてのサブコマンドが以下の共通フラグを持つ。

| フラグ | 既定値 | 用途 / 振る舞い |
|-------|-------|----------------|
| `-s`, `--server` | `$ARENA_SERVER`(未設定なら `http://localhost:8080`) | 接続先 arena-api のエンドポイント |
| `-auth` | `$ARENA_AUTH`(未設定なら `none`) | 認証方式。`iam`(標準 AWS クレデンシャルチェーンから presigned STS トークンを生成)または `none` |
| `-f` | (なし・繰り返し指定可) | manifest ファイルまたはディレクトリ(`apply` / `diff` / `delete`) |

サブコマンド固有のフラグ:

| コマンド | フラグ | 既定値 | 用途 / 振る舞い |
|---------|-------|-------|----------------|
| `apply` | `--dry-run` | `false` | 書き込みを行わず検証・diff のみ表示 |
| `apply` | `--prune` | `false` | 渡した manifest に存在しない、arenactl 管理下(`arena.dev/managed-by` ラベル付き)の fleet を削除する。稼働中の GameServer を持つ fleet は API 側で拒否されるため先に scale 0 が必要 |
| `get` | `-n` | `default` | 対象 namespace |
| `get` | `-o` | `yaml` | 出力フォーマット(現状 `yaml` のみ) |
| `delete` | `-n` | `default` | `delete fleet NAME` 時の namespace |
| `describe` | `-n` | `default` | `describe fleet NAME` 時の namespace |
| `logs` | `-group` | `$ARENA_LOG_GROUP` | CloudWatch Logs のロググループ名。**必須** |
| `logs` | `-container` | `gameserver` | ログを取得する Task 内コンテナ名 |
| `logs` | `-follow` | `false` | 新規ログイベントをポーリングし続ける |
| `logs` | `-since` | `1h` | 何時間分さかのぼって表示するか |
| `drain instance` | `-cluster` | `$ARENA_CLUSTER` | 対象 ECS クラスタ。**必須** |
| `drain instance` | `-wait` | `true` | Allocated な GameServer をセッションの自然終了まで待ってからドレインする。`false` なら即座に強制ドレイン |
| `drain instance` | `-timeout` | `0`(無期限) | Allocated な GameServer を待つ上限時間 |
| `drain instance` | `-force` | `false` | `-timeout` を過ぎても Allocated のままの GameServer を、エラーにする代わりに強制ドレインする |
| `drain instance` | `-poll-interval` | `5s` | 待機中に Allocated な GameServer の状態を再確認する間隔 |

## Go SDK(pkg/sdk)

ゲームサーバープロセスに組み込む Go クライアント。

| 環境変数 | 既定値 | 用途 / 振る舞い |
|---------|-------|----------------|
| `ARENA_SDK_ADDRESS` | `http://localhost:9357` | 接続先 sidecar のアドレス。ローカル開発やテストで sidecar 以外(モック等)に向けたいときに上書きする |

明示的なアドレスを使いたい場合は環境変数の代わりに `sdk.NewForAddress(addr)` を呼ぶ。

## 認可バインディング YAML(arena-api `-authz-file`)

IAM プリンシパル ARN を arena 内ロールへマッピングするファイル。書式・ロール一覧・
リロード間隔(1 分)は [api.md](api.md#認可ロールマッピングrbac-lite) を参照。
