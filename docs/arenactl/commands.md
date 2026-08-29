# arenactl

本ドキュメントは、Fleet manifest の適用と GameServer の運用に使用する `arenactl` command を説明する。

## ビルド

リポジトリから CLI をビルドする command を、以下に示す。

```console
$ go build -o ./bin/arenactl ./cmd/arenactl
```

以降の例は `./bin/arenactl` を `arenactl` と表記する。

## 共通設定

全 subcommand の共通 flag を、以下に示す。

| Flag | 既定値 | 用途 |
| --- | --- | --- |
| `-s`、`--server` | `$ARENA_SERVER`、未設定時 `http://localhost:8080` | `arena-api` endpoint |
| `-auth` | `$ARENA_AUTH`、未設定時 `none` | `none` または `iam` |
| `-f` | なし | Manifest file または directory。反復指定可 |

`-auth iam` は AWS SDK の標準 credential chain を使用して STS token を生成する。Server URL の host が token の `server-id` となる。

> [!IMPORTANT]
> Go の `flag` package を使用しているため、flag は `fleet NAME`、`gameserver ID`、`instance ID` などの位置引数より前に指定すること。

## apply

`apply` は manifest を読み、Fleet を作成または更新する。Server が検証、既定値の正規化、差分判定を行う。

基本的な適用 command を、以下に示す。

```console
$ arenactl apply -f fleets/ -s https://arena.example.com -auth iam
```

`-f` に directory を指定すると、`.yaml` と `.yml` を再帰的に読む。1 file に `---` 区切りで複数 Fleet を記述できる。

`-dry-run` または `--dry-run` は書き込まず、各 Fleet の予定 action を表示する。

```console
$ arenactl apply -f fleet.yaml -dry-run
```

`-prune` は、指定した manifest と同じ namespace にある `arena.dev/managed-by=arenactl` label 付き Fleet のうち、入力に存在しない Fleet を削除する。Active GameServer がある Fleet の削除は API が拒否する。

```console
$ arenactl apply -f fleets/ -prune
```

`-prune` の対象は入力 manifest に含まれる namespace だけである。Arenactl 管理 label がない Fleet は削除しない。

## diff

`diff` は `ApplyFleet` の dry-run を使用し、server-side の正規化後差分を表示する。

```console
$ arenactl diff -f fleets/
```

Exit code は、以下のとおりである。

| Code | 意味 |
| --- | --- |
| `0` | 差分なし |
| `1` | 読み込み、接続、検証などの error |
| `2` | 差分あり |

## get

`get` は現在の Fleet を再適用可能な YAML manifest として標準出力へ書き出す。

```console
$ arenactl get -n default fleet shooter-jp
```

Status、ID、version、generation は出力しない。Autoscaling が有効な Fleet では `desiredCount` を出力しない。`-o` flag は受理するが、現行実装の出力は値にかかわらず YAML である。

## delete

Manifest に含まれる Fleet を削除する command を、以下に示す。

```console
$ arenactl delete -f fleet.yaml
```

名前で Fleet を削除する command を、以下に示す。

```console
$ arenactl delete -n default fleet shooter-jp
```

Fleet に active GameServer がある場合は削除できない。希望台数を 0 にして終了を待つ必要がある。

単一 GameServer を削除する command を、以下に示す。

```console
$ arenactl delete gameserver 8c2d6a2e-1f79-4f34-86b4-e78fcd7b53a7
```

Ready、Allocated、Reserved は Draining、Scheduled、Starting は Unhealthy へ移行する。Fleet の希望台数は維持されるため、controller は代替を起動する。

## describe

Fleet の status と直近 event を表示する command を、以下に示す。

```console
$ arenactl describe -n default fleet shooter-jp
```

GameServer の状態、接続先、ECS Task、直近 event を表示する command を、以下に示す。

```console
$ arenactl describe gameserver 8c2d6a2e-1f79-4f34-86b4-e78fcd7b53a7
```

`events` DynamoDB table が必要である。認証を有効にした現行の role 定義では `ListEvents` が `admin` 専用であるため、`viewer`、`allocator`、`fleet-editor` では describe の event 取得に失敗する。

## logs

`logs` は GameServer の Task ARN から CloudWatch Logs stream prefix を組み立て、`FilterLogEvents` で表示する。

```console
$ arenactl logs -group /arena-prod/gameserver -container gameserver -since 30m -follow gameserver 8c2d6a2e-1f79-4f34-86b4-e78fcd7b53a7
```

`-group` または `ARENA_LOG_GROUP` は必須である。`-container` の既定値は `gameserver`、`-since` は `1h`、`-follow` は false である。AWS credential には CloudWatch Logs の参照権限が必要である。

## drain instance

`drain instance` は EC2 instance 上の Arena GameServer を ECS API で列挙し、GameServerService の削除操作を実行する。

```console
$ arenactl drain -cluster arena-prod -timeout 30m -force instance i-0123456789abcdef0
```

Flag を、以下にまとめる。

| Flag | 既定値 | 振る舞い |
| --- | --- | --- |
| `-cluster` | `$ARENA_CLUSTER` | 対象 ECS cluster。必須 |
| `-wait` | `true` | Allocated のセッション終了を待つ |
| `-timeout` | `0` | 待機上限。0 は context が終了するまで待つ |
| `-force` | `false` | Timeout 後の Allocated も drain する |
| `-poll-interval` | `5s` | 状態確認間隔 |

Ready、Reserved、終了処理中の GameServer は直ちに削除操作の対象となる。`-wait=true` の Allocated は Ready などへ移行するまで待ち、その後に削除する。`-wait=false` は即時に削除する。

AWS credential には `ecs:ListContainerInstances`、`ecs:ListTasks`、`ecs:DescribeTasks` が必要である。Arena API の認可では `DeleteGameServer` が `admin` 専用である。
