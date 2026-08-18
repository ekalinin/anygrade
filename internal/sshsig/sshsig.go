// Package sshsig verifies the armored signatures `ssh-keygen -Y sign`
// produces (OpenSSH's SSHSIG format), so anygrade can make a student prove
// possession of an SSH private key before registering its public half
// (SPEC §8).
//
// It is deliberately a pure-Go leaf package over golang.org/x/crypto/ssh,
// which is already a dependency, rather than a wrapper around `ssh-keygen -Y
// verify`. Two reasons. The host-dependency budget: SPEC §1 keeps the server's
// requirements at the `git` binary plus optionally docker, and openssh-client
// is not in every minimal container anygrade is deployed into - a missing
// binary would turn key registration off rather than fail loudly. And the
// shape of the shell-out: `ssh-keygen -Y verify` takes its trusted key set
// from an allowed_signers *file*, so every verification would mean writing
// attacker-supplied bytes to disk and reading a verdict out of an exit code.
// Parsing the blob here is more code, but it is code with no ambient state.
//
// Verify is the only half the server runs: students sign with their own
// `ssh-keygen`, which is the side of the flow where "no new tooling" matters.
// Sign exists so the flow can be exercised in tests without that binary.
package sshsig

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/pem"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
)

// pemType is the armor label OpenSSH writes around an SSHSIG blob.
const pemType = "SSH SIGNATURE"

// magic and sigVersion are the SSHSIG preamble (PROTOCOL.sshsig). Both the
// outer blob and the bytes that are actually signed start with the magic, so a
// signature can never be replayed as some other SSH protocol message.
var magic = [6]byte{'S', 'S', 'H', 'S', 'I', 'G'}

const sigVersion = 1

// maxArmorSize bounds the pasted blob. An SSHSIG over an RSA-4096 key armors
// to well under 2 KiB; the limit only exists so a form field cannot make the
// server base64-decode megabytes before the first structural check.
const maxArmorSize = 16 << 10

// ErrMalformed is returned for anything that is not a well-formed SSHSIG blob
// for the expected key and namespace; ErrSignature for a blob that is
// well-formed but does not verify. Callers that show the student a message
// should not distinguish them - both mean "that is not a valid proof" - but
// the split keeps server-side logs useful.
var (
	ErrMalformed = errors.New("sshsig: malformed signature")
	ErrSignature = errors.New("sshsig: signature does not verify")
)

// blob is the outer SSHSIG structure (PROTOCOL.sshsig). ssh.Unmarshal rejects
// trailing bytes when no field is tagged `ssh:"rest"`, which is what keeps a
// blob with appended junk from being accepted.
type blob struct {
	Magic     [6]byte
	Version   uint32
	PublicKey []byte
	Namespace string
	Reserved  []byte
	HashAlg   string
	Signature []byte
}

// signedData is what the key actually signs: the same magic, the namespace and
// hash algorithm from the blob, and the digest of the message. Namespace is
// inside the signed bytes, so a signature made for another tool's namespace
// (git commit signing uses "git", `ssh-keygen -Y sign` on a file uses "file")
// cannot be replayed as an anygrade proof.
type signedData struct {
	Magic     [6]byte
	Namespace string
	Reserved  []byte
	HashAlg   string
	Hash      []byte
}

// Verify reports whether armored is a valid SSHSIG over message, made in
// namespace by the private half of want.
//
// want is the key the caller intends to register: the blob carries its own
// copy of the public key and Verify requires the two to be identical, so
// signing a challenge with a key you do hold never proves anything about a key
// you do not.
func Verify(want ssh.PublicKey, namespace string, message, armored []byte) error {
	if want == nil {
		return fmt.Errorf("%w: no public key", ErrMalformed)
	}
	if len(armored) == 0 || len(armored) > maxArmorSize {
		return fmt.Errorf("%w: %d armored bytes", ErrMalformed, len(armored))
	}
	block, _ := pem.Decode(armored)
	if block == nil || block.Type != pemType {
		return fmt.Errorf("%w: not a %q block", ErrMalformed, pemType)
	}

	var b blob
	if err := ssh.Unmarshal(block.Bytes, &b); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if b.Magic != magic || b.Version != sigVersion {
		return fmt.Errorf("%w: preamble %q version %d", ErrMalformed, b.Magic[:], b.Version)
	}
	if b.Namespace != namespace {
		return fmt.Errorf("%w: namespace %q, want %q", ErrMalformed, b.Namespace, namespace)
	}

	signer, err := ssh.ParsePublicKey(b.PublicKey)
	if err != nil {
		return fmt.Errorf("%w: embedded public key: %v", ErrMalformed, err)
	}
	// Compare the wire encodings, not the fingerprints: the fingerprint is a
	// hash of exactly these bytes, so this is the same check without trusting a
	// second hash, and it also pins the key type.
	if fp, wp := signer.Marshal(), want.Marshal(); len(fp) != len(wp) || string(fp) != string(wp) {
		return fmt.Errorf("%w: signed by %s, not %s",
			ErrMalformed, ssh.FingerprintSHA256(signer), ssh.FingerprintSHA256(want))
	}

	hash, err := digest(b.HashAlg, message)
	if err != nil {
		return err
	}
	toVerify := ssh.Marshal(signedData{
		Magic:     b.Magic,
		Namespace: b.Namespace,
		Reserved:  b.Reserved,
		HashAlg:   b.HashAlg,
		Hash:      hash,
	})

	var sig struct {
		Format string
		Blob   []byte
		Rest   []byte `ssh:"rest"`
	}
	if err := ssh.Unmarshal(b.Signature, &sig); err != nil {
		return fmt.Errorf("%w: inner signature: %v", ErrMalformed, err)
	}
	if err := want.Verify(toVerify, &ssh.Signature{Format: sig.Format, Blob: sig.Blob, Rest: sig.Rest}); err != nil {
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
	return nil
}

// Sign produces the armored SSHSIG that `ssh-keygen -Y sign -n namespace`
// would produce for message, always under sha512 (OpenSSH's own default).
//
// The server never signs anything: this is the inverse of Verify, and it is
// here so that every layer above - the store, the handlers, the end-to-end
// suite - can drive the real registration flow without an OpenSSH binary on
// the machine running the tests. Keeping it beside Verify is also what keeps
// one definition of the wire format; the goldens in the test file are what
// keep that definition honest against the real ssh-keygen.
func Sign(signer ssh.Signer, namespace string, message []byte) ([]byte, error) {
	hash, err := digest("sha512", message)
	if err != nil {
		return nil, err
	}
	sig, err := signer.Sign(rand.Reader, ssh.Marshal(signedData{
		Magic: magic, Namespace: namespace, HashAlg: "sha512", Hash: hash,
	}))
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: ssh.Marshal(blob{
		Magic:     magic,
		Version:   sigVersion,
		PublicKey: signer.PublicKey().Marshal(),
		Namespace: namespace,
		HashAlg:   "sha512",
		Signature: ssh.Marshal(struct {
			Format string
			Blob   []byte
		}{sig.Format, sig.Blob}),
	})}), nil
}

// digest applies the hash algorithm named in the blob. OpenSSH only ever emits
// sha512 (the default) or sha256; anything else - including sha1 - is refused
// rather than mapped, so a signature is never accepted under a weaker hash
// than the one its producer chose.
func digest(alg string, message []byte) ([]byte, error) {
	switch alg {
	case "sha512":
		sum := sha512.Sum512(message)
		return sum[:], nil
	case "sha256":
		sum := sha256.Sum256(message)
		return sum[:], nil
	default:
		return nil, fmt.Errorf("%w: hash algorithm %q", ErrMalformed, alg)
	}
}
