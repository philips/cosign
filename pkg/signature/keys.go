// Copyright 2021 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package signature

import (
	"context"
	"crypto"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/sigstore/cosign/pkg/blob"
	"github.com/sigstore/cosign/pkg/cosign"
	"github.com/sigstore/cosign/pkg/cosign/git"
	"github.com/sigstore/cosign/pkg/cosign/git/gitlab"
	"github.com/sigstore/cosign/pkg/cosign/kubernetes"
	"github.com/sigstore/cosign/pkg/cosign/pkcs11key"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/kms"
)

func LoadPublicKey(ctx context.Context, keyRef string) (verifier signature.Verifier, err error) {
	// The key could be plaintext, in a file, at a URL, or in KMS.
	if kmsKey, err := kms.Get(ctx, keyRef, crypto.SHA256); err == nil {
		// KMS specified
		return kmsKey, nil
	}

	raw, err := blob.LoadFileOrURL(keyRef)

	if err != nil {
		return nil, err
	}

	// PEM encoded file.
	pubKey, err := cryptoutils.UnmarshalPEMToPublicKey(raw)
	if err != nil {
		return nil, errors.Wrap(err, "pem to public key")
	}
	return signature.LoadVerifier(pubKey, crypto.SHA256)
}

func loadKey(keyPath string, pf cosign.PassFunc) (*signature.ECDSASignerVerifier, error) {
	kb, err := os.ReadFile(filepath.Clean(keyPath))
	if err != nil {
		return nil, err
	}
	pass, err := pf(false)
	if err != nil {
		return nil, err
	}
	return cosign.LoadECDSAPrivateKey(kb, pass)
}

func loadPublicKey(raw []byte) (signature.Verifier, error) {
	// PEM encoded file.
	ed, err := cosign.PemToECDSAKey(raw)
	if err != nil {
		return nil, errors.Wrap(err, "pem to ecdsa")
	}
	return signature.LoadECDSAVerifier(ed, crypto.SHA256)
}

func SignerFromKeyRef(ctx context.Context, keyRef string, pf cosign.PassFunc) (signature.Signer, error) {
	return SignerVerifierFromKeyRef(ctx, keyRef, pf)
}

func SignerVerifierFromKeyRef(ctx context.Context, keyRef string, pf cosign.PassFunc) (signature.SignerVerifier, error) {
	for prefix := range kms.ProvidersMux().Providers() {
		if strings.HasPrefix(keyRef, prefix) {
			return kms.Get(ctx, keyRef, crypto.SHA256)
		}
	}

	switch {
	case strings.HasPrefix(keyRef, pkcs11key.ReferenceScheme):
		pkcs11UriConfig := pkcs11key.NewPkcs11UriConfig()
		err := pkcs11UriConfig.Parse(keyRef)
		if err != nil {
			return nil, errors.Wrap(err, "parsing pkcs11 uri")
		}

		// Since we'll be signing, we need to set askForPinIsNeeded to true
		// because we need access to the private key.
		sk, err := pkcs11key.GetKeyWithURIConfig(pkcs11UriConfig, true)
		if err != nil {
			return nil, errors.Wrap(err, "opening pkcs11 token key")
		}

		sv, err := sk.SignerVerifier()
		if err != nil {
			return nil, errors.Wrap(err, "initializing pkcs11 token signer verifier")
		}

		return sv, nil
	case strings.HasPrefix(keyRef, kubernetes.KeyReference):
		s, err := kubernetes.GetKeyPairSecret(ctx, keyRef)
		if err != nil {
			return nil, err
		}

		if len(s.Data) > 0 {
			return cosign.LoadECDSAPrivateKey(s.Data["cosign.key"], s.Data["cosign.password"])
		}
	case strings.HasPrefix(keyRef, gitlab.ReferenceScheme):
		split := strings.Split(keyRef, "://")

		if len(split) < 2 {
			return nil, errors.New("could not parse scheme, use <scheme>://<ref> format")
		}

		provider, targetRef := split[0], split[1]

		pk, err := git.GetProvider(provider).GetSecret(ctx, targetRef, "COSIGN_PRIVATE_KEY")
		if err != nil {
			return nil, err
		}

		pass, err := git.GetProvider(provider).GetSecret(ctx, targetRef, "COSIGN_PASSWORD")
		if err != nil {
			return nil, err
		}

		return cosign.LoadECDSAPrivateKey([]byte(pk), []byte(pass))
	default:
	}

	return loadKey(keyRef, pf)
}

func PublicKeyFromKeyRef(ctx context.Context, keyRef string) (signature.Verifier, error) {
	if strings.HasPrefix(keyRef, kubernetes.KeyReference) {
		s, err := kubernetes.GetKeyPairSecret(ctx, keyRef)
		if err != nil {
			return nil, err
		}

		if len(s.Data) > 0 {
			return loadPublicKey(s.Data["cosign.pub"])
		}
	}

	if strings.HasPrefix(keyRef, pkcs11key.ReferenceScheme) {
		pkcs11UriConfig := pkcs11key.NewPkcs11UriConfig()
		err := pkcs11UriConfig.Parse(keyRef)
		if err != nil {
			return nil, errors.Wrap(err, "parsing pkcs11 uri")
		}

		// Since we'll be verifying a signature, we do not need to set askForPinIsNeeded to true
		// because we only need access to the public key.
		sk, err := pkcs11key.GetKeyWithURIConfig(pkcs11UriConfig, false)
		if err != nil {
			return nil, errors.Wrap(err, "opening pkcs11 token key")
		}

		v, err := sk.Verifier()
		if err != nil {
			return nil, errors.Wrap(err, "initializing pkcs11 token verifier")
		}

		return v, nil
	} else if strings.HasPrefix(keyRef, gitlab.ReferenceScheme) {
		split := strings.Split(keyRef, "://")

		if len(split) < 2 {
			return nil, errors.New("could not parse scheme, use <scheme>://<ref> format")
		}

		provider, targetRef := split[0], split[1]

		pubKey, err := git.GetProvider(provider).GetSecret(ctx, targetRef, "COSIGN_PUBLIC_KEY")
		if err != nil {
			return nil, err
		}

		if len(pubKey) > 0 {
			return loadPublicKey([]byte(pubKey))
		}
	}

	return LoadPublicKey(ctx, keyRef)
}

func PublicKeyPem(key signature.PublicKeyProvider, pkOpts ...signature.PublicKeyOption) ([]byte, error) {
	pub, err := key.PublicKey(pkOpts...)
	if err != nil {
		return nil, err
	}
	return cryptoutils.MarshalPublicKeyToPEM(pub)
}

func CertSubject(c *x509.Certificate) string {
	switch {
	case c.EmailAddresses != nil:
		return c.EmailAddresses[0]
	case c.URIs != nil:
		return c.URIs[0].String()
	}
	return ""
}

func CertIssuerExtension(cert *x509.Certificate) string {
	for _, ext := range cert.Extensions {
		if ext.Id.String() == "1.3.6.1.4.1.57264.1.1" {
			return string(ext.Value)
		}
	}
	return ""
}
