go_mod(name="mod")

# The shared canonical-repo test vectors — single source of truth for the
# repo-identity grammar, consumed by BOTH the Go CLI test (package-relative
# `testdata/canonical-repo-vectors.json`, so the oss/ subtree and its nix
# build stay self-contained) and the Clojure lib/ids test (via this files()
# target, materialized at its repo-relative path in the pants test sandbox).
files(
    name="repo-fixtures",
    sources=["cmd/rift/testdata/canonical-repo-vectors.json"],
)
