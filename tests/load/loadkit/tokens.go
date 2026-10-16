package main

import (
	"bufio"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// tokensCommand writes one bearer token per test user of a generator's slice
// as CSV with the header user,hot,token, the file the JMeter plan reads.
//
// The users of a run are <prefix>1 to <prefix>N, zero-padded to the width of
// N. Generator i of n takes every user whose 1-based index minus one is i
// modulo n, so the slices of all generators partition the users. The first
// -hot users of the run are hot users, and the rows of a slice list them
// first: the plan reads the leading rows marked hot as its hot users.
//
// With -source self-signed the tokens are JWTs signed with a key made for
// this call, for a gateway that does not check signatures. With -source
// keycloak each user logs in to the realm with the password grant; -watch
// then keeps the command running and renews every token once 70 percent of
// its lifetime has passed, replacing the file by a rename each time.
func tokensCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("tokens", flag.ContinueOnError)
	source := fs.String("source", "keycloak", "where the tokens come from: keycloak or self-signed")
	users := fs.Int("users", 2000, "number of test users in the whole run")
	hot := fs.Int("hot", 0, "number of hot users, counted from the first user")
	generator := fs.Int("generator", 0, "0-based index of this generator")
	generators := fs.Int("generators", 1, "number of generators that share the users")
	prefix := fs.String("prefix", "svt-user-", "prefix of the user names")
	out := fs.String("out", "tokens.csv", "file to write, or - for standard output")
	issuer := fs.String("issuer", "ratelimit-loadtest", "self-signed: iss claim")
	audience := fs.String("audience", "ratelimit-loadtest", "self-signed: aud claim")
	ttl := fs.Duration("ttl", 72*time.Hour, "self-signed: lifetime of each token, counted from now")
	var kc keycloakConfig
	fs.StringVar(&kc.baseURL, "keycloak-url", "", "keycloak: server root, the part of the URL before /realms")
	fs.StringVar(&kc.realm, "realm", "", "keycloak: realm of the test users")
	fs.StringVar(&kc.caFile, "ca-file", "",
		"keycloak: PEM file with the CA certificates to trust instead of the system ones")
	fs.BoolVar(&kc.insecure, "insecure", false, "keycloak: skip the server certificate check")
	clientID := fs.String("client-id", "", "keycloak: client with Direct access grants enabled")
	clientSecretFile := fs.String("client-secret-file", "",
		"keycloak: file with the client secret; empty for a public client")
	passwordFile := fs.String("password-file", "", "keycloak: file with the password every test user has")
	concurrency := fs.Int("concurrency", 16, "keycloak: token requests in flight")
	watch := fs.Bool("watch", false, "keycloak: keep running and renew the tokens before they expire")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case *users < 1:
		return fmt.Errorf("-users is %d; want at least 1", *users)
	case *hot < 0 || *hot > *users:
		return fmt.Errorf("-hot is %d; want 0 to %d", *hot, *users)
	case *generators < 1 || *generator < 0 || *generator >= *generators:
		return fmt.Errorf("-generator %d of -generators %d is out of range", *generator, *generators)
	}

	var rows []tokenRow
	for _, u := range userSlice(*users, *generator, *generators) {
		rows = append(rows, tokenRow{user: userName(*prefix, u, *users), hot: u <= *hot})
	}

	switch *source {
	case "self-signed":
		if *watch {
			return errors.New("-watch applies to -source keycloak only")
		}
		if *ttl <= 0 {
			return fmt.Errorf("-ttl is %v; want a positive duration", *ttl)
		}
		if err := signRows(rows, *issuer, *audience, *ttl); err != nil {
			return err
		}
		return writeTokens(*out, rows, stdout)
	case "keycloak":
		if kc.baseURL == "" || kc.realm == "" || *clientID == "" || *passwordFile == "" {
			return errors.New("-source keycloak needs -keycloak-url, -realm, -client-id, and -password-file")
		}
		password, err := readSecretFile(*passwordFile)
		if err != nil {
			return err
		}
		var secret string
		if *clientSecretFile != "" {
			if secret, err = readSecretFile(*clientSecretFile); err != nil {
				return err
			}
		}
		client, err := kc.httpClient()
		if err != nil {
			return err
		}
		kl := keycloakLogin{endpoint: kc.tokenURL(kc.realm), client: client, clientID: *clientID,
			clientSecret: secret, password: password}
		indexes := make([]int, len(rows))
		for i := range rows {
			indexes[i] = i
		}
		failures, first := forEach(indexes, *concurrency, func(i int) error { return kl.login(&rows[i]) })
		if failures > 0 {
			return fmt.Errorf("%d of %d users could not log in; first error: %w", failures, len(rows), first)
		}
		if err := writeTokens(*out, rows, stdout); err != nil {
			return err
		}
		if !*watch {
			return nil
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		issued := make([]time.Time, len(rows))
		for i := range rows {
			issued[i] = rows[i].grant.expires.Add(-time.Duration(rows[i].grant.ExpiresIn) * time.Second)
		}
		r := &renewal{login: kl, rows: rows, issued: issued, out: *out, concurrency: *concurrency, stdout: stdout}
		return r.loop(ctx)
	default:
		return fmt.Errorf("-source is %q; want keycloak or self-signed", *source)
	}
}

// tokenRow is one user of the slice and the token it sends.
type tokenRow struct {
	user  string
	hot   bool
	token string
	grant grant // keycloak only
}

// userSlice returns the 1-based indexes of the users of generator i of n in
// ascending order, which lists the hot users, the lowest indexes, first.
func userSlice(users, i, n int) []int {
	var slice []int
	for u := 1 + i; u <= users; u += n {
		slice = append(slice, u)
	}
	return slice
}

func userName(prefix string, u, users int) string {
	return prefix + fmt.Sprintf("%0*d", len(strconv.Itoa(users)), u)
}

// writeTokens writes rows to path through a temporary file and a rename, so
// a reader never sees a partial file; path "-" writes to stdout instead.
func writeTokens(path string, rows []tokenRow, stdout io.Writer) error {
	write := func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		_, _ = fmt.Fprintln(bw, "user,hot,token")
		for _, r := range rows {
			hot := 0
			if r.hot {
				hot = 1
			}
			_, _ = fmt.Fprintf(bw, "%s,%d,%s\n", r.user, hot, r.token)
		}
		return bw.Flush()
	}
	if path == "-" {
		return write(stdout)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tokens-*.csv")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s wrote %d tokens to %s\n", time.Now().UTC().Format(time.RFC3339), len(rows), path)
	return nil
}

// signRows gives every row a JWT whose sub is the user name, signed with
// PS256 (RSASSA-PSS with SHA-256) by a key made for this call.
func signRows(rows []tokenRow, issuer, audience string, ttl time.Duration) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"PS256","typ":"JWT"}`))
	for i := range rows {
		payload, err := json.Marshal(map[string]any{
			"iss": issuer, "sub": rows[i].user, "aud": audience,
			// A minute back, so a gateway whose clock runs behind still
			// accepts a token signed a moment ago.
			"iat": now.Add(-time.Minute).Unix(),
			"exp": now.Add(ttl).Unix(),
		})
		if err != nil {
			return err
		}
		input := header + "." + base64.RawURLEncoding.EncodeToString(payload)
		digest := sha256.Sum256([]byte(input))
		sig, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, digest[:],
			&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		if err != nil {
			return err
		}
		rows[i].token = input + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	return nil
}

// keycloakLogin obtains and renews the tokens of the test users.
type keycloakLogin struct {
	endpoint     string
	client       *http.Client
	clientID     string
	clientSecret string
	password     string
}

// login gives r a token from the password grant.
func (k keycloakLogin) login(r *tokenRow) error {
	g, err := passwordGrant(k.client, k.endpoint, k.clientID, k.clientSecret, r.user, k.password)
	if err != nil {
		return fmt.Errorf("user %s: %w", r.user, err)
	}
	r.grant, r.token = g, g.AccessToken
	return nil
}

// renew gives r a new token: from its refresh token while that stays valid
// for longer than renewMargin, otherwise, or when Keycloak refuses the
// refresh token, from a new login.
func (k keycloakLogin) renew(r *tokenRow, now time.Time) error {
	if r.grant.RefreshToken != "" && r.grant.refreshExpires.After(now.Add(renewMargin)) {
		g, err := refreshGrant(k.client, k.endpoint, k.clientID, k.clientSecret, r.grant)
		if err == nil {
			r.grant, r.token = g, g.AccessToken
			return nil
		}
	}
	return k.login(r)
}

// renewMargin is how long a refresh token has to stay valid to be used.
const renewMargin = 10 * time.Second

// renewAt returns the moment a token issued for lifetime until expires is
// due for renewal: once 70 percent of the lifetime has passed.
func renewAt(issued, expires time.Time) time.Time {
	return issued.Add(time.Duration(float64(expires.Sub(issued)) * 0.7))
}

// renewal keeps the tokens of a tokens file fresh.
type renewal struct {
	login       keycloakLogin
	rows        []tokenRow
	issued      []time.Time // when each row's token was issued
	out         string
	concurrency int
	stdout      io.Writer
}

// loop calls renewDue every five seconds until ctx is done.
func (r *renewal) loop(ctx context.Context) error {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-tick.C:
			if err := r.renewDue(now); err != nil {
				return err
			}
		}
	}
}

// renewDue renews every token due at now and rewrites the file when it
// renewed one. A token that fails to renew stays in the file and is due
// again on the next call, so a short outage of Keycloak costs nothing while
// the old tokens stay valid.
func (r *renewal) renewDue(now time.Time) error {
	var due []int
	for i := range r.rows {
		if !now.Before(renewAt(r.issued[i], r.rows[i].grant.expires)) {
			due = append(due, i)
		}
	}
	if len(due) == 0 {
		return nil
	}
	failures, first := forEach(due, r.concurrency, func(i int) error {
		if err := r.login.renew(&r.rows[i], now); err != nil {
			return err
		}
		r.issued[i] = now
		return nil
	})
	if failures > 0 {
		_, _ = fmt.Fprintf(r.stdout, "%s %d of %d due tokens failed to renew; first error: %v\n",
			now.UTC().Format(time.RFC3339), failures, len(due), first)
	}
	if failures == len(due) {
		return nil
	}
	return writeTokens(r.out, r.rows, r.stdout)
}
