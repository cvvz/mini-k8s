package token

import (
	"context"
	"net/http"
	"testing"
	"time"

	authenticationv1 "mini-k8s/pkg/apis/authentication/v1"
	corev1 "mini-k8s/pkg/apis/core/v1"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/client"
	"mini-k8s/test/integration/framework"
)

func TestManagerCachesAndRefreshes(t *testing.T) {
	c := framework.StartTestServer(t)
	ctx := context.Background()
	pod, err := client.Pods(c, "default").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(c)
	now := time.Now()
	m.now = func() time.Time { return now }
	first, err := m.ServiceAccountToken(pod, "", 3607)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := m.ServiceAccountToken(pod, "", 3607); again != first {
		t.Fatal("a fresh token is reused")
	}
	// After 80% of its hour, the token is renewed.
	now = now.Add(50 * time.Minute)
	renewed, err := m.ServiceAccountToken(pod, "", 3607)
	if err != nil || renewed == first {
		t.Fatalf("renewal: %v (same token: %v)", err, renewed == first)
	}

	// The token is the Pod's service account.
	var who authenticationv1.SelfSubjectReview
	if err := client.New(c.BaseURL(), renewed).Do(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews", nil, "",
		&who, &who); err != nil || who.Status.UserInfo.Username != "system:serviceaccount:default:default" {
		t.Fatalf("token identity: %+v %v", who.Status, err)
	}
}
