package outbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

func TestCanonicalizeJSON(t *testing.T) {
	for name, test := range map[string]struct {
		input string
		want  string
	}{
		"member order":          {`{"b":1,"a":2}`, `{"a":2,"b":1}`},
		"nested order":          {`{"z":{"y":1,"x":2},"a":[{"b":1,"a":2}]}`, `{"a":[{"a":2,"b":1}],"z":{"x":2,"y":1}}`},
		"whitespace":            {"{\n  \"a\" : 1 ,\t\"b\": [ 1, 2 ]\n}", `{"a":1,"b":[1,2]}`},
		"integer":               {`{"v":700000000}`, `{"v":700000000}`},
		"large exact integer":   {`{"v":9007199254740992}`, `{"v":9007199254740992}`},
		"negative integer":      {`{"v":-1550250000}`, `{"v":-1550250000}`},
		"decimal":               {`{"v":15.50}`, `{"v":15.5}`},
		"small decimal":         {`{"v":0.00001}`, `{"v":0.00001}`},
		"tiny":                  {`{"v":0.0000001}`, `{"v":1e-7}`},
		"huge":                  {`{"v":1e21}`, `{"v":1e+21}`},
		"below huge":            {`{"v":100000000000000000000}`, `{"v":100000000000000000000}`},
		"exponent padding":      {`{"v":1e+09}`, `{"v":1000000000}`},
		"negative exponent":     {`{"v":1.5e-9}`, `{"v":1.5e-9}`},
		"zero":                  {`{"v":0.0}`, `{"v":0}`},
		"string escapes":        {`{"v":"<b>&\n"}`, `{"v":"<b>&\n"}`},
		"unicode literal":       {`{"v":"₦aira"}`, `{"v":"₦aira"}`},
		"control char":          {`{"v":"a\u0001b"}`, `{"v":"a\u0001b"}`},
		"rfc8785 array vector":  {`[56,{"1":[]},925,{"b":-4.5},{"d":true},null,"x"]`, `[56,{"1":[]},925,{"b":-4.5},{"d":true},null,"x"]`},
		"boolean and null":      {`{"t":true,"f":false,"n":null}`, `{"f":false,"n":null,"t":true}`},
		"empty object in array": {`{"a":{},"b":[]}`, `{"a":{},"b":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			canonical, err := CanonicalizeJSON([]byte(test.input))
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if string(canonical) != test.want {
				t.Fatalf("canonical = %s, want %s", canonical, test.want)
			}
		})
	}
}

func TestCanonicalizeJSONFailsClosed(t *testing.T) {
	for _, input := range []string{`{broken`, `[1,2] extra`, ``, `"unterminated`} {
		if _, err := CanonicalizeJSON([]byte(input)); err == nil {
			t.Fatalf("input %q canonicalized", input)
		}
	}
}

func TestSignVerifyEnvelopeRoundTrip(t *testing.T) {
	signer := testSigner(t)
	envelope, err := BuildEnvelope(sampleEvent())
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	signed, err := signer.SignEnvelope(envelope)
	if err != nil {
		t.Fatalf("sign envelope: %v", err)
	}
	parts := strings.Split(signed.Provenance.Signature, ".")
	if len(parts) != 3 {
		t.Fatalf("signature is not JWS compact: %q", signed.Provenance.Signature)
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("header decode: %v", err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatalf("header JSON: %v", err)
	}
	if header["alg"] != "EdDSA" || header["kid"] != "financial-controls-2026-08" {
		t.Fatalf("protected header = %v", header)
	}
	if err := VerifyEnvelope(signer.Public(), signer.KeyID(), signed); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// The canonical signing input excludes the signature field, so signing
	// twice is deterministic.
	again, err := signer.SignEnvelope(envelope)
	if err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	if again.Provenance.Signature != signed.Provenance.Signature {
		t.Fatal("signature is not deterministic")
	}
	// Serialization round-trip (as a consumer would receive it) still verifies.
	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var received Envelope
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := VerifyEnvelope(signer.Public(), signer.KeyID(), received); err != nil {
		t.Fatalf("verify after serialization: %v", err)
	}
}

func TestEnvelopeTamperDetection(t *testing.T) {
	signer := testSigner(t)
	envelope, err := BuildEnvelope(sampleEvent())
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	signed, err := signer.SignEnvelope(envelope)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tamper := func(mutate func(*Envelope)) Envelope {
		candidate := signed
		mutate(&candidate)
		return candidate
	}
	cases := map[string]Envelope{
		"payload mutated":     tamper(func(e *Envelope) { e.FHIR.Entry[0].Resource.(map[string]any)["maker"] = "kc-attacker" }),
		"classification drop": tamper(func(e *Envelope) { e.Classification = "PUBLIC" }),
		"event id mutated":    tamper(func(e *Envelope) { e.EventID = "forged" }),
		"principal mutated":   tamper(func(e *Envelope) { e.Provenance.PrincipalID = "kc-attacker" }),
		"commit hash mutated": tamper(func(e *Envelope) { e.Provenance.LedgerCommitHash = "forged" }),
	}
	for name, candidate := range cases {
		if err := VerifyEnvelope(signer.Public(), signer.KeyID(), candidate); !errors.Is(err, ErrSignatureInvalid) {
			t.Fatalf("%s: tampered envelope verified (%v)", name, err)
		}
	}
	// Wrong key, wrong kid and missing signature all fail closed.
	otherKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnvelope(otherKey, signer.KeyID(), signed); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("wrong key verified: %v", err)
	}
	if err := VerifyEnvelope(signer.Public(), "financial-controls-1999-01", signed); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("wrong kid verified: %v", err)
	}
	unsigned := signed
	unsigned.Provenance.Signature = ""
	if err := VerifyEnvelope(signer.Public(), signer.KeyID(), unsigned); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("unsigned envelope verified: %v", err)
	}
	forged := signed
	forged.Provenance.Signature = "a.b.c"
	if err := VerifyEnvelope(signer.Public(), signer.KeyID(), forged); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("malformed JWS verified: %v", err)
	}
}

func TestSignerFromEnvFailClosed(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBlock := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	base64Key := base64.StdEncoding.EncodeToString(privateKey)
	base64Seed := base64.RawURLEncoding.EncodeToString(privateKey.Seed())
	for name, material := range map[string]string{"pem": pemBlock, "base64 key": base64Key, "base64url seed": base64Seed} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{EnvSigningPrivateKey: material, EnvSigningKeyEpoch: "2026-08"}
			signer, err := NewEnvelopeSignerFromEnv(func(key string) string { return env[key] })
			if err != nil {
				t.Fatalf("signer from env: %v", err)
			}
			if signer.KeyID() != "financial-controls-2026-08" {
				t.Fatalf("kid = %s", signer.KeyID())
			}
			if !signer.Public().Equal(publicKey) {
				t.Fatal("public key mismatch")
			}
		})
	}
	// Every gap refuses startup.
	for name, env := range map[string]map[string]string{
		"missing key":        {EnvSigningKeyEpoch: "2026-08"},
		"missing epoch":      {EnvSigningPrivateKey: base64Key},
		"garbage key":        {EnvSigningPrivateKey: "!!!not-a-key!!!", EnvSigningKeyEpoch: "2026-08"},
		"short key":          {EnvSigningPrivateKey: base64.StdEncoding.EncodeToString([]byte("short")), EnvSigningKeyEpoch: "2026-08"},
		"rsa pem not ed25519": {EnvSigningPrivateKey: "-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----", EnvSigningKeyEpoch: "2026-08"},
		"blank epoch":        {EnvSigningPrivateKey: base64Key, EnvSigningKeyEpoch: "  "},
		"hostile epoch":      {EnvSigningPrivateKey: base64Key, EnvSigningKeyEpoch: `2026" ,"alg":"none`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewEnvelopeSignerFromEnv(func(key string) string { return env[key] }); !errors.Is(err, ErrSigningKeyInvalid) {
				t.Fatalf("accepted invalid env: %v", err)
			}
		})
	}
	if _, err := NewEnvelopeSignerFromEnv(nil); !errors.Is(err, ErrSigningKeyInvalid) {
		t.Fatalf("nil lookup accepted: %v", err)
	}
	if _, err := NewEnvelopeSigner(nil, "2026-08"); !errors.Is(err, ErrSigningKeyInvalid) {
		t.Fatalf("nil key accepted: %v", err)
	}
	if _, err := NewEnvelopeSigner(privateKey, ""); !errors.Is(err, ErrSigningKeyInvalid) {
		t.Fatalf("empty epoch accepted: %v", err)
	}
}
