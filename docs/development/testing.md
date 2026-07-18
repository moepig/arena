# テスト戦略

## 単体テスト vs 統合テスト

| | 単体テスト(`make test`) | 統合テスト(`make test-integration`) |
|---|---|---|
| 対象 | `internal/*`、`cmd/arenactl`、`pkg/sdk` | `test/integration/`(build tag `integration`) |
| バックエンド | フェイク実装(`fake*` struct)/ miniredis | testcontainers が起動する実 DynamoDB Local / Valkey / floci |
| 実行速度 | 数秒 | 数十秒(コンテナ起動込み) |
| Docker | 不要 | 必要(daemon 起動済みであること) |
| コマンド | `go test ./...` | `go test -tags integration -count=1 -race -timeout 10m ./test/integration/...` |
| いつ書く | ロジックそのものの正しさ(状態遷移、フィルタ、計算式など) | DynamoDB の条件付き書き込み/トランザクションの原子性、GSI クエリ、Redis の実際のコマンド挙動、複数プロセス間の競合(リーダー選出・シャーディング)など、フェイクでは検証できない実バックエンド固有の振る舞い |

```console
$ make test                                        # 単体テスト一式
$ go test ./internal/allocation/... -run TestFoo -v # 特定パッケージ/テストだけ
$ make test-integration                             # 統合テスト一式(-race 込み)
```

## フェイクの流儀

`internal/controller`、`internal/allocation`、`internal/gateway`、
`internal/api` などは、それぞれのテストファイルで `fakeCtrlStore` /
`fakeStore` / `fakePool` のような in-memory フェイクを定義し、本物の
`Store`/`Pool` インタフェースを実装しています。フェイクは:

- 実際の DynamoDB と同じ条件(state 遷移の許可・version 一致)をメモリ上の
  map で模倣する(`store.ErrConditionFailed` 等、本物と同じエラーを返す)
- 複数テストファイルで共有されることが多い(例:
  `internal/controller/controller_test.go` の `fakeCtrlStore` は
  `reconcile_v3_test.go`、`autoscale_v3_test.go` 等からも使われる)
- 別 goroutine から並行アクセスされる可能性がある箇所(例: shard 関連の
  テストが `leadShard` を直接 goroutine で回す)では、フェイク自身に
  `sync.Mutex` を足して race を防ぐ(`fakeLauncher.launchedCount()` のように、
  安全な読み出し専用メソッドを用意するパターンを使う)

新しいフェイクを書くときは、本物の実装がどのエラー(`store.ErrConditionFailed`、
`store.ErrVersionConflict` 等)をどの条件で返すかを踏襲してください。フェイクが
本物より緩い条件を許してしまうと、フェイク上のテストは通るのに実際の
DynamoDB では失敗する、という乖離が起きます(この手のギャップは
`test/integration/` 側のテストで最終的に捕まりますが、単体テストの段階で
気づけた方が速いです)。

## 統合テストがカバーする範囲

`test/integration/` は主に以下を検証します(詳細は各テストのコメント参照):

- **DynamoDB**: 状態遷移の条件付き書き込みが並行レースで単一勝者になること、
  割り当てトランザクション(`ClaimGameServer`/`AddAllocation`)の原子性、
  楽観ロック(`UpdateFleet` の version 競合)、リーダーリースの排他性
- **Redis(Valkey)**: 冪等キー付き並行 `Allocate` の収束、Unhealthy との
  競合時の挙動、epoch によるプール再構築
- **ECS(floci)**: 実際に Docker コンテナとして Task が起動すること、
  `startedBy` を使った sidecar なりすまし検証
- **controller のフルライフサイクル e2e**: scale-up → RUNNING イベント →
  Ready → Allocate → STOPPED イベント → Terminated → 補充、を一通り
  (`TestControllerLoopEndToEnd`)
- **fleet シャーディング**: 複数 `Controller` プロセスが実 DynamoDB のリースを
  取り合い、shard を分担すること(`TestFleetShardingSplitsAcrossTwoControllers`)

新しい統合テストを書くときは `test/integration/harness_test.go` の
`newStore(t)` / `newPool(t)` / `waitFor(t, what, timeout, cond)` を使うと、
testcontainers のセットアップやポーリングを自前で書かずに済みます。

## `-race` と既知の注意点

- `make test-integration` は常に `-race` 付きです。バックグラウンド
  goroutine から共有状態(フェイクの slice/map、テスト側のアサーション対象)に
  触るテストを書くときは、必ず mutex で保護するかチャネル経由で同期してください
  (「フェイクの流儀」参照)
- `TestLeaderLease`(`test/integration/store_test.go`)はリース失効の実時間
  待ち(`time.Sleep`)を含むため、フルスイートを `-race` 付きで回すと
  まれにタイミングでフレーキーになることがあります。単体で再実行すれば
  通ります — arena 側の変更が原因かどうか疑わしい場合は、まず単独実行
  (`go test -tags integration -run TestLeaderLease ./test/integration/...`)
  で切り分けてください

## testcontainers のデバッグ

`test/integration` は testcontainers が DynamoDB Local / Valkey / floci を
自動起動・破棄します。失敗時にコンテナの中身を見たい場合:

```console
$ docker ps -a                      # テスト終了直後ならまだ残っていることがある
$ docker logs <container-id>
```

testcontainers はテスト成功時にコンテナを破棄するため、失敗を再現しつつ
中身を見たい場合はテスト関数に一時的に `t.Skip()`/`select{}` を挟むか、
該当コンテナのイメージ(`amazon/dynamodb-local`、`valkey/valkey`、
`floci/floci`)を `docker run` で直接立ち上げて操作する方が手早いです。

## `make gen-check`(proto 生成物の鮮度チェック)

`.proto` を変更した PR では、`gen/` が最新かどうかを CI 相当でローカル確認
できます:

```console
$ make gen-check   # make gen した上で git diff --exit-code gen/
```

git 管理下にないディレクトリで実行すると `git diff` が失敗するため、リポジトリを
`git init` している(または実際の clone である)ことが前提です。
