package secrets

import (
	"context"
	"testing"
)

func TestExtractPassthrough(t *testing.T) {
	// Trailing whitespace/newlines are trimmed; the whole rest is one value.
	pairs, err := ExtractCredential(ExtractPassthrough, []string{"NPM_TOKEN"}, []byte("tok-12345\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Name != "NPM_TOKEN" || pairs[0].Value != "tok-12345" {
		t.Errorf("passthrough = %+v", pairs)
	}
	// No trailing newline is fine too.
	pairs, err = ExtractCredential(ExtractPassthrough, []string{"X"}, []byte("abc"))
	if err != nil || len(pairs) != 1 || pairs[0].Value != "abc" {
		t.Errorf("passthrough no-newline = %+v err=%v", pairs, err)
	}
}

func TestExtractPassthroughEmpty(t *testing.T) {
	if _, err := ExtractCredential(ExtractPassthrough, []string{"X"}, []byte("  \n\t")); err == nil {
		t.Error("empty (whitespace-only) source should error")
	}
	if _, err := ExtractCredential(ExtractPassthrough, nil, []byte("x")); err == nil {
		t.Error("passthrough with no env names should error")
	}
}

var awsEnv = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}

func TestExtractAWSCredsFileWithSessionToken(t *testing.T) {
	ini := `[default]
aws_access_key_id = AKIAIOSFODNN7EXAMPLE
aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
aws_session_token = FwoGZXIvYXdzEExampleSessionToken==
`
	pairs, err := ExtractCredential(ExtractAWSCredsFile, awsEnv, []byte(ini))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 3 {
		t.Fatalf("want 3 pairs, got %+v", pairs)
	}
	want := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAIOSFODNN7EXAMPLE",
		"AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"AWS_SESSION_TOKEN":     "FwoGZXIvYXdzEExampleSessionToken==",
	}
	for _, p := range pairs {
		if want[p.Name] != p.Value {
			t.Errorf("%s = %q, want %q", p.Name, p.Value, want[p.Name])
		}
	}
	// Order must follow envNames.
	if pairs[0].Name != awsEnv[0] || pairs[1].Name != awsEnv[1] || pairs[2].Name != awsEnv[2] {
		t.Errorf("pair order = %v", pairs)
	}
}

func TestExtractAWSCredsFileWithoutSessionToken(t *testing.T) {
	ini := `[default]
aws_access_key_id=AKIA
aws_secret_access_key=SECRET
`
	pairs, err := ExtractCredential(ExtractAWSCredsFile, awsEnv, []byte(ini))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("session token absent → want 2 pairs, got %+v", pairs)
	}
	if pairs[0].Value != "AKIA" || pairs[1].Value != "SECRET" {
		t.Errorf("pairs = %+v", pairs)
	}
}

func TestExtractAWSCredsFileTolerant(t *testing.T) {
	// Comments, blank lines, extra whitespace, a non-default profile, and
	// header-less leading keys must all be handled. Only [default] keys count.
	ini := `
# my creds
;another comment

[default]
   aws_access_key_id   =   AKIA_DEFAULT
aws_secret_access_key=SECRET_DEFAULT

[profile other]
aws_access_key_id = SHOULD_BE_IGNORED
`
	pairs, err := ExtractCredential(ExtractAWSCredsFile, awsEnv, []byte(ini))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 || pairs[0].Value != "AKIA_DEFAULT" || pairs[1].Value != "SECRET_DEFAULT" {
		t.Errorf("tolerant parse = %+v", pairs)
	}
}

func TestExtractAWSCredsFileHeaderless(t *testing.T) {
	// A bare key=value file with no [default] header still resolves (pre-header
	// lines count as the default profile).
	ini := "aws_access_key_id=AK\naws_secret_access_key=SK\n"
	pairs, err := ExtractCredential(ExtractAWSCredsFile, awsEnv, []byte(ini))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 || pairs[0].Value != "AK" || pairs[1].Value != "SK" {
		t.Errorf("headerless parse = %+v", pairs)
	}
}

func TestExtractAWSCredsFileMissingKeys(t *testing.T) {
	for _, ini := range []string{
		"[default]\naws_access_key_id=AKIA\n",                             // missing secret
		"[default]\naws_secret_access_key=SK\n",                           // missing id
		"[default]\n",                                                     // both missing
		"[profile other]\naws_access_key_id=X\naws_secret_access_key=Y\n", // only a non-default profile
	} {
		if _, err := ExtractCredential(ExtractAWSCredsFile, awsEnv, []byte(ini)); err == nil {
			t.Errorf("should error on missing required keys:\n%s", ini)
		}
	}
}

func TestExtractUnknownKind(t *testing.T) {
	if _, err := ExtractCredential("bogus", []string{"X"}, []byte("x")); err == nil {
		t.Error("unknown extractor kind should error")
	}
	if _, err := ExtractCredential("", []string{"X"}, []byte("x")); err == nil {
		t.Error("empty extractor kind should error")
	}
}

// TestResolveInjectSecret exercises the broker-facing entry point end to end: a
// bare name resolves to its std: built-in, carries the right EnvNames + Extract +
// Source, and the source can be read + extracted.
func TestResolveInjectSecret(t *testing.T) {
	awsCreds := secretFile(t, "[default]\naws_access_key_id=AKIA\naws_secret_access_key=SK\n")
	uc := ucWith(map[string]Source{"aws": {Path: awsCreds}}, nil)

	r, err := ResolveInjectSecret(uc, "acme/widget", "aws") // bare name → std:aws
	if err != nil {
		t.Fatal(err)
	}
	if r.Strategy != StrategyInject || r.Extract != ExtractAWSCredsFile || len(r.EnvNames) != 3 {
		t.Fatalf("resolve = %+v", r)
	}
	if r.Source.Path != awsCreds {
		t.Errorf("source = %+v", r.Source)
	}

	// Full handler boundary: ResolveInjectSecret → ReadSource → ExtractCredential.
	src, err := ReadSource(context.Background(), r.Source)
	if err != nil {
		t.Fatal(err)
	}
	pairs, err := ExtractCredential(r.Extract, r.EnvNames, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 || pairs[0].Name != "AWS_ACCESS_KEY_ID" || pairs[0].Value != "AKIA" {
		t.Errorf("pairs = %+v", pairs)
	}
}

func TestResolveInjectSecretExplicitNamespace(t *testing.T) {
	uc := ucWith(map[string]Source{"npm": {Path: secretFile(t, "tok\n")}}, nil)
	r, err := ResolveInjectSecret(uc, "acme/widget", "std:npm")
	if err != nil {
		t.Fatal(err)
	}
	if r.Strategy != StrategyInject || r.Extract != ExtractPassthrough ||
		len(r.EnvNames) != 1 || r.EnvNames[0] != "NPM_TOKEN" {
		t.Errorf("std:npm resolve = %+v", r)
	}
}

func TestResolveInjectSecretPerRepoSource(t *testing.T) {
	// A per-repo mapping wins over the global default, just like Resolve.
	repoCreds := secretFile(t, "tok-repo\n")
	uc := ucWith(
		map[string]Source{"claude": {Path: secretFile(t, "tok-global\n")}},
		map[string]RepoEntry{"github.com/acme/widget": {Map: map[string]Source{"std:claude": {Path: repoCreds}}}},
	)
	r, err := ResolveInjectSecret(uc, "acme/widget", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if r.Source.Path != repoCreds {
		t.Errorf("per-repo source should win: %+v", r.Source)
	}
}

func TestResolveInjectSecretErrors(t *testing.T) {
	uc := ucWith(map[string]Source{"aws": {Path: "/x"}}, nil)

	// Unknown std: key.
	if _, err := ResolveInjectSecret(uc, "acme/widget", "std:bogus"); err == nil {
		t.Error("unknown std: key should error")
	}
	// A non-inject std: secret (gcp is copy) is not brokered for injection.
	if _, err := ResolveInjectSecret(uc, "acme/widget", "gcp"); err == nil {
		t.Error("non-inject secret should error")
	}
	// ssh is forward, not inject.
	if _, err := ResolveInjectSecret(uc, "acme/widget", "ssh"); err == nil {
		t.Error("forward secret should error")
	}
	// An inject secret with no mapped source.
	empty := ucWith(nil, nil)
	if _, err := ResolveInjectSecret(empty, "acme/widget", "aws"); err == nil {
		t.Error("unmapped inject secret should error")
	}
	// An explicit local: name is never brokered.
	if _, err := ResolveInjectSecret(uc, "acme/widget", "local:aws"); err == nil {
		t.Error("local: secret should error")
	}
}
