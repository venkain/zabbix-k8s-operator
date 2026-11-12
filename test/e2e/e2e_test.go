//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/venkain/zabbix-k8s-operator/test/utils"
)

// namespaceFleet is the namespace the monitored stack (Zabbix instance
// and its CloudNativePG database) lives in; the operator itself runs
// in the operator-system namespace from config/default.
const namespaceFleet = "monitoring"

// operatorNamespace is where `make deploy` installs the manager.
const operatorNamespace = "zabbix-k8s-operator-system"

// cnpgRelease is the CloudNativePG consolidated install manifest. The
// version matches the one used in the development lab so the suite
// exercises the same operator versions end to end.
const cnpgRelease = "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml"

// pgCluster is the CloudNativePG Cluster the Zabbix instance uses: a
// single-instance database bootstrapped exactly the way the sample
// CR expects (database + owner "zabbix"), which makes CNPG publish
// the zabbix-db-app Secret the operator resolves the stack from.
const pgCluster = `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: zabbix-db
  namespace: ` + namespaceFleet + `
spec:
  instances: 1
  bootstrap:
    initdb:
      database: zabbix
      owner: zabbix
  storage:
    size: 5Gi
`

// zabbixInstance is the stack under test: server + frontend +
// web service in CNPG database mode, single replica per component.
const zabbixInstance = `apiVersion: zabbix.venkain.io/v1alpha1
kind: Zabbix
metadata:
  name: zbx
  namespace: ` + namespaceFleet + `
spec:
  version: "8.0"
  database:
    cnpg:
      clusterName: zabbix-db
  server:
    replicas: 1
    agent:
      enabled: true
  frontend:
    replicas: 1
  webService:
    replicas: 1
`

// kubeContext pins every kubectl call to the kind cluster the Makefile
// created for the suite (kind create cluster switches the kubeconfig
// current-context as a side effect; a stale kubeconfig otherwise
// silently points the suite at a different cluster). Empty means
// unpinned (no KIND_CLUSTER env — not running under the kind target).
var kubeContext = func() string {
	cluster := os.Getenv("KIND_CLUSTER")
	if cluster == "" {
		return ""
	}
	out, err := exec.Command("kubectl", "config", "get-contexts", "-o", "name").Output()
	if err != nil {
		return ""
	}
	for _, ctx := range strings.Fields(string(out)) {
		if ctx == "kind-"+cluster {
			return ctx
		}
	}
	return ""
}()

// runKubectl executes kubectl in the test namespace (or cluster-wide
// when namespace is empty), pinned to the suite's kind context.
func runKubectl(namespace string, args ...string) (string, error) {
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
	if namespace != "" {
		args = append([]string{"-n", namespace}, args...)
	}
	cmd := exec.Command("kubectl", args...)
	return utils.Run(cmd)
}

// kubectlApplyString applies an embedded manifest through a temp file
// (utils.Run has no stdin channel).
func kubectlApplyString(manifest string) error {
	file := filepath.Join(os.TempDir(), fmt.Sprintf("zbx-e2e-%d.yaml", time.Now().UnixNano()))
	if err := os.WriteFile(file, []byte(manifest), 0o600); err != nil {
		return err
	}
	defer os.Remove(file)
	_, err := runKubectl("", "apply", "-f", file)
	return err
}

// kubectlJSONPath runs kubectl -o jsonpath and trims the output.
func kubectlJSONPath(namespace, resource, query string) (string, error) {
	out, err := runKubectl(namespace, "get", resource, "-o", "jsonpath="+query)
	return strings.TrimSpace(out), err
}

var _ = Describe("Zabbix stack", Ordered, func() {

	BeforeAll(func() {
		By("verifying the kubectl context targets the suite's kind cluster")
		current, err := exec.Command("kubectl", "config", "current-context").Output()
		Expect(err).NotTo(HaveOccurred())
		currentContext := strings.TrimSpace(string(current))
		if kubeContext != "" {
			Expect(currentContext).To(Equal(kubeContext),
				"kubectl current-context is %q but the suite targets %q — make targets like `make deploy` run against the current context; switch with `kubectl config use-context %s` first",
				currentContext, kubeContext, kubeContext)
		}

		By("creating the fleet namespace")
		_, err = runKubectl("", "create", "ns", namespaceFleet)
		Expect(err).NotTo(HaveOccurred(), "failed to create the fleet namespace")

		By("installing the operator CRDs")
		_, err = utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred(), "failed to install CRDs")

		By("deploying the controller-manager")
		_, err = utils.Run(exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage)))
		Expect(err).NotTo(HaveOccurred(), "failed to deploy the controller-manager")
		_, err = runKubectl(operatorNamespace, "rollout", "status",
			"deployment/zabbix-k8s-operator-controller-manager", "--timeout=180s")
		Expect(err).NotTo(HaveOccurred(), "controller-manager did not roll out")

		By("installing CloudNativePG")
		_, err = runKubectl("", "apply", "--server-side", "-f", cnpgRelease)
		Expect(err).NotTo(HaveOccurred(), "failed to install CloudNativePG")
		_, err = runKubectl("cnpg-system", "rollout", "status",
			"deployment/cnpg-controller-manager", "--timeout=180s")
		Expect(err).NotTo(HaveOccurred(), "cnpg controller-manager did not roll out")

		By("creating the CloudNativePG database cluster")
		Expect(kubectlApplyString(pgCluster)).To(Succeed())
		_, err = runKubectl(namespaceFleet, "wait", "--for=condition=Ready",
			"clusters.postgresql.cnpg.io/zabbix-db", "--timeout=600s")
		Expect(err).NotTo(HaveOccurred(), "the database cluster never became ready")
	})

	AfterAll(func() {
		By("removing the Zabbix instance")
		_, _ = runKubectl(namespaceFleet, "delete", "zabbix", "zbx", "--wait=false")
		// A failed spec can leave the cluster-metrics cleanup
		// finalizer on the resource while the manager is about to be
		// undeployed — nothing would ever release it, and the CRD
		// deletion below would hang forever. Force-release any
		// finalizer left on the resource (best effort).
		_, _ = runKubectl(namespaceFleet, "patch", "zabbix", "zbx", "--type", "json", "-p",
			`[{"op":"replace","path":"/metadata/finalizers","value":[]}]`)

		By("removing the curl-metrics probe pod")
		_, _ = runKubectl(operatorNamespace, "delete", "pod", "curl-metrics",
			"--force", "--grace-period=0", "--ignore-not-found")

		By("removing the metrics reader binding")
		_, _ = runKubectl("", "delete", "clusterrolebinding", "e2e-metrics-reader",
			"--ignore-not-found")

		By("undeploying the controller-manager")
		_, _ = utils.Run(exec.Command("make", "undeploy"))

		By("uninstalling the operator CRDs")
		_, _ = utils.Run(exec.Command("make", "uninstall"))

		By("removing the fleet namespace (with the database cluster)")
		_, _ = runKubectl("", "delete", "ns", namespaceFleet, "--wait=false")

		By("removing CloudNativePG")
		_, _ = runKubectl("", "delete", "-f", cnpgRelease, "--ignore-not-found")
	})

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if !specReport.Failed() {
			return
		}
		By("collecting the controller-manager logs")
		out, err := runKubectl(operatorNamespace, "get", "pods",
			"-l", "control-plane=controller-manager", "-o", "name")
		if err == nil {
			name := strings.TrimSpace(strings.TrimPrefix(out, "pod/"))
			if logs, lerr := runKubectl(operatorNamespace, "logs", name, "--tail=100"); lerr == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "controller logs:\n%s", logs)
			}
		}
		By("collecting the fleet events")
		if events, err := runKubectl(namespaceFleet, "get", "events", "--sort-by=.lastTimestamp"); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "fleet events:\n%s", events)
		}
	})

	// assertCondition waits for one condition of the instance to be
	// True (conditions carry observedGeneration semantics from the
	// status subresource).
	assertCondition := func(condition string) {
		_, err := runKubectl(namespaceFleet, "wait", "--for=condition="+condition,
			"zabbix/zbx", "--timeout=600s")
		Expect(err).NotTo(HaveOccurred(), "condition %s never became True", condition)
	}

	// assertRollout waits for one owned workload to reach its desired
	// ready replicas.
	assertRollout := func(resource, name string) {
		_, err := runKubectl(namespaceFleet, "rollout", "status", resource, name, "--timeout=600s")
		Expect(err).NotTo(HaveOccurred(), "%s %s did not roll out", resource, name)
	}

	It("brings up the whole stack and reports it through status and conditions", func() {
		By("declaring the Zabbix instance")
		Expect(kubectlApplyString(zabbixInstance)).To(Succeed())

		By("waiting for the instance conditions")
		assertCondition("DatabaseReady")
		assertCondition("Ready")

		By("waiting for every owned workload to roll out")
		assertRollout("statefulset", "zbx-server")
		assertRollout("deployment", "zbx-frontend")
		assertRollout("deployment", "zbx-web-service")

		By("checking the observedGeneration bookkeeping")
		generation, err := kubectlJSONPath(namespaceFleet, "zabbix/zbx", "{.metadata.generation}")
		Expect(err).NotTo(HaveOccurred())
		observed, err := kubectlJSONPath(namespaceFleet, "zabbix/zbx", "{.status.observedGeneration}")
		Expect(err).NotTo(HaveOccurred())
		Expect(observed).To(Equal(generation))
	})

	It("mirrors a disruption budget on every owned workload", func() {
		for _, name := range []string{"zbx-server", "zbx-frontend", "zbx-web-service"} {
			maxUnavailable, err := kubectlJSONPath(namespaceFleet, "pdb/"+name,
				"{.spec.maxUnavailable}")
			Expect(err).NotTo(HaveOccurred(), "pdb %s missing", name)
			Expect(maxUnavailable).To(Equal("1"),
				"pdb %s: drains must be able to take one pod at a time", name)

			disruptionsAllowed, err := kubectlJSONPath(namespaceFleet, "pdb/"+name,
				"{.status.disruptionsAllowed}")
			Expect(err).NotTo(HaveOccurred())
			Expect(disruptionsAllowed).NotTo(Equal("0"),
				"pdb %s must admit at least one voluntary disruption", name)
		}
	})

	It("wires the database through the CloudNativePG app secret", func() {
		host, err := kubectlJSONPath(namespaceFleet, "secret/zabbix-db-app", "{.data.host}")
		Expect(err).NotTo(HaveOccurred(), "the CNPG app secret is missing")
		Expect(host).NotTo(BeEmpty())
		dbName, err := kubectlJSONPath(namespaceFleet, "secret/zabbix-db-app", "{.data.dbname}")
		Expect(err).NotTo(HaveOccurred())
		Expect(dbName).To(Equal(
			base64.StdEncoding.EncodeToString([]byte("zabbix"))))

		By("checking the server container carries the resolved database env")
		env, err := runKubectl(namespaceFleet, "get", "sts/zbx-server",
			"-o", "jsonpath={.spec.template.spec.containers[0].env}")
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(ContainSubstring("DB_SERVER_HOST"),
			"the server must point at the resolved database")
	})

	It("exposes the secure metrics endpoint", func() {
		By("checking the metrics service resolves the manager pods")
		endpoints, err := kubectlJSONPath(operatorNamespace,
			"endpoints/zabbix-k8s-operator-controller-manager-metrics-service",
			"{.subsets[0].addresses[0].ip}")
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoints).NotTo(BeEmpty(),
			"the metrics service has no backing endpoints")

		By("running a scraper pod with the manager's service account")
		// The scrape identity needs the bundled metrics-reader role
		// (TokenReview/SubjectAccessReview-backed endpoint). Use the
		// manager's own SA for the probe; a real scraper binds
		// metrics-reader to its own account instead.
		Expect(kubectlApplyString(`apiVersion: v1
kind: Pod
metadata:
  name: curl-metrics
  namespace: ` + operatorNamespace + `
spec:
  serviceAccountName: zabbix-k8s-operator-controller-manager
  restartPolicy: Never
  containers:
  - name: curl
    image: curlimages/curl:latest
    command: ["sleep", "180"]
`)).To(Succeed())
		_, err = runKubectl(operatorNamespace, "wait", "--for=condition=Ready",
			"pod/curl-metrics", "--timeout=120s")
		Expect(err).NotTo(HaveOccurred(), "the curl-metrics pod never became ready")

		token, err := runKubectl(operatorNamespace, "create", "token",
			"zabbix-k8s-operator-controller-manager")
		Expect(err).NotTo(HaveOccurred())
		token = strings.TrimSpace(token)

		By("checking an unauthorized token is rejected (authz enforced)")
		out, err := runKubectl(operatorNamespace, "exec", "curl-metrics", "--",
			"curl", "-sk", "-H", "Authorization: Bearer "+token,
			"-o", "/dev/null", "-w", "%{http_code}",
			"https://zabbix-k8s-operator-controller-manager-metrics-service:8443/metrics")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("403"),
			"the metrics endpoint must reject tokens without the metrics-reader role")

		By("binding the bundled metrics-reader role to the scraper")
		_, err = runKubectl("", "create", "clusterrolebinding", "e2e-metrics-reader",
			"--clusterrole=zabbix-k8s-operator-metrics-reader",
			"--serviceaccount="+operatorNamespace+":zabbix-k8s-operator-controller-manager")
		Expect(err).NotTo(HaveOccurred())

		By("scraping the metrics endpoint over HTTPS with the token")
		// The authz filter caches denied SubjectAccessReview results
		// for 30s (DenyCacheTTL in controller-runtime's metrics
		// filter); retry past the cache lifetime until the fresh
		// authorization takes effect.
		Eventually(func(g Gomega) string {
			out, err := runKubectl(operatorNamespace, "exec", "curl-metrics", "--",
				"curl", "-sk", "-H", "Authorization: Bearer "+token,
				"-o", "/dev/null", "-w", "%{http_code}",
				"https://zabbix-k8s-operator-controller-manager-metrics-service:8443/metrics")
			g.Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(out)
		}, "45s", "3s").Should(Equal("200"),
			"the metrics endpoint must admit a token-scoped scraper")
	})

	// k8s-side cluster-metrics coverage only: the Zabbix-side host
	// registration is lab-verified (docs/requirements.md, OQ5). Here a
	// dummy API token makes the operator reach the REAL frontend API
	// and park on HostRegistrationFailed — the parked state is itself
	// an assertion that the manager's API wiring (endpoint, bearer
	// header) resolves end to end, and the precondition for every
	// k8s-side object (endpoint, monitor identity, RBAC, finalizer) is
	// exercised on a real cluster, not just envtest.
	clusterMetricsReason := func(g Gomega) string {
		reason, err := kubectlJSONPath(namespaceFleet, "zabbix/zbx",
			`{.status.conditions[?(@.type=="ClusterMetricsReady")].reason}`)
		g.Expect(err).NotTo(HaveOccurred())
		return reason
	}

	It("provisions the cluster-metrics ingestion objects", func() {
		By("creating a dummy Zabbix API token secret")
		Expect(kubectlApplyString(`apiVersion: v1
kind: Secret
metadata:
  name: zbx-api-token
  namespace: ` + namespaceFleet + `
data:
  token: ` + base64.StdEncoding.EncodeToString([]byte("e2e-dummy-token")) + `
`)).To(Succeed())

		By("enabling clusterMetrics on the instance")
		Expect(kubectlApplyString(`apiVersion: zabbix.venkain.io/v1alpha1
kind: Zabbix
metadata:
  name: zbx
  namespace: ` + namespaceFleet + `
spec:
  version: "8.0"
  database:
    cnpg:
      clusterName: zabbix-db
  api:
    tokenSecretRef:
      name: zbx-api-token
      key: token
  clusterMetrics:
    clusterName: e2e-kind-cluster
    tlsVerify: none
  server:
    replicas: 1
    agent:
      enabled: true
  frontend:
    replicas: 1
  webService:
    replicas: 1
`)).To(Succeed())

		By("expecting the cleanup finalizer on the resource")
		finalizers, err := kubectlJSONPath(namespaceFleet, "zabbix/zbx",
			"{.metadata.finalizers}")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalizers).To(ContainSubstring("cluster-metrics-cleanup"),
			"enabling clusterMetrics must add the cleanup finalizer")

		By("expecting the managed kube-state-metrics endpoint to roll out")
		assertRollout("deployment", "zbx-kube-state-metrics")
		port, err := kubectlJSONPath(namespaceFleet, "service/zbx-kube-state-metrics",
			"{.spec.ports[0].name}")
		Expect(err).NotTo(HaveOccurred())
		Expect(port).To(Equal("http"),
			"the KSM service port must be named 'http' (template discovery contract)")

		By("expecting the monitor identity and its cluster-scoped RBAC")
		// Namespaced identities live in the fleet namespace; the RBAC
		// is cluster-scoped and needs no namespace.
		for _, obj := range []string{
			"serviceaccount/zbx-kube-monitor",
			"secret/zbx-kube-monitor-token",
		} {
			_, err := runKubectl(namespaceFleet, "get", obj)
			Expect(err).NotTo(HaveOccurred(), "cluster-metrics object %s missing", obj)
		}
		for _, obj := range []string{
			"clusterrole/monitoring-zbx-kube-state-metrics",
			"clusterrolebinding/monitoring-zbx-kube-state-metrics",
		} {
			_, err := runKubectl("", "get", obj)
			Expect(err).NotTo(HaveOccurred(), "cluster-metrics object %s missing", obj)
		}

		By("expecting the token controller to fill the monitor token")
		Eventually(func(g Gomega) string {
			out, err := kubectlJSONPath(namespaceFleet, "secret/zbx-kube-monitor-token",
				"{.data.token}")
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}, "60s", "3s").ShouldNot(BeEmpty(),
			"the service-account token controller must fill the token key")

		By("expecting the host registration to park on the real Zabbix API (dummy token rejected)")
		Eventually(clusterMetricsReason, "180s", "5s").Should(Equal("HostRegistrationFailed"),
			"the operator must reach the instance's frontend API and fail authentication")

		By("disabling clusterMetrics again")
		_, err = runKubectl(namespaceFleet, "patch", "zabbix", "zbx", "--type", "json", "-p",
			`[{"op":"remove","path":"/spec/clusterMetrics"},{"op":"remove","path":"/spec/api"}]`)
		Expect(err).NotTo(HaveOccurred())

		By("expecting the operator to clean up the endpoint objects itself")
		// Kubernetes garbage collection cannot fire here: the owner
		// (the Zabbix resource) stays alive when only a spec knob is
		// removed. The operator's disable path deletes the namespaced
		// objects and the cluster-scoped RBAC and releases the
		// finalizer.
		Eventually(func(g Gomega) bool {
			_, depErr := runKubectl(namespaceFleet, "get", "deployment/zbx-kube-state-metrics")
			g.Expect(depErr).To(HaveOccurred(), "the KSM deployment must be deleted by the disable path")
			_, roleErr := runKubectl("", "get", "clusterrole/monitoring-zbx-kube-state-metrics")
			g.Expect(roleErr).To(HaveOccurred(), "the ClusterRole must be deleted by the disable path")
			return true
		}, "90s", "5s").Should(BeTrue())

		By("expecting the finalizer released and the stack back at full readiness")
		Eventually(func(g Gomega) string {
			out, err := kubectlJSONPath(namespaceFleet, "zabbix/zbx", "{.metadata.finalizers}")
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}, "60s", "5s").ShouldNot(ContainSubstring("cluster-metrics-cleanup"),
			"disabling the feature must release the cleanup finalizer")
		assertCondition("Ready")
	})

	It("places cluster-metrics objects through the namespace and namePrefix overrides", func() {
		// Runs after the cluster-metrics spec: the stack is up, the
		// dummy token Secret is still there and the feature is off.
		// A separate override namespace exercises the cross-namespace
		// placement (no owner references there) without a second stack.
		const (
			overrideNS     = "zbx-cm-place"
			overridePrefix = "placed"
			rbacName       = overrideNS + "-" + overridePrefix + "-kube-state-metrics"
		)

		By("creating the override namespace")
		_, err := runKubectl("", "create", "ns", overrideNS)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = runKubectl("", "delete", "ns", overrideNS, "--wait=false")
		})

		By("enabling clusterMetrics with the placement overrides")
		_, err = runKubectl(namespaceFleet, "patch", "zabbix", "zbx", "--type", "json", "-p", `[
			{"op":"add","path":"/spec/api","value":{"tokenSecretRef":{"name":"zbx-api-token","key":"token"}}},
			{"op":"add","path":"/spec/clusterMetrics","value":{"clusterName":"e2e-kind-cluster","tlsVerify":"none","namespace":"`+overrideNS+`","namePrefix":"`+overridePrefix+`"}}
		]`)
		Expect(err).NotTo(HaveOccurred())

		By("expecting the KSM workload in the override namespace under the override prefix")
		// kubectl rollout status fails immediately on a missing
		// resource (verified), so the deployment must be awaited
		// explicitly before the rollout check: the operator needs a
		// reconcile cycle after the patch above.
		Eventually(func(g Gomega) {
			_, err := runKubectl(overrideNS, "get",
				"deployment/"+overridePrefix+"-kube-state-metrics")
			g.Expect(err).NotTo(HaveOccurred())
		}, "120s", "2s").Should(Succeed(), "the override-placed KSM deployment was never created")
		// assertRollout is bound to the fleet namespace; the override
		// placement needs its own namespace argument.
		_, err = runKubectl(overrideNS, "rollout", "status",
			"deployment/"+overridePrefix+"-kube-state-metrics", "--timeout=600s")
		Expect(err).NotTo(HaveOccurred(), "the override-placed KSM deployment did not roll out")

		By("expecting the monitor identity in the override namespace")
		for _, obj := range []string{
			"serviceaccount/" + overridePrefix + "-kube-monitor",
			"secret/" + overridePrefix + "-kube-monitor-token",
		} {
			_, err := runKubectl(overrideNS, "get", obj)
			Expect(err).NotTo(HaveOccurred(), "override-placed object %s missing", obj)
		}

		By("expecting nothing placed in the resource's own namespace under the override prefix")
		_, defErr := runKubectl(namespaceFleet, "get",
			"deployment/"+overridePrefix+"-kube-state-metrics")
		Expect(defErr).To(HaveOccurred(), "default-placement lookup must fail for the override prefix")

		By("expecting the cluster-scoped RBAC to embed the placement namespace")
		for _, obj := range []string{
			"clusterrole/" + rbacName,
			"clusterrolebinding/" + rbacName,
		} {
			_, err := runKubectl("", "get", obj)
			Expect(err).NotTo(HaveOccurred(), "override RBAC object %s missing", obj)
		}

		By("expecting no owner references on cross-namespace objects")
		owners, err := kubectlJSONPath(overrideNS,
			"deployment/"+overridePrefix+"-kube-state-metrics", "{.metadata.ownerReferences}")
		Expect(err).NotTo(HaveOccurred())
		// A missing field jsonpaths to an empty string; any output
		// would mean a (rejected-by-design) owner reference leaked.
		Expect(owners).To(BeEmpty(),
			"k8s rejects cross-namespace owner references; placement objects must not carry one")

		By("expecting the placement recorded on the status")
		Eventually(func(g Gomega) string {
			out, err := kubectlJSONPath(namespaceFleet, "zabbix/zbx",
				"{.status.clusterMetricsNamespace}/{.status.clusterMetricsNamePrefix}")
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}, "60s", "5s").Should(Equal(overrideNS+"/"+overridePrefix),
			"the placement must be recorded and pinned on the status")

		By("disabling the feature and expecting the override placement honored on cleanup")
		_, err = runKubectl(namespaceFleet, "patch", "zabbix", "zbx", "--type", "json", "-p",
			`[{"op":"remove","path":"/spec/clusterMetrics"},{"op":"remove","path":"/spec/api"}]`)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) bool {
			_, depErr := runKubectl(overrideNS, "get",
				"deployment/"+overridePrefix+"-kube-state-metrics")
			g.Expect(depErr).To(HaveOccurred(), "the override-placed KSM deployment must be deleted on disable")
			_, roleErr := runKubectl("", "get", "clusterrole/"+rbacName)
			g.Expect(roleErr).To(HaveOccurred(), "the override-named ClusterRole must be deleted on disable")
			return true
		}, "90s", "5s").Should(BeTrue())

		Eventually(func(g Gomega) string {
			out, err := kubectlJSONPath(namespaceFleet, "zabbix/zbx", "{.metadata.finalizers}")
			g.Expect(err).NotTo(HaveOccurred())
			return out
		}, "60s", "5s").ShouldNot(ContainSubstring("cluster-metrics-cleanup"),
			"disabling the feature must release the cleanup finalizer")
		assertCondition("Ready")
	})
})
