package utlsbridge

import (
	"crypto/tls"

	utls "github.com/refraction-networking/utls"
)

// The WorkBuddy desktop client is a Node main process, not the Chromium
// renderer. The captured ClientHello therefore has 18 cipher suites, 10
// extensions, no ALPN, and the groups X25519 / P-256 / P-384. The same
// fingerprint was observed for both www.workbuddy.cn and www.workbuddy.ai.
var officialCipherSuites = []uint16{
	tls.TLS_AES_128_GCM_SHA256,
	tls.TLS_AES_256_GCM_SHA384,
	tls.TLS_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
}

var officialSigAlgs = []utls.SignatureScheme{
	0x0403, // ecdsa_secp256r1_sha256
	0x0804, // rsa_pss_rsae_sha256
	0x0401, // rsa_pkcs1_sha256
	0x0503, // ecdsa_secp384r1_sha384
	0x0805, // rsa_pss_rsae_sha384
	0x0501, // rsa_pkcs1_sha384
	0x0806, // rsa_pss_rsae_sha512
	0x0601, // rsa_pkcs1_sha512
	0x0201, // rsa_pkcs1_sha1
}

func officialHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		CipherSuites:       officialCipherSuites,
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.UtlsExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519,
				utls.CurveP256,
				utls.CurveP384,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: officialSigAlgs},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519},
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13,
				tls.VersionTLS12,
			}},
		},
	}
}
