package controller

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	previewv1alpha1 "example.dev/mayfly/api/v1alpha1"
)

const (
	finalizerName   = "preview.example.dev/cleanup"
	namespacePrefix = "mayfly-"
	// Set on the namespace so it can be found even if ownerReferences are removed.
	labelEnvironment = "preview.example.dev/environment"
)

// EnvironmentReconciler reconciles an Environment object.
type EnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Now is injectable for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=preview.example.dev,resources=environments,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=preview.example.dev,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=preview.example.dev,resources=environments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile moves the cluster toward the Environment spec and is safe to repeat.
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var env previewv1alpha1.Environment
	if err := r.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !env.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &env)
	}

	// Add the finalizer before creating anything so the namespace can't leak.
	if controllerutil.AddFinalizer(&env, finalizerName) {
		if err := r.Update(ctx, &env); err != nil {
			return ctrl.Result{}, err
		}
	}

	expiresAt, err := computeExpiry(env.CreationTimestamp.Time, env.Spec.TTL)
	if err != nil {
		// Retrying can't fix a bad TTL; report it and wait for a spec change.
		return ctrl.Result{}, r.setDegraded(ctx, &env, "InvalidTTL", err.Error())
	}
	remaining, expired := timeUntil(expiresAt, r.Now())
	if expired {
		log.Info("environment expired, deleting", "name", env.Name)
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &env))
	}

	if err := r.ensureNamespace(ctx, &env); err != nil {
		return ctrl.Result{}, err
	}
	for i := range env.Spec.Workloads {
		w := &env.Spec.Workloads[i]
		if err := r.ensureDeployment(ctx, &env, w); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.ensureService(ctx, &env, w); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Deployment changes re-trigger us through Owns(), so the only timer is expiry.
	if err := r.updateStatus(ctx, &env, expiresAt); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: remaining}, nil
}

// reconcileDelete deletes the namespace and holds the finalizer until it is gone.
// The namespace delete event re-triggers us, so there is no polling.
func (r *EnvironmentReconciler) reconcileDelete(ctx context.Context, env *previewv1alpha1.Environment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(env, finalizerName) {
		return ctrl.Result{}, nil
	}

	var ns corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: namespaceName(env)}, &ns)
	switch {
	case apierrors.IsNotFound(err):
		// Gone: safe to release.
		controllerutil.RemoveFinalizer(env, finalizerName)
		if err := r.Update(ctx, env); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	// Never delete a namespace we don't control, even if the name collides.
	if !metav1.IsControlledBy(&ns, env) {
		return ctrl.Result{}, fmt.Errorf("namespace %q exists but is not controlled by environment %q; refusing to delete", ns.Name, env.Name)
	}
	if ns.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	// Still terminating; the namespace delete event will wake us.
	return ctrl.Result{}, nil
}

// ensureNamespace creates the namespace or updates its labels. The Environment and
// Namespace are both cluster-scoped, so a real ownerReference is allowed.
func (r *EnvironmentReconciler) ensureNamespace(ctx context.Context, env *previewv1alpha1.Environment) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName(env)}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		if !ns.CreationTimestamp.IsZero() && !metav1.IsControlledBy(ns, env) {
			return fmt.Errorf("namespace %q exists but is not controlled by environment %q", ns.Name, env.Name)
		}
		// Merge so labels added by the API server (kubernetes.io/metadata.name) survive.
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		maps.Copy(ns.Labels, commonLabels(env))
		return controllerutil.SetControllerReference(env, ns, r.Scheme)
	})
	return err
}

func (r *EnvironmentReconciler) ensureDeployment(ctx context.Context, env *previewv1alpha1.Environment, w *previewv1alpha1.WorkloadSpec) error {
	desired := deploymentFor(env, w)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, d, func() error {
		// Selector is immutable, so only set it on create.
		if d.CreationTimestamp.IsZero() {
			d.Spec.Selector = desired.Spec.Selector
		}
		d.Labels = desired.Labels
		d.Spec.Replicas = desired.Spec.Replicas
		d.Spec.Template.Labels = desired.Spec.Template.Labels
		// Leave server-defaulted container fields alone to avoid update loops.
		if len(d.Spec.Template.Spec.Containers) == 0 {
			d.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
		} else {
			c := &d.Spec.Template.Spec.Containers[0]
			c.Name = desired.Spec.Template.Spec.Containers[0].Name
			c.Image = desired.Spec.Template.Spec.Containers[0].Image
			c.Ports = desired.Spec.Template.Spec.Containers[0].Ports
		}
		return controllerutil.SetControllerReference(env, d, r.Scheme)
	})
	return err
}

func (r *EnvironmentReconciler) ensureService(ctx context.Context, env *previewv1alpha1.Environment, w *previewv1alpha1.WorkloadSpec) error {
	desired := serviceFor(env, w)
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, s, func() error {
		// Don't touch ClusterIP or other allocated fields.
		s.Labels = desired.Labels
		s.Spec.Selector = desired.Spec.Selector
		s.Spec.Ports = desired.Spec.Ports
		return controllerutil.SetControllerReference(env, s, r.Scheme)
	})
	return err
}

// updateStatus derives conditions from the observed Deployments. It uses a merge
// patch so it can't conflict with spec or finalizer updates.
func (r *EnvironmentReconciler) updateStatus(ctx context.Context, env *previewv1alpha1.Environment, expiresAt time.Time) error {
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(namespaceName(env))); err != nil {
		return err
	}
	byName := make(map[string]*appsv1.Deployment, len(deployments.Items))
	for i := range deployments.Items {
		byName[deployments.Items[i].Name] = &deployments.Items[i]
	}

	var notReady []string
	for i := range env.Spec.Workloads {
		w := &env.Spec.Workloads[i]
		if d, ok := byName[w.Name]; !ok || !deploymentReady(d, desiredReplicas(w)) {
			notReady = append(notReady, w.Name)
		}
	}

	base := env.DeepCopy()
	gen := env.Generation
	set := func(t string, s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
			Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen,
		})
	}
	if len(notReady) == 0 {
		set(previewv1alpha1.ConditionReady, metav1.ConditionTrue, "AllWorkloadsAvailable", "all workloads are available")
		set(previewv1alpha1.ConditionProgressing, metav1.ConditionFalse, "Complete", "reconcile complete")
	} else {
		msg := fmt.Sprintf("waiting for workloads: %s", strings.Join(notReady, ", "))
		set(previewv1alpha1.ConditionReady, metav1.ConditionFalse, "WorkloadsNotAvailable", msg)
		set(previewv1alpha1.ConditionProgressing, metav1.ConditionTrue, "WaitingForWorkloads", msg)
	}
	// Clears a previous InvalidTTL once the spec is fixed.
	set(previewv1alpha1.ConditionDegraded, metav1.ConditionFalse, "AsExpected", "no problems detected")

	t := metav1.NewTime(expiresAt)
	env.Status.ExpiresAt = &t
	env.Status.Namespace = namespaceName(env)
	env.Status.ObservedGeneration = gen
	return r.Status().Patch(ctx, env, client.MergeFrom(base))
}

func (r *EnvironmentReconciler) setDegraded(ctx context.Context, env *previewv1alpha1.Environment, reason, msg string) error {
	base := env.DeepCopy()
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               previewv1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: env.Generation,
	})
	env.Status.ObservedGeneration = env.Generation
	return r.Status().Patch(ctx, env, client.MergeFrom(base))
}

// SetupWithManager registers the controller and its watches.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&previewv1alpha1.Environment{}).
		Owns(&corev1.Namespace{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("environment").
		Complete(r)
}
