# Arena

Arena は、専用ゲームサーバーの Fleet 管理、割り当て、オートスケール、ヘルスチェックを AWS ECS 上で実行する Go 製のコントロールプレーンである。ゲームクライアントは割り当て結果のアドレスとポートへ直接接続する。

主な機能は次のとおりである。

- Fleet の宣言的管理とローリング更新
- 冪等な GameServer 割り当て
- Buffer、Schedule、Webhook、Counter、Chain によるオートスケール
- Agones 互換 SDK sidecar
- DynamoDB を永続データ、Redis 互換ストアを派生データに用いる障害回復

## 開発環境

Go 1.26 以上が必要である。リポジトリのビルドと単体テストは、以下のコマンドで実行する。

```console
$ go build ./...
$ make test
```

Docker が利用できる環境では、実際の DynamoDB Local、Valkey、floci を用いる統合テストを実行できる。

```console
$ make test-integration
```

## ドキュメント

文書の索引と推奨する参照順序は、[docs/README.md](docs/README.md) を参照。

> [!IMPORTANT]
> `samples/terraform` は AWS リソースの構成例であり、そのままデプロイできる完成済みモジュールではない。本番導入前に必要な補完事項は、[docs/arena/aws-resources.md](docs/arena/aws-resources.md) を参照。
