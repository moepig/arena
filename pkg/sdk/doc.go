// Package sdk is the public Go client SDK for game servers. It wraps the
// Agones-compatible SDK service (arena/v1/sdk.proto) served by the sidecar
// on localhost:9357: Ready, Health, Shutdown, GetGameServer, SetLabel,
// SetAnnotation and WatchGameServer.
//
// This is the only package in this repository intended for import by game
// developers; everything under internal/ is implementation detail.
package sdk
