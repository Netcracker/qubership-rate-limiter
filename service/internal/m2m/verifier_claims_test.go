package m2m

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	qubetoken "github.com/netcracker/qubership-core-lib-go/v3/security/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A token is refused by its time claims with the leeway of 30 seconds the
// platform's verifier allows: one issued in the future, one without an issue
// time, and one that is not valid until past the leeway. Each comes back with
// the error the management API maps onto its 401 detail.
func TestNewVerifier_refusesATokenOnItsTimeClaims(t *testing.T) {
	c := newCluster(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := NewVerifier(ctx, Config{Audience: audience, Token: c.podToken})
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		override jwt.MapClaims
		want     error
	}{
		{"issued an hour ahead", jwt.MapClaims{"iat": time.Now().Add(time.Hour).Unix()},
			jwt.ErrTokenUsedBeforeIssued},
		{"no issue time", jwt.MapClaims{"iat": nil}, qubetoken.ErrTokenClaimMissing},
		{"not valid until 35 seconds ahead, past the leeway",
			jwt.MapClaims{"nbf": time.Now().Add(35 * time.Second).Unix()}, jwt.ErrTokenNotValidYet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifier.Verify(ctx, c.token(t, c.key, "cluster", tc.override))
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

// A token that is not valid until 25 seconds ahead is inside the leeway, and
// accepted. TestNewVerifier_refusesATokenOnItsTimeClaims holds the one
// past it.
func TestNewVerifier_acceptsATokenNotValidYetInsideTheLeeway(t *testing.T) {
	c := newCluster(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := NewVerifier(ctx, Config{Audience: audience, Token: c.podToken})
	require.NoError(t, err)

	_, err = verifier.Verify(ctx, c.token(t, c.key, "cluster", jwt.MapClaims{
		"nbf": time.Now().Add(25 * time.Second).Unix(),
	}))

	assert.NoError(t, err, "Verify of a token not valid until 25 seconds ahead")
}

// A token whose header names none is refused by its algorithm, before a key is
// looked up, although it carries the claims of an accepted token under the
// cluster's key id.
func TestNewVerifier_refusesATokenSignedWithNone(t *testing.T) {
	c := newCluster(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := NewVerifier(ctx, Config{Audience: audience, Token: c.podToken})
	require.NoError(t, err)
	// The payload of an accepted token, under a header that names none, and
	// without the signature.
	_, signed, _ := strings.Cut(c.token(t, c.key, "cluster", nil), ".")
	payload, _, _ := strings.Cut(signed, ".")
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"cluster","typ":"JWT"}`)) +
		"." + payload + "."

	_, err = verifier.Verify(ctx, unsigned)

	assert.ErrorIs(t, err, jwt.ErrTokenSignatureInvalid)
}

// A verified token keeps its numeric claims as json.Number, and its time
// claims are read from that form. The claims are decoded before the signature
// is checked, so the caller chooses every literal the decode converts.
func TestNewVerifier_decodesTheClaimsNumbersAsJSONNumbers(t *testing.T) {
	c := newCluster(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := NewVerifier(ctx, Config{Audience: audience, Token: c.podToken})
	require.NoError(t, err)

	token, err := verifier.Verify(ctx, c.token(t, c.key, "cluster", jwt.MapClaims{"n": json.Number("1.5")}))

	require.NoError(t, err, "Verify of a token with a numeric claim")
	claims, ok := token.Claims.(jwt.MapClaims)
	require.True(t, ok, "the claims of a verified token are %T", token.Claims)
	assert.IsType(t, json.Number(""), claims["n"], `claim "n"`)
	assert.IsType(t, json.Number(""), claims["exp"], `claim "exp"`)
}

// A token's header is bounded to the compact one an external signer may
// write: at most maxHeaderSegment bytes encoded, and no field but alg, kid,
// and typ, none of them a number. A token the cluster signs under a kid of
// 1024 bytes, which its key set publishes, is verified. A longer header, one
// with another field, and one whose kid is a number are refused as malformed
// before golang-jwt decodes them, although the cluster signed them: golang-jwt
// converts the header's number literals to float64 whatever its options, and
// the header is the caller's choice until the signature is checked.
func TestNewVerifier_boundsTheHeaderBeforeDecodingIt(t *testing.T) {
	c := newCluster(t)
	longest := strings.Repeat("k", 1024)
	c.alias.Store(longest)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := NewVerifier(ctx, Config{Audience: audience, Token: c.podToken})
	require.NoError(t, err)
	sign := func(header map[string]any) string {
		return c.signedWithHeader(t, jwt.SigningMethodRS256, c.key, header, nil)
	}

	atBound := sign(map[string]any{"kid": longest})
	header, _, _ := strings.Cut(atBound, ".")
	require.Len(t, header, maxHeaderSegment, "the header segment under a kid of 1024 bytes")
	_, err = verifier.Verify(ctx, atBound)
	require.NoError(t, err, "Verify of a token signed under a kid of 1024 bytes")

	for _, refused := range []struct {
		name   string
		header map[string]any
	}{
		{"a kid of 1025 bytes, past maxHeaderSegment", map[string]any{"kid": longest + "k"}},
		{"a field besides alg, kid, and typ", map[string]any{"kid": "cluster", "x": "y"}},
		{"a kid that is a number", map[string]any{"kid": json.Number("0.247032822920623272088284e-300")}},
	} {
		t.Run(refused.name, func(t *testing.T) {
			_, err := verifier.Verify(ctx, sign(refused.header))
			assert.ErrorIs(t, err, jwt.ErrTokenMalformed)
		})
	}
}
