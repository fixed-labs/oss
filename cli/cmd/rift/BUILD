# The package test reads testdata/canonical-repo-vectors.json (the shared
# repo-identity grammar vectors, also consumed by the lib/ids Clojure test) and
# the init-fixtures/** (the `rift init` eval-spine fixtures). `go test` and the
# oss nix build see them on disk; the pants sandbox only materializes declared
# deps, so both files() targets must be wired here.
#
# init.go `//go:embed playbooks/init.md` (the `rift init` playbook) is a
# COMPILE-TIME embed: bare `go test` and the oss nix build (`src = ./cli`) see
# the file on disk, but pants' Go backend resolves embed patterns against
# declared `resource` deps, so the playbook must be wired as a resource here or
# the compile fails "could not embed playbooks/init.md: no matching files found".
resource(name="init-playbook", source="playbooks/init.md")

go_package(
    dependencies=[
        "oss/cli:repo-fixtures",
        "oss/cli:init-fixtures",
        ":init-playbook",
    ]
)

go_binary(name="bin")
