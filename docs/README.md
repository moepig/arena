# Arena ドキュメント

本ドキュメントは、Arena の利用、運用、開発に関する文書の索引である。

## システムの理解

Arena の役割と主要な用語は、[overview.md](overview.md) を参照。

実行コンポーネント、データフロー、状態管理は、[arena/architecture.md](arena/architecture.md) を参照。

AWS リソースとの対応と Terraform サンプルの範囲は、[arena/aws-resources.md](arena/aws-resources.md) を参照。

## 利用と運用

用途ごとの文書を、以下にまとめる。

| 文書 | 内容 |
| --- | --- |
| [arena/api.md](arena/api.md) | コントロールプレーン API、認証、エラー |
| [arena/sdk.md](arena/sdk.md) | ゲームサーバーへの SDK 組み込みと Agones 互換性 |
| [arenactl/commands.md](arenactl/commands.md) | `arenactl` のコマンド |
| [arenactl/manifest.md](arenactl/manifest.md) | Fleet manifest の書式 |
| [arena/config.md](arena/config.md) | 各バイナリの起動設定 |
| [arena/operations.md](arena/operations.md) | 起動、デプロイ、障害対応 |
| [arena/monitoring.md](arena/monitoring.md) | メトリクスとトレーシング |
| [agones-migration.md](agones-migration.md) | Agones からの移行時の対応関係 |

## 開発

開発環境とリポジトリ構成は、[development/README.md](development/README.md) を参照。

変更手順は、[development/workflow.md](development/workflow.md) を参照。

テストの分類と実行方法は、[development/testing.md](development/testing.md) を参照。
