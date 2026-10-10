# Forgejo plugin protocol

`plugin.proto`, `plugin.pb.go` and `plugin_grpc.pb.go` are vendored from
Forgejo Runner's `act/plugin/proto/v1alpha/`. Their MIT license is preserved
in [LICENSE](LICENSE). The generated headers identify protoc-gen-go 1.36.12
and protoc-gen-go-grpc 1.6.2.

The original schema copy came from an unversioned source snapshot; its exact
upstream revision was not recorded. Integration compatibility was tested with
Runner v13.1.0, commit `6095cb17bfdbded5aa4ea84c18a4b69fd9574cca`.
That test revision does not establish the origin of the vendored files.

The `go_package` option retains Forgejo's original module path. SpawnPod code
imports the vendored package through its local module path.
