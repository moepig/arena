# GameServer SDK

本ドキュメントは、GameServer process と `arena-sidecar` の通信、Go SDK の使用方法、Agones 互換 API を説明する。

## 接続

Sidecar は同じ ECS Task 内で 2 個の localhost endpoint を公開する。

| Endpoint | 既定値 | API |
| --- | --- | --- |
| gRPC over h2c | `localhost:9357` | Arena SDK、`agones.dev.sdk.SDK`、`agones.dev.sdk.beta.SDK` |
| HTTP | `localhost:9358` | Agones SDK REST 互換 route |

GameServer process は localhost の sidecar だけに接続する。Sidecar は SDK Gateway への 1 本の双方向 stream を維持し、状態更新、heartbeat、Allocation 通知、Counter と List を転送する。

## Arena Go SDK

公開 package は `github.com/moepig/arena/pkg/sdk` である。既定の接続先は `http://localhost:9357` であり、`ARENA_SDK_ADDRESS` で変更できる。

起動、heartbeat、Allocation 待機、再利用の基本形を、以下に示す。

```go
package main

import (
	"context"
	"log"
	"time"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/pkg/sdk"
)

func main() {
	ctx := context.Background()
	client := sdk.New()

	if err := client.Ready(ctx); err != nil {
		log.Fatal(err)
	}

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := client.Health(ctx); err != nil {
				log.Printf("health: %v", err)
			}
		}
	}()

	if err := client.WatchGameServer(ctx, func(gs *arenav1.GameServer) {
		if gs.GetState() == arenav1.GameServer_STATE_ALLOCATED {
			log.Printf("allocated: %s", gs.GetAnnotations()["session"])
		}
	}); err != nil {
		log.Fatal(err)
	}
}
```

Go client の method を、以下に示す。

| Method | 振る舞い |
| --- | --- |
| `Ready` | Starting、Allocated、Reserved から Ready へ移行する |
| `Health` | Game process の生存を sidecar へ通知する |
| `Shutdown` | GameServer を Draining へ移行する |
| `GameServer` | Sidecar が保持する現在の GameServer を取得する |
| `SetLabel` | Allocation selector から参照できる label を設定する |
| `SetAnnotation` | Annotation を設定する |
| `WatchGameServer` | 状態更新を stream で受け取る |

Arena Go SDK は `Reserve`、self `Allocate`、Counter、List を wrapper として公開していない。これらが必要な場合は生成済み Connect client または Agones 互換 API を使用する。

## Heartbeat

Sidecar は既定で 10 秒ごとに Redis へ heartbeat を送る。Game process が一度も `Health` を呼んでいない間は、sidecar が heartbeat を継続する。最初の `Health` 呼び出し後は、30 秒間 `Health` がない場合に upstream heartbeat を停止する。

Controller は Ready、Allocated、Reserved の GameServer について 30 秒ごとに heartbeat を確認し、Ready または Allocation から 60 秒間は失効判定を行わない。Sidecar の 10 秒間隔、30 秒 timeout、controller の 30 秒 sweep と 60 秒猶予は command-line で変更できない。

Fleet API の HealthSpec は初期猶予、期待間隔、失敗回数を保持するが、現行 controller は `disabled` 以外の値を失効判定に使用しない。Arenactl manifest は `disabled` を表現しない。

## 状態通知

`WatchGameServer` は Allocation による状態変化と metadata 更新を受信する。SDK Gateway との接続が切れた場合、sidecar は再接続を試行する。再接続後には DynamoDB の現在状態が送られる。

GameServer をセッション終了後も再利用する場合は `Ready` を呼ぶ。Active Allocation record は解放され、GameServer は Ready pool へ戻る。Task を終了する場合は `Shutdown` を呼ぶ。

## Reserve と self Allocation

`Reserve` は Ready GameServer を Ready pool から外し、縮小から保護する。期間 0 は `Ready`、`Allocate`、`Shutdown` のいずれかが呼ばれるまで保持する。正の期間は秒単位で指定し、期限後に Ready へ戻る。

SDK の `Allocate` は GameServer 自身を Ready または Reserved から Allocated へ移行する。この Allocation には `arena.dev/self-allocated=true` metadata が付く。

## Counter と List

Agones beta SDK の Counter と List は sidecar process 内の memory を一次状態として扱う。変更時、30 秒ごと、SDK Gateway 再接続時に全 snapshot を Redis へ同期する。

Counter は count と capacity を持ち、Allocation filter、priority、高密度 Allocation、Fleet 集計、Counter autoscaling に使用する。List は capacity と string value の集合を持つ。Sidecar の再起動では process 内の Counter と List が失われるため、ゲーム側で再設定する必要がある。

## Agones 互換 API

Sidecar は `agones.dev.sdk.SDK` の Ready、Allocate、Shutdown、Health、GetGameServer、WatchGameServer、SetLabel、SetAnnotation、Reserve を実装する。Beta service は GetCounter、UpdateCounter、GetList、UpdateList、AddListValue、RemoveListValue を実装する。

REST endpoint が提供する route を、以下に示す。

| Method と path | 操作 |
| --- | --- |
| `POST /ready` | Ready |
| `POST /allocate` | Self Allocation |
| `POST /shutdown` | Shutdown |
| `POST /health` | Health |
| `POST /reserve` | Reserve |
| `PUT /metadata/label` | Label 更新 |
| `PUT /metadata/annotation` | Annotation 更新 |
| `GET /gameserver` | 現在状態 |
| `GET /watch/gameserver` | 改行区切りの状態 stream |

互換範囲と移行時の差分は、[../agones-migration.md](../agones-migration.md) を参照。
