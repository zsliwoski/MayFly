package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	previewv1alpha1 "example.dev/mayfly/api/v1alpha1"
)

const (
	labelWorkload  = "preview.example.dev/workload"
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "mayfly"
	portName       = "http"
)

// namespaceName is the namespace an Environment owns.
func namespaceName(env *previewv1alpha1.Environment) string {
	return namespacePrefix + env.Name
}

// commonLabels adds the controller's labels after the user's so they can't be overridden.
// The CRD also rejects reserved keys.
func commonLabels(env *previewv1alpha1.Environment) map[string]string {
	l := make(map[string]string, len(env.Spec.Labels)+2)
	for k, v := range env.Spec.Labels {
		l[k] = v
	}
	l[labelEnvironment] = env.Name
	l[labelManagedBy] = managedByValue
	return l
}

// selectorLabels must not change after creation; Deployment selectors are immutable.
func selectorLabels(env *previewv1alpha1.Environment, w *previewv1alpha1.WorkloadSpec) map[string]string {
	return map[string]string{
		labelEnvironment: env.Name,
		labelWorkload:    w.Name,
	}
}

func workloadLabels(env *previewv1alpha1.Environment, w *previewv1alpha1.WorkloadSpec) map[string]string {
	l := commonLabels(env)
	l[labelWorkload] = w.Name
	return l
}

func desiredReplicas(w *previewv1alpha1.WorkloadSpec) int32 {
	if w.Replicas == nil {
		return 1
	}
	return *w.Replicas
}

// deploymentFor builds the desired Deployment for a workload. It has no owner reference.
func deploymentFor(env *previewv1alpha1.Environment, w *previewv1alpha1.WorkloadSpec) *appsv1.Deployment {
	replicas := desiredReplicas(w)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.Name,
			Namespace: namespaceName(env),
			Labels:    workloadLabels(env, w),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(env, w)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: workloadLabels(env, w)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  w.Name,
						Image: w.Image,
						Ports: []corev1.ContainerPort{{Name: portName, ContainerPort: w.Port, Protocol: corev1.ProtocolTCP}},
					}},
				},
			},
		},
	}
}

// serviceFor maps a workload to its ClusterIP Service.
func serviceFor(env *previewv1alpha1.Environment, w *previewv1alpha1.WorkloadSpec) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.Name,
			Namespace: namespaceName(env),
			Labels:    workloadLabels(env, w),
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels(env, w),
			Ports: []corev1.ServicePort{{
				Name:       portName,
				Port:       w.Port,
				TargetPort: intstr.FromInt32(w.Port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// deploymentReady is true when the controller has seen the latest spec and all
// desired replicas are updated and available.
func deploymentReady(d *appsv1.Deployment, want int32) bool {
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas == want &&
		d.Status.AvailableReplicas == want
}
