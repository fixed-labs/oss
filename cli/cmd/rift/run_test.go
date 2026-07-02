package main

import (
	"reflect"
	"testing"
)

func TestParseRunArgsDefault(t *testing.T) {
	o, err := parseRunArgs([]string{"--secret", "aws", "--", "aws", "s3", "ls"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(o.secrets, []string{"aws"}) {
		t.Fatalf("secrets = %v", o.secrets)
	}
	if !reflect.DeepEqual(o.cmd, []string{"aws", "s3", "ls"}) {
		t.Fatalf("cmd = %v", o.cmd)
	}
	if o.shell || o.materializeTo != "" {
		t.Fatalf("unexpected shell/materialize: %+v", o)
	}
}

func TestParseRunArgsRepeatableSecret(t *testing.T) {
	o, err := parseRunArgs([]string{"--secret", "aws", "--secret=npm", "--", "make"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(o.secrets, []string{"aws", "npm"}) {
		t.Fatalf("secrets = %v", o.secrets)
	}
}

func TestParseRunArgsShell(t *testing.T) {
	o, err := parseRunArgs([]string{"--shell", "--secret", "aws"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !o.shell || len(o.cmd) != 0 {
		t.Fatalf("shell parse wrong: %+v", o)
	}
}

func TestParseRunArgsMaterialize(t *testing.T) {
	o, err := parseRunArgs([]string{"--secret", "aws", "--materialize-to", "/tmp/creds"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.materializeTo != "/tmp/creds" || len(o.secrets) != 1 {
		t.Fatalf("materialize parse wrong: %+v", o)
	}
}

func TestParseRunArgsErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no secret", []string{"--", "ls"}},
		{"no command", []string{"--secret", "aws"}},
		{"shell with command", []string{"--shell", "--secret", "aws", "--", "ls"}},
		{"materialize with shell", []string{"--secret", "aws", "--shell", "--materialize-to", "/tmp/x"}},
		{"materialize with command", []string{"--secret", "aws", "--materialize-to", "/tmp/x", "--", "ls"}},
		{"materialize two secrets", []string{"--secret", "aws", "--secret", "npm", "--materialize-to", "/tmp/x"}},
		{"empty secret name", []string{"--secret", "  ", "--", "ls"}},
		{"unknown flag", []string{"--bogus", "--secret", "aws", "--", "ls"}},
		{"stray positional before --", []string{"--secret", "aws", "ls"}},
		{"dangling --secret", []string{"--secret"}},
		{"dangling --materialize-to", []string{"--secret", "aws", "--materialize-to"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRunArgs(tc.args); err == nil {
				t.Fatalf("expected error for %v", tc.args)
			}
		})
	}
}

func TestParseRunArgsEmptyTrailingCommandAfterDashDash(t *testing.T) {
	// `--secret aws --` with nothing after `--` is "no command".
	if _, err := parseRunArgs([]string{"--secret", "aws", "--"}); err == nil {
		t.Fatal("expected error for empty trailing command")
	}
}
