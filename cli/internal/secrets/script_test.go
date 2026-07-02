package secrets

import (
	"strings"
	"testing"
)

func TestValidateDest(t *testing.T) {
	ok := map[string]string{
		"~/.aws/credentials": ".aws/credentials",
		"~/app/.env":         "app/.env",
		"~/x":                "x",
	}
	for in, want := range ok {
		got, err := validateDest(in)
		if err != nil || got != want {
			t.Errorf("validateDest(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
	bad := []string{"/etc/passwd", "~/../x", "~/a/../b", "~/a//b", "~/", "~", "relative/path", "~/a b/c", "~/a;rm -rf/b"}
	for _, in := range bad {
		if _, err := validateDest(in); err == nil {
			t.Errorf("validateDest(%q) should fail", in)
		}
	}
}

func TestStoreName(t *testing.T) {
	cases := map[Key]string{
		{NS: NSStd, Name: "aws"}:     "std-aws",
		{NS: NSLocal, Name: "env"}:   "local-env",
		{NS: NSOrg, Name: "datadog"}: "org-datadog",
	}
	for k, want := range cases {
		if got := storeName(k); got != want {
			t.Errorf("storeName(%v) = %q, want %q", k, got, want)
		}
	}
}

func TestRepoDirName(t *testing.T) {
	for in, want := range map[string]string{
		"acme/widget":            "widget",
		"github.com/acme/widget": "widget",
	} {
		got, err := repoDirName(in)
		if err != nil || got != want {
			t.Errorf("repoDirName(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
}

func TestPushScriptTmpfsVsPersistent(t *testing.T) {
	tmpfs := pushScript("app/.env", "0600", true, "local-env")
	if !strings.Contains(tmpfs, secretsStore) || !strings.Contains(tmpfs, "ln -sfn") {
		t.Errorf("tmpfs push script missing store/symlink:\n%s", tmpfs)
	}
	persist := pushScript("app/.env", "0600", false, "local-env")
	if strings.Contains(persist, "ln -sfn") {
		t.Errorf("persistent push script should not symlink into the store:\n%s", persist)
	}
	if !strings.Contains(persist, `dest="$HOME/app/.env"`) {
		t.Errorf("persistent push script missing dest:\n%s", persist)
	}
}

func TestParseKey(t *testing.T) {
	good := map[string]Key{
		"std:aws":   {NS: NSStd, Name: "aws"},
		"local:env": {NS: NSLocal, Name: "env"},
		"env":       {NS: NSLocal, Name: "env"}, // default ns
		"org:dd":    {NS: NSOrg, Name: "dd"},
	}
	for in, want := range good {
		got, err := ParseKey(in)
		if err != nil || got != want {
			t.Errorf("ParseKey(%q) = (%v,%v), want %v", in, got, err, want)
		}
	}
	bad := []string{"bogus:aws", "std:", "std:-aws", "std:AWS", "std:a/b", ":aws"}
	for _, in := range bad {
		if _, err := ParseKey(in); err == nil {
			t.Errorf("ParseKey(%q) should fail", in)
		}
	}
}
