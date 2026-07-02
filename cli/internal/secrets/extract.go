package secrets

import (
	"fmt"
	"strings"
)

// EnvPair is a single named environment value an inject secret resolves to. The
// broker (`devbox run`, owned by another component) sets these on the child
// process's environment; the value never touches the box's disk or the agent's
// context.
type EnvPair struct {
	Name  string
	Value string
}

// ExtractCredential maps an inject secret's opaque source bytes (read via
// ReadSource) onto its named env values, per the entry's Extract kind. It is the
// seam the broker handler calls after resolving a secret and reading its source:
//
//	r, _ := ResolveInjectSecret(uc, repoDir, "aws")
//	src, _ := ReadSource(ctx, r.Source)
//	pairs, _ := ExtractCredential(r.Extract, r.EnvNames, src)
//
// envNames is the entry's Env slice (order-significant for aws-creds-file). The
// returned pairs preserve that order and never include an empty value.
func ExtractCredential(kind Extract, envNames []string, src []byte) ([]EnvPair, error) {
	switch kind {
	case ExtractPassthrough:
		if len(envNames) == 0 {
			return nil, fmt.Errorf("passthrough extractor needs one env name")
		}
		v := strings.TrimRight(string(src), " \t\r\n")
		if v == "" {
			return nil, fmt.Errorf("passthrough source is empty")
		}
		return []EnvPair{{Name: envNames[0], Value: v}}, nil
	case ExtractAWSCredsFile:
		return extractAWSCredsFile(envNames, src)
	case "":
		return nil, fmt.Errorf("no extractor set for this secret")
	default:
		return nil, fmt.Errorf("unknown extractor %q", kind)
	}
}

// extractAWSCredsFile parses the standard AWS shared-credentials INI and maps the
// [default] profile's keys onto the three conventional env vars (in the order
// envNames declares them: access key id, secret access key, session token). The
// session token is optional. Tolerant of surrounding whitespace, blank lines, and
// `#`/`;` comments; values are taken verbatim (after trimming) — AWS credential
// values are not quoted.
func extractAWSCredsFile(envNames []string, src []byte) ([]EnvPair, error) {
	if len(envNames) < 2 {
		return nil, fmt.Errorf("aws-creds-file extractor needs at least 2 env names (access key id, secret access key)")
	}
	const (
		keyAccessKeyID     = "aws_access_key_id"
		keySecretAccessKey = "aws_secret_access_key"
		keySessionToken    = "aws_session_token"
	)
	vals, err := parseINIDefault(src)
	if err != nil {
		return nil, err
	}
	id := vals[keyAccessKeyID]
	secret := vals[keySecretAccessKey]
	if id == "" || secret == "" {
		var missing []string
		if id == "" {
			missing = append(missing, keyAccessKeyID)
		}
		if secret == "" {
			missing = append(missing, keySecretAccessKey)
		}
		return nil, fmt.Errorf("aws credentials missing required key(s): %s", strings.Join(missing, ", "))
	}
	pairs := []EnvPair{
		{Name: envNames[0], Value: id},
		{Name: envNames[1], Value: secret},
	}
	// Session token is optional, and only emitted if a third env name is declared.
	if tok := vals[keySessionToken]; tok != "" && len(envNames) >= 3 {
		pairs = append(pairs, EnvPair{Name: envNames[2], Value: tok})
	}
	return pairs, nil
}

// parseINIDefault returns the key→value pairs of the [default] section of an AWS
// shared-credentials INI (keys lowercased; values trimmed). Lines before any
// section header are treated as belonging to [default] too, tolerating a
// credentials file written with no explicit profile header. Comment lines
// (`#`/`;`) and blanks are skipped. Keys outside [default] are ignored.
func parseINIDefault(src []byte) (map[string]string, error) {
	out := map[string]string{}
	inDefault := true // pre-header lines count as the default profile
	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			// AWS profile sections may be `[default]` or `[profile foo]`-style; we
			// only need the default profile.
			inDefault = section == "default"
			continue
		}
		if !inDefault {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue // not a key=value line; ignore
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			continue
		}
		out[key] = val
	}
	return out, nil
}
