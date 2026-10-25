package controller

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	previewv1alpha1 "example.dev/mayfly/api/v1alpha1"
)

const (
	testImage = "nginx:1.27"
	testTeam  = "payments"
)

func TestComputeExpiry(t *testing.T) {
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		ttl     string
		want    time.Time
		wantErr bool
	}{
		{"minutes", "30m", created.Add(30 * time.Minute), false},
		{"compound", "1h30m", created.Add(90 * time.Minute), false},
		{"fractional", "1.5h", created.Add(90 * time.Minute), false},
		{"zero", "0s", time.Time{}, true},
		{"negative", "-5m", time.Time{}, true},
		{"garbage", "soon", time.Time{}, true},
		{"empty", "", time.Time{}, true},
		{"missing unit", "30", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := computeExpiry(created, tc.ttl)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !got.Equal(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTimeUntil(t *testing.T) {
	exp := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		now         time.Time
		wantRemain  time.Duration
		wantExpired bool
	}{
		{"before", exp.Add(-10 * time.Minute), 10 * time.Minute, false},
		{"exactly at expiry", exp, 0, true},
		{"after", exp.Add(time.Second), -time.Second, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			remain, expired := timeUntil(exp, tc.now)
			if remain != tc.wantRemain || expired != tc.wantExpired {
				t.Errorf("got (%v, %v), want (%v, %v)", remain, expired, tc.wantRemain, tc.wantExpired)
			}
		})
	}
}

func newTestEnvironment() (*previewv1alpha1.Environment, *previewv1alpha1.WorkloadSpec) {
	two := int32(2)
	w := previewv1alpha1.WorkloadSpec{Name: "web", Image: testImage, Port: 8080, Replicas: &two}
	env := &previewv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-42"},
		Spec: previewv1alpha1.EnvironmentSpec{
			TTL:       "1h",
			Workloads: []previewv1alpha1.WorkloadSpec{w},
			Labels:    map[string]string{"team": testTeam, labelEnvironment: "hijack"},
		},
	}
	return env, &env.Spec.Workloads[0]
}

func TestDeploymentFor(t *testing.T) {
	env, w := newTestEnvironment()
	d := deploymentFor(env, w)

	if d.Namespace != "mayfly-pr-42" || d.Name != "web" {
		t.Errorf("unexpected identity %s/%s", d.Namespace, d.Name)
	}
	if *d.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *d.Spec.Replicas)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != testImage || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("bad container: %+v", c)
	}
	// Selector must match pod template labels or the API server rejects the Deployment.
	for k, v := range d.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("selector label %s=%s not in pod template", k, v)
		}
	}
	if d.Labels["team"] != testTeam {
		t.Error("user label not propagated")
	}
	if d.Labels[labelEnvironment] != "pr-42" {
		t.Errorf("user label overrode reserved label: %q", d.Labels[labelEnvironment])
	}
}

func TestDeploymentForDefaultReplicas(t *testing.T) {
	env, w := newTestEnvironment()
	w.Replicas = nil
	if got := *deploymentFor(env, w).Spec.Replicas; got != 1 {
		t.Errorf("replicas = %d, want 1", got)
	}
}

func TestServiceFor(t *testing.T) {
	env, w := newTestEnvironment()
	s := serviceFor(env, w)
	if s.Namespace != "mayfly-pr-42" || s.Spec.Ports[0].Port != 8080 || s.Spec.Ports[0].TargetPort.IntVal != 8080 {
		t.Errorf("unexpected service: %+v", s.Spec)
	}
	d := deploymentFor(env, w)
	for k, v := range s.Spec.Selector {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("service selector %s=%s does not match pods", k, v)
		}
	}
}

func TestDeploymentReady(t *testing.T) {
	mk := func(gen, obs int64, updated, avail int32) *appsv1.Deployment {
		d := &appsv1.Deployment{}
		d.Generation = gen
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: obs, UpdatedReplicas: updated, AvailableReplicas: avail}
		return d
	}
	tests := []struct {
		name string
		d    *appsv1.Deployment
		want int32
		ok   bool
	}{
		{"ready", mk(1, 1, 2, 2), 2, true},
		{"stale generation", mk(2, 1, 2, 2), 2, false},
		{"not available", mk(1, 1, 2, 1), 2, false},
		{"rolling", mk(1, 1, 1, 2), 2, false},
		{"scaled to zero", mk(1, 1, 0, 0), 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := deploymentReady(tc.d, tc.want); got != tc.ok {
				t.Errorf("got %v, want %v", got, tc.ok)
			}
		})
	}
}
