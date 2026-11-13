package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	dpopSigningAlgorithm = "ES256"
	dpopTokenType        = "dpop+jwt"
	huaweiSigningAlg     = "SDK-HMAC-SHA256"
)

// DPoPProofInfo is the verified, public part of one DPoP JWT. It intentionally
// excludes the signature and any private key material.
type DPoPProofInfo struct {
	Htm       string
	Htu       string
	Iat       int64
	JTI       string
	Nonce     string
	PublicJWK json.RawMessage
}

// GeneratePKCE returns a 64-byte random base64url verifier and its S256
// challenge. The verifier is 86 characters, within RFC 7636's 43..128 bound.
func GeneratePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("codearts: generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// randomHex returns size random bytes as lowercase hex.
func randomHex(size int) (string, error) {
	if size <= 0 {
		return "", errors.New("codearts: random byte count must be positive")
	}
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("codearts: generate random value: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

type dpopJWKFields struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

func decodeBase64URL(text string) ([]byte, error) {
	text = strings.TrimRight(strings.TrimSpace(text), "=")
	if text == "" {
		return nil, errors.New("empty base64url value")
	}
	return base64.RawURLEncoding.DecodeString(text)
}

func decodeJWKCoordinate(text, name string) (*big.Int, error) {
	raw, err := decodeBase64URL(text)
	if err != nil {
		return nil, fmt.Errorf("invalid DPoP JWK %s", name)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("invalid DPoP JWK %s length", name)
	}
	return new(big.Int).SetBytes(raw), nil
}

// parseDPoPPrivateKey reads and validates the persisted private JWK. The
// public point must match the private scalar, so a corrupted or swapped
// document cannot be used to sign a proof with the wrong key id.
func parseDPoPPrivateKey(raw json.RawMessage) (*ecdsa.PrivateKey, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, errors.New("codearts: DPoP private JWK is empty")
	}
	var fields dpopJWKFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("codearts: invalid DPoP JWK")
	}
	if fields.Kty != "EC" || fields.Crv != "P-256" {
		return nil, errors.New("codearts: DPoP JWK must use EC P-256")
	}
	d, err := decodeJWKCoordinate(fields.D, "d")
	if err != nil {
		return nil, err
	}
	x, err := decodeJWKCoordinate(fields.X, "x")
	if err != nil {
		return nil, err
	}
	y, err := decodeJWKCoordinate(fields.Y, "y")
	if err != nil {
		return nil, err
	}

	curve := elliptic.P256()
	params := curve.Params()
	if d.Sign() <= 0 || d.Cmp(params.N) >= 0 {
		return nil, errors.New("codearts: invalid DPoP JWK private scalar")
	}
	if x.Cmp(params.P) >= 0 || y.Cmp(params.P) >= 0 || !curve.IsOnCurve(x, y) {
		return nil, errors.New("codearts: invalid DPoP JWK public point")
	}
	checkX, checkY := curve.ScalarBaseMult(d.FillBytes(make([]byte, 32)))
	if checkX.Cmp(x) != 0 || checkY.Cmp(y) != 0 {
		return nil, errors.New("codearts: DPoP JWK public/private key mismatch")
	}
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         d,
	}, nil
}

func parseDPoPPublicJWK(raw json.RawMessage) (*ecdsa.PublicKey, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, errors.New("codearts: DPoP public JWK is empty")
	}
	var fields dpopJWKFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("codearts: invalid DPoP JWK")
	}
	if fields.Kty != "EC" || fields.Crv != "P-256" {
		return nil, errors.New("codearts: DPoP JWK must use EC P-256")
	}
	if strings.TrimSpace(fields.D) != "" {
		return nil, errors.New("codearts: DPoP header must not contain a private key")
	}
	x, err := decodeJWKCoordinate(fields.X, "x")
	if err != nil {
		return nil, err
	}
	y, err := decodeJWKCoordinate(fields.Y, "y")
	if err != nil {
		return nil, err
	}
	curve := elliptic.P256()
	if x.Cmp(curve.Params().P) >= 0 || y.Cmp(curve.Params().P) >= 0 || !curve.IsOnCurve(x, y) {
		return nil, errors.New("codearts: invalid DPoP JWK public point")
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

// NewDPoPKey generates a P-256 private JWK. The field set matches v1 exactly:
// kty, crv, x, y, d, with each coordinate encoded as 32 base64url bytes.
func NewDPoPKey() (privateJWK json.RawMessage, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("codearts: generate DPoP key: %w", err)
	}
	jwk := map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		"y":   base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
		"d":   base64.RawURLEncoding.EncodeToString(key.D.FillBytes(make([]byte, 32))),
	}
	encoded, err := json.Marshal(jwk)
	if err != nil {
		return nil, fmt.Errorf("codearts: encode DPoP JWK: %w", err)
	}
	return json.RawMessage(encoded), nil
}

// DPoPProof builds an ES256 DPoP JWT. The header carries the matching public
// JWK; the payload carries htm/htu/iat/jti and nonce when the endpoint supplied
// one. Signatures use JOSE's raw r||s form and are low-S normalized.
func DPoPProof(jwk json.RawMessage, method, url, nonce string) (string, error) {
	key, err := parseDPoPPrivateKey(jwk)
	if err != nil {
		return "", err
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	url = strings.TrimSpace(url)
	if method == "" {
		return "", errors.New("codearts: DPoP method is empty")
	}
	if url == "" {
		return "", errors.New("codearts: DPoP URL is empty")
	}
	nonce = strings.TrimSpace(nonce)
	jti, err := randomHex(16)
	if err != nil {
		return "", err
	}

	header := map[string]any{
		"alg": dpopSigningAlgorithm,
		"typ": dpopTokenType,
		"jwk": map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"x":   base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
			"y":   base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
		},
	}
	payload := map[string]any{
		"htm": method,
		"htu": url,
		"iat": time.Now().Unix(),
		"jti": jti,
	}
	if nonce != "" {
		payload["nonce"] = nonce
	}
	headJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("codearts: encode DPoP header: %w", err)
	}
	bodyJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("codearts: encode DPoP payload: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headJSON) + "." +
		base64.RawURLEncoding.EncodeToString(bodyJSON)

	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("codearts: sign DPoP proof: %w", err)
	}
	half := new(big.Int).Rsh(new(big.Int).Set(key.Curve.Params().N), 1)
	if s.Cmp(half) > 0 {
		s = new(big.Int).Sub(key.Curve.Params().N, s)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func verifyES256(public *ecdsa.PublicKey, message, signature []byte) bool {
	if public == nil || len(signature) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	params := public.Curve.Params()
	if r.Sign() <= 0 || s.Sign() <= 0 || r.Cmp(params.N) >= 0 || s.Cmp(params.N) >= 0 {
		return false
	}
	digest := sha256.Sum256(message)
	return ecdsa.Verify(public, digest[:], r, s)
}

type dpopHeader struct {
	Alg string          `json:"alg"`
	Typ string          `json:"typ"`
	JWK json.RawMessage `json:"jwk"`
}

type dpopPayload struct {
	Htm   string `json:"htm"`
	Htu   string `json:"htu"`
	Iat   int64  `json:"iat"`
	JTI   string `json:"jti"`
	Nonce string `json:"nonce"`
}

// VerifyDPoPProof validates the JWT structure, public-only JWK, ES256
// signature, and required claims. It is used by tests and by fake upstreams;
// production code only creates proofs.
func VerifyDPoPProof(proof string) (DPoPProofInfo, error) {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return DPoPProofInfo{}, errors.New("codearts: malformed DPoP proof")
	}
	headJSON, err := decodeBase64URL(parts[0])
	if err != nil {
		return DPoPProofInfo{}, errors.New("codearts: malformed DPoP header")
	}
	bodyJSON, err := decodeBase64URL(parts[1])
	if err != nil {
		return DPoPProofInfo{}, errors.New("codearts: malformed DPoP payload")
	}
	signature, err := decodeBase64URL(parts[2])
	if err != nil {
		return DPoPProofInfo{}, errors.New("codearts: malformed DPoP signature")
	}

	var header dpopHeader
	if err := json.Unmarshal(headJSON, &header); err != nil {
		return DPoPProofInfo{}, errors.New("codearts: malformed DPoP header")
	}
	if header.Alg != dpopSigningAlgorithm || header.Typ != dpopTokenType {
		return DPoPProofInfo{}, errors.New("codearts: unsupported DPoP header")
	}
	public, err := parseDPoPPublicJWK(header.JWK)
	if err != nil {
		return DPoPProofInfo{}, err
	}
	if !verifyES256(public, []byte(parts[0]+"."+parts[1]), signature) {
		return DPoPProofInfo{}, errors.New("codearts: invalid DPoP signature")
	}

	var payload dpopPayload
	if err := json.Unmarshal(bodyJSON, &payload); err != nil {
		return DPoPProofInfo{}, errors.New("codearts: malformed DPoP payload")
	}
	if payload.Htm == "" || payload.Htu == "" || payload.Iat <= 0 || payload.JTI == "" {
		return DPoPProofInfo{}, errors.New("codearts: incomplete DPoP claims")
	}
	return DPoPProofInfo{
		Htm:       payload.Htm,
		Htu:       payload.Htu,
		Iat:       payload.Iat,
		JTI:       payload.JTI,
		Nonce:     payload.Nonce,
		PublicJWK: append(json.RawMessage(nil), header.JWK...),
	}, nil
}

// SignCredentials are the Huawei Cloud AK/SK (and optional STS security token)
// used by SDK-HMAC-SHA256 request signing.
type SignCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SecurityToken   string
}

// SignCredential is an alias matching the reference signer's type name.
type SignCredential = SignCredentials

// SignRequest adds the Huawei Cloud signing headers to req in place, using the
// current UTC time for X-Sdk-Date.
func SignRequest(req *http.Request, body []byte, cred SignCredentials) error {
	return SignRequestAt(req, body, cred, "")
}

// SignRequestAt is SignRequest with a caller-supplied X-Sdk-Date. An empty
// date means now. The format is YYYYMMDDTHHMMSSZ.
func SignRequestAt(req *http.Request, body []byte, cred SignCredentials, xDate string) error {
	if req == nil || req.URL == nil {
		return errors.New("codearts: sign request is missing a URL")
	}
	headers := make(map[string]string, len(req.Header)+5)
	for key, values := range req.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		} else {
			headers[key] = ""
		}
	}
	signed, err := signHeaders(req.Method, req.URL.String(), headers, body, cred, xDate)
	if err != nil {
		return err
	}
	for key, value := range signed {
		req.Header.Set(key, value)
	}
	return nil
}

// signHeaders implements the v1/Python SDK-HMAC-SHA256 canonicalization:
// every outgoing header participates in SignedHeaders, path segments are
// percent-encoded with quote(safe="") semantics, query pairs are sorted and
// percent-encoded, and the body hash is carried in X-Sdk-Content-Sha256.
func signHeaders(
	method, rawURL string, headers map[string]string, body []byte,
	cred SignCredentials, xDate string,
) (map[string]string, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return nil, errors.New("codearts: sign request method is empty")
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("codearts: sign request URL is invalid")
	}
	if xDate == "" {
		xDate = time.Now().UTC().Format("20060102T150405Z")
	}

	out := make(map[string]string, len(headers)+5)
	for key, value := range headers {
		out[key] = value
	}
	out["X-Sdk-Date"] = xDate
	out["Host"] = parsed.Host
	if cred.SecurityToken != "" {
		out["X-Security-Token"] = cred.SecurityToken
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	out["X-Sdk-Content-Sha256"] = payloadHash

	lower := make(map[string]string, len(out))
	for key, value := range out {
		lower[strings.ToLower(key)] = strings.TrimSpace(value)
	}
	keys := make([]string, 0, len(lower))
	for key := range lower {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonicalHeaders strings.Builder
	for _, key := range keys {
		canonicalHeaders.WriteString(key)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(lower[key])
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(keys, ";")

	canonicalRequest := strings.Join([]string{
		method,
		canonicalRequestURI(parsed.EscapedPath()),
		canonicalRequestQuery(parsed.RawQuery),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	canonicalSum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := huaweiSigningAlg + "\n" + xDate + "\n" + hex.EncodeToString(canonicalSum[:])

	mac := hmac.New(sha256.New, []byte(cred.SecretAccessKey))
	if _, err := mac.Write([]byte(stringToSign)); err != nil {
		return nil, fmt.Errorf("codearts: sign request: %w", err)
	}
	signature := hex.EncodeToString(mac.Sum(nil))
	out["Authorization"] = huaweiSigningAlg +
		" Access=" + cred.AccessKeyID +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + signature
	return out, nil
}

// canonicalRequestURI percent-encodes every path byte except RFC 3986
// unreserved characters and appends the trailing slash Huawei expects.
func canonicalRequestURI(path string) string {
	if path == "" {
		path = "/"
	}
	segments := strings.Split(path, "/")
	for i := range segments {
		segments[i] = percentEncode(segments[i])
	}
	out := strings.Join(segments, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

type queryPair struct {
	key   string
	value string
}

// canonicalRequestQuery decodes query pairs, sorts by (key, value), and
// re-encodes with percentEncode, matching v1's parse_qsl + quote(safe="").
func canonicalRequestQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := make([]queryPair, 0, strings.Count(raw, "&")+1)
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			decodedValue = value
		}
		pairs = append(pairs, queryPair{key: decodedKey, value: decodedValue})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key != pairs[j].key {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].value < pairs[j].value
	})
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, percentEncode(pair.key)+"="+percentEncode(pair.value))
	}
	return strings.Join(parts, "&")
}

func percentEncode(text string) string {
	const upperHex = "0123456789ABCDEF"
	var builder strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			builder.WriteByte(c)
		default:
			builder.WriteByte('%')
			builder.WriteByte(upperHex[c>>4])
			builder.WriteByte(upperHex[c&0x0f])
		}
	}
	return builder.String()
}
