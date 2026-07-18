# 開発者ガイド

arena 自体のコードに手を入れる開発者向けのドキュメントです。arena を
デプロイ・運用する人は [arena/operations.md](../arena/operations.md)、
ゲームサーバーに SDK を組み込む人は [arena/sdk.md](../arena/sdk.md) を
参照してください。

- [workflow.md](workflow.md) — proto 変更・ローカル実行・機能追加の手順、コミット前チェックリスト
- [testing.md](testing.md) — テスト戦略(単体/統合)、実行方法、既知の注意点

## 前提ツール

| ツール | バージョン目安 | 用途 |
|-------|--------------|------|
| Go | 1.26+(`go.mod` の `go` ディレクティブが正) | ビルド・テスト全般 |
| Docker | 動作するもの(daemon 起動済み) | `make test-integration`(testcontainers)、`make compose-up` |
| buf / protoc-gen-go / protoc-gen-connect-go / protoc-gen-connect-openapi | 固定なし(`@latest`) | `.proto` を変更する時のみ必要。`make tools` が `$GOBIN` に未インストールなら自動 `go install`(**ネットワークアクセスが要る**) |

`.proto` に触らない変更(Go コードのみ)であれば、Docker と buf 系ツールは
`make test-integration` を回すとき以外は不要です。

## リポジトリ構成

| パス | 内容 |
|------|------|
| `api/proto/` | protobuf 定義(`arena/v1`、`arena/gateway/v1`、ベンダリングした `agones/dev/sdk`) |
| `gen/` | `buf generate` の生成物(Go + connect-go + OpenAPI)。**手で編集しない**、`make gen` で再生成 |
| `internal/store/` | DynamoDB 永続化層(唯一の Source of Truth)。状態機械・条件付き書き込み |
| `internal/pool/` | Redis 派生データ層(Ready プール、ハートビート、pub/sub、Counter 補助 ZSET) |
| `internal/convert/` | store レコード ⇔ proto メッセージの相互変換 |
| `internal/allocation/` | 割り当てホットパス(ロックフリー fast path + セレクタ slow path) |
| `internal/controller/` | Fleet/Health/Autoscale reconciler、リーダー選出・シャーディング、SQS イベントコンシューマ |
| `internal/api/` | `arena-api` の RPC ハンドラ(バリデーション含む) |
| `internal/gateway/` | SDK Gateway(sidecar との双方向ストリーム終端) |
| `internal/sidecar/` | sidecar 本体(Agones 互換 SDK サーバー、Counters/Lists) |
| `internal/ecs/` | ECS API ラッパー(Task 起動・停止・sidecar 認証) |
| `internal/manifest/` | `arenactl` の YAML ⇔ proto 変換 |
| `internal/router/` | マルチリージョン allocation router |
| `internal/auth/` | IAM ベース認証・認可 |
| `internal/telemetry/` | EMF メトリクス・Prometheus エクスポート・OTel トレーシング |
| `cmd/arena-api/`、`cmd/arena-controller/`、`cmd/arena-sidecar/`、`cmd/arena-router/`、`cmd/arenactl/` | 各バイナリの `main` パッケージ(薄い配線のみ、ロジックは `internal/`) |
| `pkg/sdk/` | ゲーム開発者向け公開 Go クライアント SDK |
| `test/integration/` | 実バックエンド(testcontainers)を使う統合テスト。build tag `integration` |
| `deploy/terraform/` | AWS インフラの IaC |
| `deploy/grafana/` | ダッシュボード定義 |
| `docs/` | 本ドキュメント一式。索引は [overview.md](../overview.md) |

## 5 分クイックスタート

```console
$ go build ./...
$ make test                # 単体テスト(Docker 不要)
$ make test-integration    # 統合テスト(Docker が要る。初回はイメージ pull が走る)
```

ローカルで実際に arena-api / arena-controller を動かして触ってみたい場合は
[workflow.md#ローカルでエンドツーエンドに動かす](workflow.md#ローカルでエンドツーエンドに動かす)
を参照してください。

## アーキテクチャを理解する

コードを読む前に、以下の設計ドキュメントに目を通すと理解が早いです。

- [arena/architecture.md](../arena/architecture.md) — 設計原則・データモデル・状態機械・障害耐性
- [agones-migration.md](../agones-migration.md) — Agones との機能ギャップ・既知の差分
