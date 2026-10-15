// Package serviceaccount issues and checks service account tokens, like
// k8s.io/kubernetes/pkg/serviceaccount: JWTs whose subject is
// system:serviceaccount:<namespace>:<name>, optionally bound to a Pod.
// Kubernetes signs them with RS256 and publishes the key for other
// verifiers; mini has a single API server, the only verifier, so it signs
// them with HS256 and a key kept in etcd.
package serviceaccount

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"mini-k8s/pkg/auth/user"
)

// Issuer is the iss claim and the default audience of tokens, like the
// API server's --service-account-issuer.
const Issuer = "https://kubernetes.default.svc.cluster.local"

const prefix = "system:serviceaccount:"

// MakeUsername is a service account's user name.
func MakeUsername(namespace, name string) string { return prefix + namespace + ":" + name }

// SplitUsername returns the namespace and name of a service account user.
func SplitUsername(username string) (namespace, name string, ok bool) {
	rest, ok := strings.CutPrefix(username, prefix)
	if !ok {
		return "", "", false
	}
	namespace, name, ok = strings.Cut(rest, ":")
	if !ok || namespace == "" || name == "" || strings.Contains(name, ":") {
		return "", "", false
	}
	return namespace, name, true
}

// MakeGroupNames are the groups every service account of a namespace is in.
func MakeGroupNames(namespace string) []string {
	return []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace}
}

// UserInfo is the identity of a service account.
func UserInfo(namespace, name, uid string) *user.Info {
	return &user.Info{Name: MakeUsername(namespace, name), UID: uid,
		Groups: append(MakeGroupNames(namespace), user.AllAuthenticated)}
}

// Ref names an object by name and UID.
type Ref struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// PrivateClaims are the kubernetes.io claims of a token.
type PrivateClaims struct {
	Namespace      string `json:"namespace"`
	ServiceAccount Ref    `json:"serviceaccount"`
	Pod            *Ref   `json:"pod,omitempty"`
}

// Claims are the claims of a token.
type Claims struct {
	Audience   []string      `json:"aud"`
	Expiry     int64         `json:"exp"`
	IssuedAt   int64         `json:"iat"`
	Issuer     string        `json:"iss"`
	ID         string        `json:"jti"`
	Kubernetes PrivateClaims `json:"kubernetes.io"`
	NotBefore  int64         `json:"nbf"`
	Subject    string        `json:"sub"`
}

var enc = base64.RawURLEncoding

// header is the JOSE header of every token.
var header = enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// NewClaims fills the claims of a token issued now for ttl.
func NewClaims(now time.Time, ttl time.Duration, audiences []string, private PrivateClaims) Claims {
	return Claims{Audience: audiences, Expiry: now.Add(ttl).Unix(), IssuedAt: now.Unix(), Issuer: Issuer, ID: newID(),
		Kubernetes: private, NotBefore: now.Unix(), Subject: MakeUsername(private.Namespace, private.ServiceAccount.Name)}
}

// Sign returns the token of claims, signed with key.
func Sign(key []byte, c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	unsigned := header + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// Errors of Verify.
var (
	ErrNotServiceAccountToken = errors.New("not a service account token")
	ErrInvalidToken           = errors.New("invalid token")
)

// Verify checks a token's signature, issuer, lifetime and audience
// (leeway: a minute of clock skew) and returns its claims. A token that is
// not a JWT of this issuer gives ErrNotServiceAccountToken.
func Verify(key []byte, token string, now time.Time, audience string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != header {
		return nil, ErrNotServiceAccountToken
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return nil, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrInvalidToken
	}
	payload, err := enc.DecodeString(parts[1])
	if err != nil {
		return nil, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil || c.Issuer != Issuer {
		return nil, ErrInvalidToken
	}
	const leeway = 60
	if now.Unix() > c.Expiry+leeway || now.Unix() < c.NotBefore-leeway {
		return nil, fmt.Errorf("%w: expired or not yet valid", ErrInvalidToken)
	}
	ok := false
	for _, a := range c.Audience {
		ok = ok || a == audience
	}
	if !ok {
		return nil, fmt.Errorf("%w: wrong audience", ErrInvalidToken)
	}
	return &c, nil
}
