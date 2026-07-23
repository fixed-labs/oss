go_mod(name="mod")

# The shared canonical-repo test vectors — single source of truth for the
# repo-identity grammar. This one `cmd/rift/testdata/` copy is read by BOTH the
# Go fixtures test (cmd/rift/repoid_fixtures_test.go, which drives the exported
# repoid.CanonicalRepo; package-relative, wired into the go_package below so the
# pants sandbox materializes it) and the Clojure lib/ids test (via this files()
# target, materialized at its repo-relative path in the pants sandbox). The
# resolver itself moved to internal/repoid, but its fixtures test stays here so
# this remains the single cross-language vectors copy (no drift-prone second
# copy under internal/repoid/testdata).
files(
    name="repo-fixtures",
    sources=["cmd/rift/testdata/canonical-repo-vectors.json"],
)

# The `rift init` eval-spine fixtures (testdata/init-fixtures/**): the two
# skeleton fixtures' repo markers plus the existing-flake pair. The eval test
# (cmd/rift/init_eval_test.go) reads the checked-in
# existing-flake/expected-merged/flake.nix off disk, so — like repo-fixtures —
# the files() target must be wired into the go_package so the pants sandbox
# materializes them (the oss/ nix build and bare `go test` see them on disk).
files(
    name="init-fixtures",
    sources=["cmd/rift/testdata/init-fixtures/**"],
)
