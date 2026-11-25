package codearts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// decodeSessionJSON accepts the session object itself or a JSON string that
// contains it.
func decodeSessionJSON(raw any) (map[string]any, *adminError) {
	switch value := raw.(type) {
	case map[string]any:
		return value, nil
	case string:
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		var decoded map[string]any
		if err := decoder.Decode(&decoded); err != nil || decoded == nil {
			return nil, badAdminRequest("session_json is not a JSON object")
		}
		return decoded, nil
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, badAdminRequest("session_json is not a JSON object")
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded == nil {
			return nil, badAdminRequest("session_json is not a JSON object")
		}
		return decoded, nil
	}
}

// authFromSessionJSON maps the CodeArts Agent session export into Auth. It
// accepts both the IDE's camelCase fields and the snake_case aliases used by
// existing exporters.
func authFromSessionJSON(payload map[string]any, station Station) (Auth, *adminError) {
	station = stationOrDefault(station)
	if payload == nil {
		return Auth{}, badAdminRequest("session_json is required")
	}
	account := modelMap(payload["account"])
	refreshToken := sessionText(payload["refresh_token"], payload["refreshToken"])
	uid := sessionText(
		account["id"],
		payload["id"],
		payload["user_id"],
		payload["userId"],
	)
	if uid == "" {
		uid = sessionUIDFromJWT(refreshToken)
	}
	if uid == "" {
		return Auth{}, badAdminRequest("session_json requires an account id")
	}
	name := sessionText(
		account["label"],
		account["name"],
		payload["user_name"],
		payload["userName"],
		uid,
	)
	domain := sessionText(payload["domainId"], payload["domain_id"], account["domain_id"])
	accessKey := sessionText(payload["accessKey"], payload["access_key_id"], payload["accessKeyId"])
	secretKey := sessionText(payload["secretKey"], payload["secret_access_key"], payload["secretAccessKey"])
	securityToken := sessionText(
		payload["securityToken"],
		payload["security_token"],
		payload["securitytoken"],
	)
	if accessKey == "" || secretKey == "" || securityToken == "" {
		return Auth{}, badAdminRequest("session_json requires accessKey, secretKey, and securityToken")
	}
	stationType := strings.ToLower(sessionText(payload["stationType"], payload["station_type"], payload["station"]))
	if station.ID == Mainland.ID {
		if stationType == "international" || stationType == "intl" {
			return Auth{}, badAdminRequest("session_json belongs to the international station")
		}
		// International exports may omit stationType. Only explicit mainland
		// sessions are safe here; legacy Auth documents use a separate importer.
		if stationType != "mainland" && stationType != "cn" {
			return Auth{}, badAdminRequest("session_json requires an explicit mainland station")
		}
	} else {
		if stationType != "" && stationType != "international" && stationType != "intl" {
			return Auth{}, badAdminRequest("session_json belongs to a different station")
		}
	}

	login := modelMap(payload["loginContext"])
	pkce := modelMap(login["pkcePair"])
	dpop := modelMap(login["dpopKeyPair"])
	verifier := sessionText(
		payload["code_verifier"],
		payload["codeVerifier"],
		pkce["codeVerifier"],
		pkce["code_verifier"],
	)
	var privateKey json.RawMessage
	if value := dpop["privateKeyJwk"]; value != nil {
		privateKey = sessionRawJSON(value)
	} else if value := sessionFirstValue(payload["dpop_private_key"], payload["dpopPrivateKey"]); value != nil {
		privateKey = sessionRawJSON(value)
	}

	auth := Auth{
		UserID:          uid,
		UserName:        name,
		DomainID:        domain,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		RefreshToken:    refreshToken,
		CodeVerifier:    verifier,
		ClientID:        sessionText(payload["client_id"], payload["clientId"]),
		Expiration:      parseExpirationText(sessionText(payload["expiresAt"], payload["expires_at"], payload["expiration"])),
		DPopPrivateKey:  privateKey,
		UpdatedAt:       time.Now().Unix(),
	}
	if station.ID == Mainland.ID {
		auth.CloudDragonToken = securityToken
	} else {
		auth.Station = station.ID
		auth.SecurityToken = securityToken
	}
	if auth.ClientID == "" {
		auth.ClientID = oauthClientID()
	}
	return auth, nil
}

func sessionFirstValue(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func sessionText(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(modelText(value)); text != "" {
			return text
		}
	}
	return ""
}

func sessionRawJSON(value any) json.RawMessage {
	switch typed := value.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return nil
		}
		return json.RawMessage(text)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil
		}
		return encoded
	}
}

// sessionUIDFromJWT reads a UID from the refresh token's payload. It does not
// verify the signature; the token is used only after STS has issued it.
func sessionUIDFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var claims map[string]any
	if err := decoder.Decode(&claims); err != nil {
		return ""
	}
	profile := modelMap(claims["user_profile"])
	return sessionText(
		profile["user_id"],
		profile["id"],
		profile["sub"],
		claims["user_id"],
		claims["sub"],
	)
}
