package traefik_jwt_plugin

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config the plugin configuration.
type Config struct {
	OpaUrl        string
	OpaAllowField string
	PayloadFields []string
	Required      bool
	Keys          []string
	Alg           string
	Iss           string
	Aud           string
}

// CreateConfig creates a new OPA Config
func CreateConfig() *Config {
	return &Config{}
}

// JwtPlugin contains the runtime config
type JwtPlugin struct {
	next          http.Handler
	opaUrl        string
	opaAllowField string
	payloadFields []string
	required      bool
	keys          map[string]interface{}
	alg           string
	iss           string
	aud           string
}

// LogEvent contains a single log entry
type LogEvent struct {
	Level      string    `json:"level"`
	Msg        string    `json:"msg"`
	Time       time.Time `json:"time"`
	RemoteAddr string    `json:"remote"`
	URL        string    `json:"url"`
	Sub        string    `json:"sub"`
}

type JWTHeader struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Typ  string   `json:"typ"`
	Cty  string   `json:"cty"`
	Crit []string `json:"crit"`
}

type JSONWebToken struct {
	Plaintext []byte
	Signature []byte
	Header    JWTHeader
	Payload   map[string]interface{}
}

var supportedHeaderNames = map[string]struct{}{"alg": {}, "kid": {}, "typ": {}, "cty": {}, "crit": {}}

// Key is a JSON web key returned by the JWKS request.
type Key struct {
	Kid string   `json:"kid"`
	Kty string   `json:"kty"`
	Alg string   `json:"alg"`
	Use string   `json:"use"`
	X5c []string `json:"x5c"`
	X5t string   `json:"x5t"`
	N   string   `json:"n"`
	E   string   `json:"e"`
	K   string   `json:"k,omitempty"`
	X   string   `json:"x,omitempty"`
	Y   string   `json:"y,omitempty"`
	D   string   `json:"d,omitempty"`
	P   string   `json:"p,omitempty"`
	Q   string   `json:"q,omitempty"`
	Dp  string   `json:"dp,omitempty"`
	Dq  string   `json:"dq,omitempty"`
	Qi  string   `json:"qi,omitempty"`
}

// Keys represents a set of JSON web keys.
type Keys struct {
	// Keys is an array of JSON web keys.
	Keys []Key `json:"keys"`
}

// PayloadInput is the input payload
type PayloadInput struct {
	Host       string                 `json:"host"`
	Method     string                 `json:"method"`
	Path       []string               `json:"path"`
	Parameters url.Values             `json:"parameters"`
	Headers    map[string][]string    `json:"headers"`
	JWTHeader  JWTHeader              `json:"tokenHeader"`
	JWTPayload map[string]interface{} `json:"tokenPayload"`
}

// Payload for OPA requests
type Payload struct {
	Input *PayloadInput `json:"input"`
}

// Response from OPA
type Response struct {
	Result map[string]json.RawMessage `json:"result"`
}

// New creates a new plugin
func New(_ context.Context, next http.Handler, config *Config, _ string) (http.Handler, error) {
	keys, err := getKeyFromCertOrJWK(config.Keys)
	if err != nil {
		return nil, err
	}
	return &JwtPlugin{
		next:          next,
		opaUrl:        config.OpaUrl,
		opaAllowField: config.OpaAllowField,
		payloadFields: config.PayloadFields,
		required:      config.Required,
		keys:          keys,
		alg:           config.Alg,
		iss:           config.Iss,
		aud:           config.Aud,
	}, nil
}

func getKeyFromCertOrJWK(certificates []string) (map[string]interface{}, error) {
	var keys = make(map[string]interface{})
	for _, certificate := range certificates {
		if block, rest := pem.Decode([]byte(certificate)); block != nil {
			if len(rest) > 0 {
				return nil, fmt.Errorf("extra data after a PEM certificate block")
			}
			if block.Type == "CERTIFICATE" {
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil, fmt.Errorf("failed to parse a PEM certificate: %v", err)
				}
				keys[base64.RawURLEncoding.EncodeToString(cert.SubjectKeyId)] = cert.PublicKey
			} else if block.Type == "PUBLIC KEY" || block.Type == "RSA PUBLIC KEY" {
				key, err := x509.ParsePKIXPublicKey(block.Bytes)
				if err != nil {
					return nil, fmt.Errorf("failed to parse a PEM public key: %v", err)
				}
				keys[strconv.Itoa(len(keys))] = key
			} else {
				return nil, fmt.Errorf("failed to extract a Key from the PEM certificate")
			}
		} else {
			if u, err := url.ParseRequestURI(certificate); err == nil {
				response, err := http.Get(u.String())
				if err == nil {
					body, err := ioutil.ReadAll(response.Body)
					if err == nil {
						var jwksKeys Keys
						err := json.Unmarshal(body, &jwksKeys)
						if err == nil {
							for _, key := range jwksKeys.Keys {
								switch key.Kty {
								case "RSA":
									{
										nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
										if err != nil {
											return nil, err
										}
										eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
										if err != nil {
											return nil, err
										}
										keys[key.Kid] = rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(new(big.Int).SetBytes(eBytes).Uint64())}
									}
								case "EC":
									{
										xBytes, err := base64.RawURLEncoding.DecodeString(key.X)
										if err != nil {
											return nil, err
										}
										yBytes, err := base64.RawURLEncoding.DecodeString(key.Y)
										if err != nil {
											return nil, err
										}
										keys[key.Kid] = ecdsa.PublicKey{X: new(big.Int).SetBytes(xBytes), Y: new(big.Int).SetBytes(yBytes)}
									}
								case "oct":
									{
										kBytes, err := base64.RawURLEncoding.DecodeString(key.K)
										if err != nil {
											return nil, err
										}
										keys[key.Kid] = kBytes
									}
								}
							}
						}
					}
				}
			}
		}
	}

	return keys, nil
}

func (jwtPlugin *JwtPlugin) ServeHTTP(rw http.ResponseWriter, request *http.Request) {
	if err := jwtPlugin.CheckToken(request); err != nil {
		http.Error(rw, err.Error(), http.StatusForbidden)
		return
	}
	jwtPlugin.next.ServeHTTP(rw, request)
}

func (jwtPlugin *JwtPlugin) CheckToken(request *http.Request) error {
	jwtToken, err := jwtPlugin.ExtractToken(request)
	if err != nil {
		return err
	}
	if jwtToken != nil {
		// only verify jwt tokens if keys are configured
		if len(jwtPlugin.keys) > 0 {
			if err = jwtPlugin.VerifyToken(jwtToken); err != nil {
				return err
			}
		}
		for _, fieldName := range jwtPlugin.payloadFields {
			if _, ok := jwtToken.Payload[fieldName]; !ok {
				if jwtPlugin.required {
					return fmt.Errorf("payload missing required field %s", fieldName)
				} else {
					sub := fmt.Sprint(jwtToken.Payload["sub"])
					jsonLogEvent, _ := json.Marshal(&LogEvent{
						Level:      "warning",
						Msg:        fmt.Sprintf("Missing JWT field %s", fieldName),
						Time:       time.Now(),
						Sub:        sub,
						RemoteAddr: request.RemoteAddr,
						URL:        request.URL.String(),
					})
					fmt.Println(string(jsonLogEvent))
				}
			}
		}
	}
	if jwtPlugin.opaUrl != "" {
		if err := jwtPlugin.CheckOpa(request, jwtToken); err != nil {
			return err
		}
	}
	return nil
}

func (jwtPlugin *JwtPlugin) ExtractToken(request *http.Request) (*JSONWebToken, error) {
	authHeader, ok := request.Header["Authorization"]
	if !ok {
		fmt.Println("No Authorization header found")
		return nil, nil
	}
	auth := authHeader[0]
	if !strings.HasPrefix(auth, "Bearer ") {
		fmt.Println("No bearer token")
		return nil, nil
	}
	parts := strings.Split(auth[7:], ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	jwtToken := JSONWebToken{
		Plaintext: []byte(auth[7 : len(parts[0])+len(parts[1])+8]),
		Signature: signature,
	}
	err = json.Unmarshal(header, &jwtToken.Header)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(payload, &jwtToken.Payload)
	if err != nil {
		return nil, err
	}
	return &jwtToken, nil
}

func (jwtPlugin *JwtPlugin) VerifyToken(jwtToken *JSONWebToken) error {
	for _, h := range jwtToken.Header.Crit {
		if _, ok := supportedHeaderNames[h]; !ok {
			return fmt.Errorf("unsupported header: %s", h)
		}
	}
	// Look up the algorithm
	a, ok := tokenAlgorithms[jwtToken.Header.Alg]
	if !ok {
		return fmt.Errorf("unknown JWS algorithm: %s", jwtToken.Header.Alg)
	}
	if jwtPlugin.alg != "" && jwtToken.Header.Alg != jwtPlugin.alg {
		return fmt.Errorf("incorrect alg, expected %s got %s", jwtPlugin.alg, jwtToken.Header.Alg)
	}
	key, ok := jwtPlugin.keys[jwtToken.Header.Kid]
	if ok {
		return a.verify(key, a.hash, jwtToken.Plaintext, jwtToken.Signature)
	} else {
		for _, key := range jwtPlugin.keys {
			err := a.verify(key, a.hash, jwtToken.Plaintext, jwtToken.Signature)
			if err == nil {
				return nil
			}
		}
		return fmt.Errorf("token validation failed")
	}
}

func (jwtPlugin *JwtPlugin) CheckOpa(request *http.Request, token *JSONWebToken) error {
	opaPayload := toOPAPayload(request)
	if (token != nil) {
		opaPayload.Input.JWTHeader =  token.Header
		opaPayload.Input.JWTPayload= token.Payload
	}
	authPayloadAsJSON, err := json.Marshal(opaPayload)
	if err != nil {
		return err
	}
	authResponse, err := http.Post(jwtPlugin.opaUrl, "application/json", bytes.NewBuffer(authPayloadAsJSON))
	if err != nil {
		return err
	}
	body, err := ioutil.ReadAll(authResponse.Body)
	if err != nil {
		return err
	}
	var result Response
	err = json.Unmarshal(body, &result)
	if err != nil {
		return err
	}
	var allow bool
	err = json.Unmarshal(result.Result[jwtPlugin.opaAllowField], &allow)
	if err != nil {
		return err
	}
	if allow != true {
		return fmt.Errorf("%s", body)
	}
	return nil
}

func toOPAPayload(request *http.Request) *Payload {
	return &Payload{
		Input: &PayloadInput{
			Host:       request.Host,
			Method:     request.Method,
			Path:       strings.Split(request.URL.Path, "/")[1:],
			Parameters: request.URL.Query(),
			Headers:    request.Header,
		},
	}
}

type tokenVerifyFunction func(key interface{}, hash crypto.Hash, payload []byte, signature []byte) error
type tokenVerifyAsymmetricFunction func(key interface{}, hash crypto.Hash, digest []byte, signature []byte) error

// jwtAlgorithm describes a JWS 'alg' value
type tokenAlgorithm struct {
	hash   crypto.Hash
	verify tokenVerifyFunction
}

// tokenAlgorithms is the known JWT algorithms
var tokenAlgorithms = map[string]tokenAlgorithm{
	"RS256": {crypto.SHA256, verifyAsymmetric(verifyRSAPKCS)},
	"RS384": {crypto.SHA384, verifyAsymmetric(verifyRSAPKCS)},
	"RS512": {crypto.SHA512, verifyAsymmetric(verifyRSAPKCS)},
	"PS256": {crypto.SHA256, verifyAsymmetric(verifyRSAPSS)},
	"PS384": {crypto.SHA384, verifyAsymmetric(verifyRSAPSS)},
	"PS512": {crypto.SHA512, verifyAsymmetric(verifyRSAPSS)},
	"ES256": {crypto.SHA256, verifyAsymmetric(verifyECDSA)},
	"ES384": {crypto.SHA384, verifyAsymmetric(verifyECDSA)},
	"ES512": {crypto.SHA512, verifyAsymmetric(verifyECDSA)},
	"HS256": {crypto.SHA256, verifyHMAC},
	"HS384": {crypto.SHA384, verifyHMAC},
	"HS512": {crypto.SHA512, verifyHMAC},
}

// errSignatureNotVerified is returned when a signature cannot be verified.
func verifyHMAC(key interface{}, hash crypto.Hash, payload []byte, signature []byte) error {
	macKey, ok := key.([]byte)
	if !ok {
		return fmt.Errorf("incorrect symmetric key type")
	}
	mac := hmac.New(hash.New, macKey)
	if _, err := mac.Write(payload); err != nil {
		return err
	}
	if !hmac.Equal(signature, mac.Sum([]byte{})) {
		return fmt.Errorf("token verification failed (HMAC)")
	}
	return nil
}

func verifyAsymmetric(verify tokenVerifyAsymmetricFunction) tokenVerifyFunction {
	return func(key interface{}, hash crypto.Hash, payload []byte, signature []byte) error {
		h := hash.New()
		_, err := h.Write(payload)
		if err != nil {
			return err
		}
		return verify(key, hash, h.Sum([]byte{}), signature)
	}
}

func verifyRSAPKCS(key interface{}, hash crypto.Hash, digest []byte, signature []byte) error {
	publicKeyRsa := key.(*rsa.PublicKey)
	if err := rsa.VerifyPKCS1v15(publicKeyRsa, hash, digest, signature); err != nil {
		return fmt.Errorf("token verification failed (RSAPKCS)")
	}
	return nil
}

func verifyRSAPSS(key interface{}, hash crypto.Hash, digest []byte, signature []byte) error {
	publicKeyRsa, ok := key.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("incorrect public key type")
	}
	if err := rsa.VerifyPSS(publicKeyRsa, hash, digest, signature, nil); err != nil {
		return fmt.Errorf("token verification failed (RSAPSS)")
	}
	return nil
}

func verifyECDSA(key interface{}, _ crypto.Hash, digest []byte, signature []byte) error {
	publicKeyEcdsa, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("incorrect public key type")
	}
	r, s := &big.Int{}, &big.Int{}
	n := len(signature) / 2
	r.SetBytes(signature[:n])
	s.SetBytes(signature[n:])
	if ecdsa.Verify(publicKeyEcdsa, digest, r, s) {
		return nil
	}
	return fmt.Errorf("token verification failed (ECDSA)")
}
