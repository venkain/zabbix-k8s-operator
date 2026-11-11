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
})
