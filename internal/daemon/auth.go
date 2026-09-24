package daemon

// auth: the bearer token in front of the HTTP API, and the listen address a
// daemon without a token accepts.

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"unicode"

	"freedo/internal/cli"
)

// apiTokenEnv names the environment variable that carries the API token.
const apiTokenEnv = "FREEDO_API_TOKEN"

// apiToken picks the API token: --api-token-file, then FREEDO_API_TOKEN, then
// the bootstrap api_token_file. Empty means no token is configured.
func apiToken(flagFile string, b *cli.Bootstrap, lookupEnv func(string) (string, bool)) (string, error) {
	if flagFile != "" {
		return readAPIToken(flagFile, "--api-token-file "+flagFile)
	}
	// A variable that is set and blank is a broken secret mapping, not an
	// intent to run without a token.
	if v, ok := lookupEnv(apiTokenEnv); ok {
		return checkAPIToken(strings.TrimSpace(v), apiTokenEnv)
	}
	if b.APITokenFile != "" {
		return readAPIToken(b.APITokenFile, "api_token_file "+b.APITokenFile)
	}
	return "", nil
}

func readAPIToken(path, source string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("API token: %w", err)
	}
	return checkAPIToken(strings.TrimSpace(string(data)), source)
}

func checkAPIToken(token, source string) (string, error) {
	switch {
	case token == "":
		return "", fmt.Errorf("API token from %s is empty", source)
	case strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }):
		return "", fmt.Errorf("API token from %s holds whitespace or a control character", source)
	}
	return token, nil
}

// requireToken answers 401 to a request that does not carry the bearer token.
// With healthPublic, GET /api/health stays open for a container health check.
// An empty token serves next as it is.
func requireToken(token string, healthPublic bool, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	// Comparing digests keeps the comparison constant-time in the token length.
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthPublic && r.URL.Path == "/api/health" &&
			(r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		got, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		sum := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken reads "Authorization: Bearer <token>". The scheme is
// case-insensitive (RFC 7235).
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// checkListen refuses to serve the API without a token on an address that
// other hosts reach, unless the operator asked for it with --insecure-no-auth.
func checkListen(addr string, hasToken, insecure bool) error {
	if hasToken || insecure || loopbackAddr(addr) {
		return nil
	}
	return fmt.Errorf("--addr %s is not a loopback address and no API token is configured: "+
		"set --api-token-file, %s or the bootstrap api_token_file, "+
		"or pass --insecure-no-auth to serve the API to every host without authentication",
		addr, apiTokenEnv)
}

// loopbackAddr reports whether addr binds the loopback interface alone. An
// empty host binds every interface.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
