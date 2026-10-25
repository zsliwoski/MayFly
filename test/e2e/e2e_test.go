//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"example.dev/mayfly/test/utils"
)

// operatorNamespace is where `make deploy` installs the manager (kustomize namespace).
const operatorNamespace = "mayfly-system"

const environmentManifest = `apiVersion: preview.example.dev/v1alpha1
kind: Environment
metadata:
  name: %s
spec:
  ttl: %s
  labels:
    team: platform
  workloads:
  - name: web
    image: nginxinc/nginx-unprivileged:1.27-alpine
    port: 8080
    replicas: 1
`

// kubectl runs kubectl and returns trimmed output. stdin is optional.
func kubectl(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := utils.Run(cmd)
	return strings.TrimSpace(out), err
}

func kubectlGet(args ...string) func() (string, error) {
	return func() (string, error) { return kubectl("", append([]string{"get"}, args...)...) }
}

func applyEnvironment(name, ttl string) {
	_, err := kubectl(fmt.Sprintf(environmentManifest, name, ttl), "apply", "-f", "-")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

var _ = Describe("mayfly", Ordered, func() {
	BeforeAll(func() {
		By("creating the operator namespace")
		_, err := kubectl("", "create", "ns", operatorNamespace)
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectl("", "label", "--overwrite", "ns", operatorNamespace, "pod-security.kubernetes.io/enforce=restricted")
		Expect(err).NotTo(HaveOccurred())

		By("installing CRDs and deploying the operator")
		_, err = utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage)))
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the operator to be available")
		_, err = kubectl("", "wait", "deployment/mayfly-controller-manager", "-n", operatorNamespace,
			"--for=condition=Available", "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, _ = kubectl("", "delete", "environments.preview.example.dev", "--all", "--wait=false")
		_, _ = utils.Run(exec.Command("make", "undeploy", "ignore-not-found=true"))
		_, _ = utils.Run(exec.Command("make", "uninstall", "ignore-not-found=true"))
		_, _ = kubectl("", "delete", "ns", operatorNamespace, "--ignore-not-found")
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			logs, _ := kubectl("", "logs", "-n", operatorNamespace, "-l", "control-plane=controller-manager", "--tail=200")
			_, _ = fmt.Fprintf(GinkgoWriter, "operator logs:\n%s\n", logs)
			envs, _ := kubectl("", "get", "environments.preview.example.dev", "-o", "yaml")
			_, _ = fmt.Fprintf(GinkgoWriter, "environments:\n%s\n", envs)
		}
	})

	Context("Environment lifecycle", func() {
		const name = "e2e-basic"
		const ns = "mayfly-" + name

		It("provisions an owned namespace with a running workload and reports Ready", func() {
			applyEnvironment(name, "1h")

			By("waiting for Ready=True (needs real image pull and pod startup)")
			Eventually(kubectlGet("environment", name, "-o",
				`jsonpath={.status.conditions[?(@.type=="Ready")].status}`), 4*time.Minute, 2*time.Second).
				Should(Equal("True"))

			By("checking the namespace is owned by the Environment and labelled")
			Expect(kubectlGet("ns", ns, "-o", "jsonpath={.metadata.ownerReferences[0].kind}")()).To(Equal("Environment"))
			Expect(kubectlGet("ns", ns, "-o", "jsonpath={.metadata.labels.team}")()).To(Equal("platform"))

			By("checking the workload")
			Expect(kubectlGet("deployment", "web", "-n", ns, "-o", "jsonpath={.status.availableReplicas}")()).To(Equal("1"))
			Expect(kubectlGet("service", "web", "-n", ns, "-o", "jsonpath={.spec.ports[0].port}")()).To(Equal("8080"))
			Expect(kubectlGet("deployment", "web", "-n", ns, "-o", "jsonpath={.metadata.ownerReferences[0].name}")()).To(Equal(name))
		})

		It("removes the namespace and releases the finalizer when the Environment is deleted", func() {
			_, err := kubectl("", "delete", "environment", name, "--wait=true", "--timeout=3m")
			Expect(err).NotTo(HaveOccurred())

			// A real namespace controller exists here, unlike envtest.
			Eventually(kubectlGet("ns", ns, "--ignore-not-found", "-o", "name"), 3*time.Minute, 2*time.Second).
				Should(BeEmpty())
		})
	})

	Context("TTL expiry", func() {
		const name = "e2e-ttl"
		const ns = "mayfly-" + name

		It("deletes the Environment and its namespace after the TTL, with no manual action", func() {
			applyEnvironment(name, "60s")

			Eventually(kubectlGet("ns", ns, "-o", "name"), time.Minute, 2*time.Second).Should(Equal("namespace/" + ns))

			By("waiting for the operator to expire the Environment")
			Eventually(kubectlGet("environment", name, "--ignore-not-found", "-o", "name"), 3*time.Minute, 2*time.Second).
				Should(BeEmpty())
			Eventually(kubectlGet("ns", ns, "--ignore-not-found", "-o", "name"), 3*time.Minute, 2*time.Second).
				Should(BeEmpty())
		})
	})

	Context("admission validation", func() {
		It("rejects an invalid TTL at the API server", func() {
			out, err := kubectl(fmt.Sprintf(environmentManifest, "e2e-bad", "soon"), "apply", "-f", "-")
			Expect(err).To(HaveOccurred())
			Expect(out).To(ContainSubstring("ttl"))
		})
	})
})
