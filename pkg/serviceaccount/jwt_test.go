package serviceaccount

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1792048598, 0)
	private := PrivateClaims{Namespace: "ns", ServiceAccount: Ref{Name: "robot", UID: "sa-uid"}, Pod: &Ref{Name: "web", UID: "pod-uid"}}
	token, err := Sign(key, NewClaims(now, time.Hour, []string{Issuer}, private))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(token, ".") != 2 {
		t.Fatalf("not a JWT: %s", token)
	}
	c, err := Verify(key, token, now.Add(time.Minute), Issuer)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "system:serviceaccount:ns:robot" || c.Kubernetes.Pod.UID != "pod-uid" || c.Expiry != now.Add(time.Hour).Unix() ||
		c.ID == "" {
		t.Fatalf("claims: %+v", c)
	}
	for name, tc := range map[string]struct {
		token    string
		key      []byte
		at       time.Time
		audience string
	}{
		"other key":      {token, []byte("another key"), now, Issuer},
		"expired":        {token, key, now.Add(2 * time.Hour), Issuer},
		"wrong audience": {token, key, now, "vault"},
		"tampered":       {token[:len(token)-2] + "xx", key, now, Issuer},
	} {
		if _, err := Verify(tc.key, tc.token, tc.at, tc.audience); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Verify(key, "not-a-jwt", now, Issuer); !errors.Is(err, ErrNotServiceAccountToken) {
		t.Fatalf("opaque token: %v", err)
	}
}

func TestUsernames(t *testing.T) {
	if MakeUsername("ns", "robot") != "system:serviceaccount:ns:robot" {
		t.Fatal("MakeUsername")
	}
	for name, want := range map[string][2]string{
		"system:serviceaccount:ns:robot": {"ns", "robot"},
		"system:serviceaccount:ns":       {},
		"alice":                          {},
		"system:serviceaccount:ns:a:b":   {},
	} {
		ns, n, ok := SplitUsername(name)
		if ok != (want[0] != "") || ns != want[0] || n != want[1] {
			t.Errorf("%s: %q %q %v", name, ns, n, ok)
		}
	}
	u := UserInfo("ns", "robot", "uid")
	if u.Name != "system:serviceaccount:ns:robot" || strings.Join(u.Groups, ",") != "system:serviceaccounts,system:serviceaccounts:ns,system:authenticated" {
		t.Fatalf("user: %+v", u)
	}
}
