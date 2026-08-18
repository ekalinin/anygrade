package sshsig

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The fixtures below were produced by a real OpenSSH 9.9 client:
//
//	printf '%s' 'ag-nonce-abc123' | ssh-keygen -Y sign -f <key> -n anygrade -
//
// They are the interop contract: the verifier here has to keep accepting what
// students' own ssh-keygen emits, for every key type and both hash algorithms
// OpenSSH will pick, without this package ever running that binary.
const (
	goldenMessage   = "ag-nonce-abc123"
	goldenNamespace = "anygrade"

	ed25519Pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBgQn1PZiSDDU3tKm1Y2s6DIVptcctoSUwoMl4O/8R1K test"
	ed25519Sig = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgGBCfU9mJIMNTe0qbVjazoMhWm1
xy2hJTCgyXg7/xHUoAAAAIYW55Z3JhZGUAAAAAAAAABnNoYTUxMgAAAFMAAAALc3NoLWVk
MjU1MTkAAABA9qzubIt27q3odn3bgQ/wJMGS5AylfKpeuSZsnzFYt9o2g+9+aTYntm/ebM
ZbwYoQQ4rnGq1Wf6PYwnq2YtIRAw==
-----END SSH SIGNATURE-----
`
	// Same key and message, signed with -O hashalg=sha256.
	ed25519Sig256 = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgGBCfU9mJIMNTe0qbVjazoMhWm1
xy2hJTCgyXg7/xHUoAAAAIYW55Z3JhZGUAAAAAAAAABnNoYTI1NgAAAFMAAAALc3NoLWVk
MjU1MTkAAABAsxK4CQ3AVOk0oevY0WHpwjKXpYNgJfo+AMkUIbgmeaiX7q0iwYwefNz6EK
3cBFK7joGcce9/+9LMthggBnISDA==
-----END SSH SIGNATURE-----
`
	// Same key and message, namespace "file" instead of "anygrade": a valid
	// signature the student really made, for something that is not this proof.
	ed25519SigFileNS = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgGBCfU9mJIMNTe0qbVjazoMhWm1
xy2hJTCgyXg7/xHUoAAAAEZmlsZQAAAAAAAAAGc2hhNTEyAAAAUwAAAAtzc2gtZWQyNTUx
OQAAAEAsfgqB3kgx4bHfA+Bi9sGqfr2/InNdpOGKcMadA4C8tkoz3d6ci3NoQXr5YrKSjO
Ude0ox2WlY0DhwnlDTyxkP
-----END SSH SIGNATURE-----
`

	ecdsaPub = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBB6hDPEP+bV+d7t0KGuQFBBFrfbEnOec+dR7fO6R1PN7Jdh7WPArfu6HX1rNaOFoCWLPxwDCvgSf9lAXBJxzFhU= test"
	ecdsaSig = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAAGgAAAATZWNkc2Etc2hhMi1uaXN0cDI1NgAAAAhuaXN0cDI1NgAAAE
EEHqEM8Q/5tX53u3Qoa5AUEEWt9sSc55z51Ht87pHU83sl2HtY8Ct+7odfWs1o4WgJYs/H
AMK+BJ/2UBcEnHMWFQAAAAhhbnlncmFkZQAAAAAAAAAGc2hhNTEyAAAAZAAAABNlY2RzYS
1zaGEyLW5pc3RwMjU2AAAASQAAACEAlXQgPC/2r5FT+/4mj106DXBE5hYflTwjxCtyewtD
+NMAAAAgYUrqkaVbk/RKYhtnuzHFJigHs2rFc4xFuWejSfAb4yE=
-----END SSH SIGNATURE-----
`

	rsaPub = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCk8+dduJiwLE8248uZY8Y24fzKcYJAuwmj1e3nWd3wP2asZneNe8YN4jIMHQtVKj6WSeRc6ZeKTmLS2NNoT4FwRKhdexsNduGLdrb9AAuUI4ol4PxTmct2mdPSqkEDS7BronT2yly+q+RBCkL37uA3akAWZuPtZwfRpxd3K3qfyEgplGxVnODqy2fqAXGJrDD5VCttgDuxFDTx7Ej69hPT5FupB7Dmt3ymVqWMl9S2D5D+mrPWwJcYhoX6By/aABTNXMR7TGGtR7x2q3cEve0gzFNyTMFPsfHkgBlzEaU60HqftKZfaX8WZMWNL1RbHkhOhhh+hE1EKz+U/ZbSn3Te3WeKQtPAGgx6ph445scrQW77n0TOcw0U6I6ap+1iZRqakv2wOUYvrtGCkk1rvDtBgtYcpVJILxE6d2G+wNT1N5eeKbjoGUEwSsfYtOol6DIqzZFWgOEE33EZba36wya9w7zCWV2LHTPiwjNC2NjnsxUYoSNN/8SMfFYUAAtT0AM= test"
	rsaSig = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAAZcAAAAHc3NoLXJzYQAAAAMBAAEAAAGBAKTz5124mLAsTzbjy5ljxj
bh/MpxgkC7CaPV7edZ3fA/Zqxmd417xg3iMgwdC1UqPpZJ5Fzpl4pOYtLY02hPgXBEqF17
Gw124Yt2tv0AC5QjiiXg/FOZy3aZ09KqQQNLsGuidPbKXL6r5EEKQvfu4DdqQBZm4+1nB9
GnF3crep/ISCmUbFWc4OrLZ+oBcYmsMPlUK22AO7EUNPHsSPr2E9PkW6kHsOa3fKZWpYyX
1LYPkP6as9bAlxiGhfoHL9oAFM1cxHtMYa1HvHardwS97SDMU3JMwU+x8eSAGXMRpTrQep
+0pl9pfxZkxY0vVFseSE6GGH6ETUQrP5T9ltKfdN7dZ4pC08AaDHqmHjjmxytBbvufRM5z
DRTojpqn7WJlGpqS/bA5Ri+u0YKSTWu8O0GC1hylUkgvETp3Yb7A1PU3l54puOgZQTBKx9
i06iXoMirNkVaA4QTfcRltrfrDJr3DvMJZXYsdM+LCM0LY2OezFRihI03/xIx8VhQAC1PQ
AwAAAAhhbnlncmFkZQAAAAAAAAAGc2hhNTEyAAABlAAAAAxyc2Etc2hhMi01MTIAAAGALV
10Rwi8icnxkWMJYnVOOY8J8d0ZvHJjGwO1yKEY7FEodrQPCK5XPhpQrVgInjzbn1HFrnSS
qEoocOrZpxwKMeI6paSkU5MxFowLcL8i4Z9iRym+qiBbzEew7hoARxXUlTaNjrq7w2M5QP
ET6myK/TSe+S7v4m24O3goFKGTYHR85/ZQGKMwCSW2z3gvcV1kxn0DOSkB7De9z8KtkvoG
+qg+3X7fUzVWpxG6hPvFcQba0FtByJ9XPbrENxZ0aQ3HF8LaPAwlLbRUhZz9iLr3Dw2kQv
Sf0NcJ5TjN9wBoOijqcxqH1wzg/fgeZhMlgJxjZm3/6zAG0yFUnCZ4GoDrBcOv3HmX9XHf
1qM4wX17OoZukaRhMSrszOhnRCwdq7Pcz9M9aVUV1HJ1ImoMTrRXGukh11wXdnuFOFHeu3
DotnpPQmpmJgTiEdYpQY6rxqE/ypJ2Y2GCiEbsBRqCqxzJMG0t6/O6KGESXwMw3WvEiKGg
OMfLibonUZoWKJGLZbtu
-----END SSH SIGNATURE-----
`
)

func mustKey(t *testing.T, authorized string) ssh.PublicKey {
	t.Helper()
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		t.Fatalf("parse %q: %v", authorized, err)
	}
	return pk
}

// TestVerifyGolden pins interop with the real ssh-keygen across every key type
// and both hash algorithms OpenSSH emits.
func TestVerifyGolden(t *testing.T) {
	for _, tc := range []struct{ name, pub, sig string }{
		{"ed25519/sha512", ed25519Pub, ed25519Sig},
		{"ed25519/sha256", ed25519Pub, ed25519Sig256},
		{"ecdsa/sha512", ecdsaPub, ecdsaSig},
		{"rsa/sha512", rsaPub, rsaSig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(mustKey(t, tc.pub), goldenNamespace, []byte(goldenMessage), []byte(tc.sig)); err != nil {
				t.Fatalf("Verify = %v, want nil", err)
			}
		})
	}
}

// TestVerifyRejects covers the ways a proof can be wrong. Every one of these
// would otherwise let somebody register a key they do not hold, or reuse a
// signature that was never a proof for this server.
func TestVerifyRejects(t *testing.T) {
	other := mustKey(t, ecdsaPub)
	tests := []struct {
		name    string
		key     ssh.PublicKey
		ns      string
		message string
		sig     string
		want    error
	}{
		{
			// Signature over a different challenge: the whole point of the nonce.
			name: "different message", key: mustKey(t, ed25519Pub), ns: goldenNamespace,
			message: goldenMessage + "x", sig: ed25519Sig, want: ErrSignature,
		},
		{
			// Signed with a key the student really holds, submitted for a key
			// they do not: the blob's own public key is what gives it away.
			name: "key mismatch", key: other, ns: goldenNamespace,
			message: goldenMessage, sig: ed25519Sig, want: ErrMalformed,
		},
		{
			// A valid signature the student made for git or for a file, replayed
			// here. Namespace is inside the signed bytes, so it cannot be edited.
			name: "wrong namespace in blob", key: mustKey(t, ed25519Pub), ns: goldenNamespace,
			message: goldenMessage, sig: ed25519SigFileNS, want: ErrMalformed,
		},
		{
			name: "server expects another namespace", key: mustKey(t, ed25519Pub), ns: "other",
			message: goldenMessage, sig: ed25519Sig, want: ErrMalformed,
		},
		{
			name: "not pem", key: mustKey(t, ed25519Pub), ns: goldenNamespace,
			message: goldenMessage, sig: "just some text", want: ErrMalformed,
		},
		{
			name: "empty", key: mustKey(t, ed25519Pub), ns: goldenNamespace,
			message: goldenMessage, sig: "", want: ErrMalformed,
		},
		{
			name: "wrong pem type", key: mustKey(t, ed25519Pub), ns: goldenNamespace,
			message: goldenMessage,
			sig:     strings.ReplaceAll(ed25519Sig, "SSH SIGNATURE", "PGP SIGNATURE"),
			want:    ErrMalformed,
		},
		{
			name: "no key", key: nil, ns: goldenNamespace,
			message: goldenMessage, sig: ed25519Sig, want: ErrMalformed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.key, tc.ns, []byte(tc.message), []byte(tc.sig))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestVerifyRejectsTruncatedAndPadded: the blob is length-prefixed all the way
// down, so neither cutting bytes off it nor appending junk may still parse.
func TestVerifyRejectsTruncatedAndPadded(t *testing.T) {
	key := mustKey(t, ed25519Pub)
	inner, _ := pem.Decode([]byte(ed25519Sig))
	reArmor := func(b []byte) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: b})
	}
	for _, tc := range []struct {
		name string
		sig  []byte
	}{
		{"truncated armor", []byte(ed25519Sig)[:len(ed25519Sig)/2]},
		{"truncated blob", reArmor(inner.Bytes[:len(inner.Bytes)-4])},
		{"junk appended to the blob", reArmor(append(append([]byte{}, inner.Bytes...), 'x'))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(key, goldenNamespace, []byte(goldenMessage), tc.sig); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Verify = %v, want %v", err, ErrMalformed)
			}
		})
	}
}

// TestVerifyRejectsOversizedArmor keeps a form field from making the server
// decode megabytes before the first structural check.
func TestVerifyRejectsOversizedArmor(t *testing.T) {
	key := mustKey(t, ed25519Pub)
	huge := make([]byte, maxArmorSize+1)
	if err := Verify(key, goldenNamespace, []byte(goldenMessage), huge); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify = %v, want %v", err, ErrMalformed)
	}
}

// TestVerifyRejectsUnsupportedHash: the hash algorithm is attacker-chosen
// inside the blob, so an unknown or weak one must be refused, not mapped onto
// something the verifier happens to have.
func TestVerifyRejectsUnsupportedHash(t *testing.T) {
	if _, err := digest("sha1", []byte("x")); !errors.Is(err, ErrMalformed) {
		t.Fatalf("digest(sha1) = %v, want %v", err, ErrMalformed)
	}
}

// TestVerifyAgainstFreshKey signs in-process, so the happy path does not
// depend on the goldens staying readable.
func TestVerifyAgainstFreshKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	armored, err := Sign(signer, "anygrade", []byte("nonce-42"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(sshPub, "anygrade", []byte("nonce-42"), armored); err != nil {
		t.Fatalf("Verify = %v, want nil", err)
	}
}

// reArmor rebuilds an armored blob from a modified structure, for the negative
// cases that need a blob no ssh-keygen would ever emit.
func reArmorBlob(t *testing.T, b blob) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: ssh.Marshal(b)})
}

func decodeBlob(t *testing.T, armored string) blob {
	t.Helper()
	block, _ := pem.Decode([]byte(armored))
	var b blob
	if err := ssh.Unmarshal(block.Bytes, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestVerifyRejectsNonEmptyReserved: the reserved string is echoed into the
// bytes that get signed, so a non-empty one would smuggle attacker-chosen data
// into the signed message. OpenSSH never writes one.
func TestVerifyRejectsNonEmptyReserved(t *testing.T) {
	b := decodeBlob(t, ed25519Sig)
	b.Reserved = []byte("extra")
	err := Verify(mustKey(t, ed25519Pub), goldenNamespace, []byte(goldenMessage), reArmorBlob(t, b))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify = %v, want %v", err, ErrMalformed)
	}
}

// TestVerifyRejectsTrailingSignatureBytes: `ssh:"rest"` is the one field
// ssh.Unmarshal cannot reject, and only security-key signatures legitimately
// use it.
func TestVerifyRejectsTrailingSignatureBytes(t *testing.T) {
	b := decodeBlob(t, ed25519Sig)
	b.Signature = append(append([]byte{}, b.Signature...), 'X', 'Y')
	err := Verify(mustKey(t, ed25519Pub), goldenNamespace, []byte(goldenMessage), reArmorBlob(t, b))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify = %v, want %v", err, ErrMalformed)
	}
}

// TestVerifyRejectsRSASHA1: x/crypto still accepts "ssh-rsa" (RSA over SHA-1)
// for an ssh-rsa key. OpenSSH has never emitted it for SSHSIG, so refusing it
// costs nothing and keeps a deprecated hash out of a credential path.
func TestVerifyRejectsRSASHA1(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	as, ok := signer.(ssh.AlgorithmSigner)
	if !ok {
		t.Fatal("rsa signer is not an AlgorithmSigner")
	}

	const message = "agc_rsa"
	hash, err := digest("sha512", []byte(message))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := as.SignWithAlgorithm(rand.Reader, ssh.Marshal(signedData{
		Magic: magic, Namespace: goldenNamespace, HashAlg: "sha512", Hash: hash,
	}), ssh.KeyAlgoRSA)
	if err != nil {
		t.Fatal(err)
	}
	if sig.Format != ssh.KeyAlgoRSA {
		t.Fatalf("signature format = %q, want %q", sig.Format, ssh.KeyAlgoRSA)
	}
	armored := reArmorBlob(t, blob{
		Magic:     magic,
		Version:   sigVersion,
		PublicKey: signer.PublicKey().Marshal(),
		Namespace: goldenNamespace,
		HashAlg:   "sha512",
		Signature: ssh.Marshal(struct {
			Format string
			Blob   []byte
		}{sig.Format, sig.Blob}),
	})
	if err := Verify(signer.PublicKey(), goldenNamespace, []byte(message), armored); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify = %v, want %v", err, ErrMalformed)
	}
}
