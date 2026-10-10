# Pinned Forgejo plugin protocol

These three files are copied from the local Forgejo Runner source tree at
`/home/dmedovich/runner-main/runner/act/plugin/proto/v1alpha/` on 2026-10-08:
`plugin.proto`, `plugin.pb.go`, and `plugin_grpc.pb.go`. Their headers identify
the protocol and generated code as MIT licensed; see [LICENSE](LICENSE).

The local tree has no Git metadata, so its exact revision is unknown. The
generated files identify protoc-gen-go 1.36.12 and protoc-gen-go-grpc 1.6.2.
Before claiming compatibility with Runner v13.1.0, compare these files against
the release tag and record the checksums and exact upstream revision.

The `go_package` option retains Forgejo's original module path for wire
identity. Go code in spawnPod imports this local package path instead.
