# 開発ワークフロー

## 日常の変更ループ

```console
$ go build ./...                          # コンパイルが通るか
$ gofmt -l $(git diff --name-only -- '*.go')   # フォーマット崩れがないか(直すなら gofmt -w)
$ go vet ./...
$ go test ./...                           # 影響範囲だけなら go test ./internal/foo/... -run TestBar -v
```

`internal/*` パッケージの単体テストは Docker 不要・数秒で終わります。変更した
パッケージだけをまず回し、最後に `make test` で全体を確認してください。

## `.proto` を変更する手順

1. `api/proto/**/*.proto` を編集する
   - `arena/v1`、`arena/gateway/v1` が arena 独自の proto。`agones/dev/sdk` 以下は
     Agones 公式 proto のワイヤ互換ベンダリングなので、フィールド番号を変える
     ような変更は基本的にしない(するなら [agones-migration.md](../agones-migration.md) の
     検証手順に従って公式リポジトリと再度突き合わせる)
   - 既存フィールドの**番号を変更・再利用しない**(後方互換を壊す)。追加は
     常に新しい番号で
2. `make gen` を実行する(`buf lint` → `buf generate`)
   - 初回、`$GOBIN` に `buf`/`protoc-gen-go`/`protoc-gen-connect-go`/
     `protoc-gen-connect-openapi` が無ければ `go install ... @latest` が走る
     (**ネットワークアクセスが必要**)。オフライン環境では事前に用意しておく
   - `gen/` 以下(Go 生成コード + `gen/openapi/arena.v1.yaml`)が更新される。
     **`gen/` は直接編集しない** — 次の `make gen` で上書きされる
3. `gen/` の diff を確認する(git 管理下であれば `git diff gen/`)。CI 相当の
   鮮度チェックは `make gen-check`(`make gen` の後に `git diff --exit-code gen/`)
4. 新しいフィールド/RPC を実装に反映する。典型的な変更範囲は下記
   「機能を追加する」を参照

## 機能を追加する(典型的な変更範囲)

新しい Fleet spec フィールドや RPC を追加するとき、触ることが多いレイヤーを
上から順に並べたものです(全部を毎回触るわけではありません — 該当する層だけ):

1. **`api/proto/arena/v1/*.proto`** — メッセージ/フィールド/RPC を追加し `make gen`
2. **`internal/convert/`** — store レコード(DynamoDB 形)⇔ proto メッセージの
   相互変換に新フィールドを追加(`SpecFromStore`/`SpecToStore` 等)
3. **`internal/store/`** — DynamoDB に永続化する値なら `types.go` の struct に
   フィールド追加(`dynamodbav` タグ)。GSI やテーブル定義が要るなら `schema.go`
4. **`internal/pool/`** — Redis に置く派生データ(高頻度更新・再構築可能なもの)
   ならここに追加。DynamoDB と Redis のどちらに置くかは
   [arena/architecture.md](../arena/architecture.md) の原則(SoT は DynamoDB、
   Redis は再構築可能な派生データのみ)に従って判断する
5. **`internal/api/`** — RPC ハンドラでのバリデーション・変換呼び出し
6. **`internal/controller/`** / **`internal/allocation/`** / **`internal/gateway/`** /
   **`internal/sidecar/`** — reconcile ロジック・割り当てロジック・SDK 実装など、
   機能の実体
7. **`internal/manifest/`** — `arenactl apply`/`get` の YAML から宣言できるように
   するなら、ここに Decode/Encode の対応を追加(**Encode 側で必ずラウンドトリップ
   させる** — `arenactl get` の出力がそのまま `apply` に通らないと使い物にならない)
8. **テスト** — 変更したパッケージにユニットテストを追加。DynamoDB/Redis/ECS の
   実際の挙動(条件付き書き込みの原子性、GSI クエリ等)に依存する部分は
   `test/integration/` に統合テストを追加(詳細は [testing.md](testing.md))
9. **`docs/`** — ユーザー影響がある変更なら該当ドキュメントを更新
   (`docs/arena/api.md`、`docs/arenactl/manifest.md` 等)。Agones との対応表に
   関わるなら [agones-migration.md](../agones-migration.md) も

この順番は「疎結合な層から実装する」というより「コンパイルが通る順番」に近い
— proto → convert → store → 上位ロジック、の順で積み上げると、各段階で
`go build ./...` が通る状態を保ちやすいです。

## ローカルでエンドツーエンドに動かす

`compose.yaml` が AWS 相当のローカル環境(DynamoDB Local / Valkey / floci)を
起動します。テーブル作成や env の詳細は
[arena/operations.md#ローカル実行](../arena/operations.md#ローカル実行) を参照してください。

```console
$ make compose-up

$ AWS_ENDPOINT_URL_DYNAMODB=http://localhost:18000 \
  AWS_ENDPOINT_URL_ECS=http://localhost:14566 \
  AWS_ENDPOINT_URL_SQS=http://localhost:14566 \
  AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1 \
  go run ./cmd/arena-api -redis localhost:16379 -table-prefix arena-local-

# 別ターミナルで controller も同様に起動可能(-cluster 等は operations.md 参照)

$ arenactl apply -f fleets/example.yaml -s http://localhost:8080
$ arenactl get fleet example -s http://localhost:8080

$ make compose-down   # 停止・状態破棄
```

`internal/*` の単体テストは compose 環境を使いません(すべてフェイク実装か
miniredis)。`make test-integration` も compose とは独立に
testcontainers が自分専用のコンテナを起動・破棄するので、compose を
上げっぱなしにする必要はありません。

## コミット前チェックリスト

- [ ] `gofmt -l` に何も出ない(`gofmt -w` で直す)
- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `go test ./...`(変更に関係するパッケージは `-race` も)
- [ ] `.proto` を変更したなら `make gen` 後の `gen/` を含めてコミット
- [ ] DynamoDB/Redis/ECS の実挙動に依存する変更なら `make test-integration` も緑
- [ ] ユーザー影響のある変更は `docs/` を更新した
