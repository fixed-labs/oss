# The package test reads testdata/canonical-repo-vectors.json (the shared
# repo-identity grammar vectors, also consumed by the lib/ids Clojure test).
# `go test` and the oss nix build see it on disk; the pants sandbox only
# materializes declared deps, so the files() target must be wired here.
go_package(dependencies=["oss/cli:repo-fixtures"])

go_binary(name="bin")
