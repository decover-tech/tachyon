package prebuild

import (
	"fmt"
	"net/url"
	"regexp"
)

// CloneTokenEnv holds the clone token while, and only while, the clone
// commands run. Warm steps never see it.
const CloneTokenEnv = "TACHYON_CLONE_TOKEN"

var (
	commitRe        = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	urlPathRe       = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	urlHostRe       = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?$`)
	verifyGitConfig = `if git config --local --get-regexp '(extraheader|credential|askpass)' >/dev/null || grep -qE '://[^/]*@' .git/config; then echo 'credentials found in .git/config' >&2; exit 1; fi`
)

// ValidCommit reports whether s is a full git object id (SHA-1 or SHA-256).
func ValidCommit(s string) bool { return commitRe.MatchString(s) }

// CloneCommands returns the shell commands a prebuild VM runs in its
// workspace to fetch and check out commit from cloneURL. They work both on
// an empty disk and on a fork of the previous prebuild, where only new
// objects are fetched.
//
// The token is passed to git through GIT_CONFIG_* environment variables for
// the one fetch, so it is never written to .git/config or any other file,
// and the last command fails the build if .git/config holds credentials.
func CloneCommands(cloneURL, commit string) ([]string, error) {
	u, err := url.Parse(cloneURL)
	switch {
	case err != nil:
		return nil, fmt.Errorf("clone url: %w", err)
	case u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return nil, fmt.Errorf("clone url %q: must be https with no credentials, query or fragment", cloneURL)
	case !urlHostRe.MatchString(u.Host) || !urlPathRe.MatchString(u.Path):
		return nil, fmt.Errorf("clone url %q: unexpected characters", cloneURL)
	case !ValidCommit(commit):
		return nil, fmt.Errorf("commit %q: must be a full hex object id", commit)
	}
	clean := "https://" + u.Host + u.Path
	// Scope the auth header to the clone host; credential.helper is cleared
	// so no helper can store the token.
	auth := fmt.Sprintf(`GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0= `+
		`GIT_CONFIG_KEY_1=http.https://%s/.extraheader `+
		`GIT_CONFIG_VALUE_1="AUTHORIZATION: basic $(printf 'x-access-token:%%s' "$%s" | base64 -w0)"`,
		u.Host, CloneTokenEnv)
	return []string{
		"git init -q .",
		fmt.Sprintf("git remote add origin '%s' 2>/dev/null || git remote set-url origin '%s'", clean, clean),
		fmt.Sprintf("%s git fetch -q --no-tags origin %s", auth, commit),
		fmt.Sprintf("git checkout -q --force --detach %s", commit),
		verifyGitConfig,
	}, nil
}
