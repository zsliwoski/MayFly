package controller

import (
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	previewv1alpha1 "example.dev/mayfly/api/v1alpha1"
)

var envCounter atomic.Int32

// uniqueName keeps specs independent: namespaces never finish terminating in envtest.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, envCounter.Add(1))
}

func newEnvironment(name, ttl string, workloads ...previewv1alpha1.WorkloadSpec) *previewv1alpha1.Environment {
	return &previewv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       previewv1alpha1.EnvironmentSpec{TTL: ttl, Workloads: workloads},
	}
}

func webWorkload(replicas int32) previewv1alpha1.WorkloadSpec {
	return previewv1alpha1.WorkloadSpec{Name: testWorkload, Image: testImage, Port: 8080, Replicas: &replicas}
}

func getEnv(name string) *previewv1alpha1.Environment {
	env := &previewv1alpha1.Environment{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Name: name}, env)).To(Succeed())
	return env
}

// expectControlledBy checks the ownerReference directly, since envtest has no garbage collector.
func expectControlledBy(obj client.Object, env *previewv1alpha1.Environment) {
	var found *metav1.OwnerReference
	for i, ref := range obj.GetOwnerReferences() {
		if ref.UID == env.UID {
			found = &obj.GetOwnerReferences()[i]
		}
	}
	ExpectWithOffset(1, found).NotTo(BeNil(), "%T %s has no ownerReference to %s", obj, obj.GetName(), env.Name)
	ExpectWithOffset(1, found.Kind).To(Equal("Environment"))
	ExpectWithOffset(1, found.APIVersion).To(Equal(previewv1alpha1.GroupVersion.String()))
	ExpectWithOffset(1, found.Name).To(Equal(env.Name))
	ExpectWithOffset(1, found.Controller).To(HaveValue(BeTrue()))
	ExpectWithOffset(1, found.BlockOwnerDeletion).To(HaveValue(BeTrue()))
}

// markDeploymentAvailable stands in for the deployment controller and kubelet.
func markDeploymentAvailable(namespace, name string, replicas int32) {
	EventuallyWithOffset(1, func(g Gomega) {
		d := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, d)).To(Succeed())
		d.Status = appsv1.DeploymentStatus{
			ObservedGeneration: d.Generation,
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			ReadyReplicas:      replicas,
			AvailableReplicas:  replicas,
		}
		g.Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
	}).Should(Succeed())
}

// finishNamespaceTermination stands in for the namespace controller by clearing
// the "kubernetes" finalizer via the finalize subresource.
func finishNamespaceTermination(name string) {
	EventuallyWithOffset(1, func(g Gomega) {
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns); apierrors.IsNotFound(err) {
			return
		}
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)).To(Succeed())
		ns.Spec.Finalizers = nil
		g.Expect(k8sClient.SubResource("finalize").Update(ctx, ns)).To(Succeed())
	}).Should(Succeed())
}

func expectGone(obj client.Object) {
	EventuallyWithOffset(1, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj))
	}).Should(BeTrue(), "%T %s should be gone", obj, obj.GetName())
}

var _ = Describe("Environment controller", func() {
	Describe("provisioning", func() {
		var (
			name string
			ns   string
		)

		BeforeEach(func() {
			name = uniqueName("prov")
			ns = namespacePrefix + name
			env := newEnvironment(name, "1h", webWorkload(2))
			env.Spec.Labels = map[string]string{"team": testTeam}
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
		})

		It("creates an owned namespace, deployment and service", func() {
			namespace := &corev1.Namespace{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)
			}).Should(Succeed())
			env := getEnv(name)
			expectControlledBy(namespace, env)
			Expect(namespace.Labels).To(HaveKeyWithValue(labelEnvironment, name))
			Expect(namespace.Labels).To(HaveKeyWithValue("team", testTeam))

			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: testWorkload}, dep)
			}).Should(Succeed())
			expectControlledBy(dep, env)
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(testImage))

			svc := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: testWorkload}, svc)
			}).Should(Succeed())
			expectControlledBy(svc, env)
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
		})

		It("adds the finalizer and reports status", func() {
			Eventually(func(g Gomega) {
				env := getEnv(name)
				g.Expect(env.Finalizers).To(ContainElement(finalizerName))
				g.Expect(env.Status.Namespace).To(Equal(ns))
				g.Expect(env.Status.ExpiresAt).NotTo(BeNil())
				g.Expect(env.Status.ExpiresAt.Time).To(BeTemporally("~", env.CreationTimestamp.Add(time.Hour), time.Second))
				g.Expect(env.Status.ObservedGeneration).To(Equal(env.Generation))
			}).Should(Succeed())
		})

		It("goes Ready only after the deployment becomes available", func() {
			Eventually(func(g Gomega) {
				env := getEnv(name)
				ready := meta.FindStatusCondition(env.Status.Conditions, previewv1alpha1.ConditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				progressing := meta.FindStatusCondition(env.Status.Conditions, previewv1alpha1.ConditionProgressing)
				g.Expect(progressing).NotTo(BeNil())
				g.Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
			}).Should(Succeed())

			markDeploymentAvailable(ns, testWorkload, 2)

			Eventually(func(g Gomega) {
				env := getEnv(name)
				ready := meta.FindStatusCondition(env.Status.Conditions, previewv1alpha1.ConditionReady)
				g.Expect(ready).NotTo(BeNil())
				g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
				progressing := meta.FindStatusCondition(env.Status.Conditions, previewv1alpha1.ConditionProgressing)
				g.Expect(progressing.Status).To(Equal(metav1.ConditionFalse))
				degraded := meta.FindStatusCondition(env.Status.Conditions, previewv1alpha1.ConditionDegraded)
				g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
			}).Should(Succeed())
		})

		It("restores a deleted service and applies spec changes (idempotent convergence)", func() {
			svc := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: testWorkload}, svc)
			}).Should(Succeed())
			Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: testWorkload}, &corev1.Service{})
			}).Should(Succeed())

			Eventually(func(g Gomega) {
				env := getEnv(name)
				three := int32(3)
				env.Spec.Workloads[0].Replicas = &three
				g.Expect(k8sClient.Update(ctx, env)).To(Succeed())
			}).Should(Succeed())
			Eventually(func(g Gomega) {
				dep := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: testWorkload}, dep)).To(Succeed())
				g.Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
			}).Should(Succeed())
		})
	})

	Describe("validation", func() {
		It("rejects a malformed ttl", func() {
			err := k8sClient.Create(ctx, newEnvironment(uniqueName("badttl"), "soon", webWorkload(1)))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		})

		It("rejects an empty workload list", func() {
			err := k8sClient.Create(ctx, newEnvironment(uniqueName("nowl"), "1h"))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		})

		It("rejects labels under the reserved prefix", func() {
			env := newEnvironment(uniqueName("reserved"), "1h", webWorkload(1))
			env.Spec.Labels = map[string]string{"preview.example.dev/environment": "x"}
			err := k8sClient.Create(ctx, env)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		})

		It("rejects names too long to fit the namespace prefix", func() {
			long := uniqueName("a-very-long-environment-name-that-exceeds-the-limit-by-far")
			err := k8sClient.Create(ctx, newEnvironment(long, "1h", webWorkload(1)))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		})

		It("defaults replicas to 1", func() {
			name := uniqueName("defrep")
			env := newEnvironment(name, "1h", previewv1alpha1.WorkloadSpec{Name: testWorkload, Image: "nginx", Port: 80})
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			Expect(*getEnv(name).Spec.Workloads[0].Replicas).To(Equal(int32(1)))
		})
	})

	Describe("deletion", func() {
		It("waits for the namespace to disappear before releasing the finalizer", func() {
			name := uniqueName("del")
			ns := namespacePrefix + name
			Expect(k8sClient.Create(ctx, newEnvironment(name, "1h", webWorkload(1)))).To(Succeed())
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Name: ns}, &corev1.Namespace{})
			}).Should(Succeed())

			Expect(k8sClient.Delete(ctx, getEnv(name))).To(Succeed())

			Eventually(func(g Gomega) {
				namespace := &corev1.Namespace{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
				g.Expect(namespace.DeletionTimestamp).NotTo(BeNil())
			}).Should(Succeed())

			// Without a namespace controller the namespace stays Terminating, so the
			// finalizer must still be on the Environment.
			Consistently(func(g Gomega) {
				env := getEnv(name)
				g.Expect(env.DeletionTimestamp).NotTo(BeNil())
				g.Expect(env.Finalizers).To(ContainElement(finalizerName))
			}).Should(Succeed())

			finishNamespaceTermination(ns)
			expectGone(&previewv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})

		It("refuses to adopt or delete a pre-existing namespace it does not control", func() {
			name := uniqueName("clash")
			ns := namespacePrefix + name
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			Expect(k8sClient.Create(ctx, newEnvironment(name, "1h", webWorkload(1)))).To(Succeed())

			Consistently(func(g Gomega) {
				namespace := &corev1.Namespace{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
				g.Expect(namespace.OwnerReferences).To(BeEmpty())
				var deployments appsv1.DeploymentList
				g.Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
				g.Expect(deployments.Items).To(BeEmpty())
			}).Should(Succeed())

			Expect(k8sClient.Delete(ctx, getEnv(name))).To(Succeed())
			Consistently(func(g Gomega) {
				namespace := &corev1.Namespace{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
				g.Expect(namespace.DeletionTimestamp).To(BeNil())
			}).Should(Succeed())

			// Remove the finalizer so the object doesn't linger.
			env := getEnv(name)
			env.Finalizers = nil
			Expect(k8sClient.Update(ctx, env)).To(Succeed())
		})
	})

	Describe("TTL expiry", func() {
		It("deletes the environment and its namespace once the TTL elapses, without polling", func() {
			name := uniqueName("ttl")
			ns := namespacePrefix + name
			Expect(k8sClient.Create(ctx, newEnvironment(name, "4s", webWorkload(1)))).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Name: ns}, &corev1.Namespace{})
			}).Should(Succeed())
			Expect(getEnv(name).DeletionTimestamp).To(BeNil())

			// Only RequeueAfter can trigger this.
			Eventually(func(g Gomega) {
				g.Expect(getEnv(name).DeletionTimestamp).NotTo(BeNil())
				namespace := &corev1.Namespace{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, namespace)).To(Succeed())
				g.Expect(namespace.DeletionTimestamp).NotTo(BeNil())
			}, "20s").Should(Succeed())

			finishNamespaceTermination(ns)
			expectGone(&previewv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: name}})
		})
	})
})
