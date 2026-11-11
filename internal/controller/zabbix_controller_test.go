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

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
)

const (
	// Fixtures shared by every Context: the CNPG app Secret, the API
	// token Secret and the CNPG cluster they point at.
	dbSecretName       = "zabbix-db-app"
	apiTokenSecretName = "zbx-api-token"
	cnpgClusterName    = "zabbix-db"
	cmClusterName      = "envtest-cluster"
)

// createSecretIfMissing tolerates Contexts sharing one envtest API
// server: the same Secrets are created by more than one Context.
// The test namespace is fixed for the whole suite.
//
//nolint:unparam // the namespace stays "default" for every Context
func createSecretIfMissing(name, namespace string, data map[string][]byte) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: data}
	err := k8sClient.Create(ctx, secret)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// dbSecretData returns the connection data the CNPG app Secret carries.
func dbSecretData() map[string][]byte {
	return map[string][]byte{
		"host":     []byte("zabbix-db-rw"),
		"port":     []byte("5432"),
		"user":     []byte("zabbix"),
		"password": []byte("secret"),
		"dbname":   []byte("zabbix"),
	}
}

var _ = Describe("Zabbix Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-zabbix"
			resourceNamespace = "default"
		)

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}}

		newDatabaseSecret := func() *corev1.Secret {
			return &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: dbSecretName, Namespace: resourceNamespace},
				Data:       dbSecretData(),
			}
		}

		newZabbix := func() *zabbixv1alpha1.Zabbix {
			return &zabbixv1alpha1.Zabbix{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec: zabbixv1alpha1.ZabbixSpec{
					Version: haTestVersion,
					Database: zabbixv1alpha1.DatabaseSpec{
						CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: cnpgClusterName},
					},
				},
			}
		}

		condition := func(z *zabbixv1alpha1.Zabbix, condType zabbixv1alpha1.ComponentConditionType) *metav1.Condition {
			for i := range z.Status.Conditions {
				if z.Status.Conditions[i].Type == string(condType) {
					return &z.Status.Conditions[i]
				}
			}
			return nil
		}

		It("should converge the stack and publish readiness conditions", func() {
			By("creating the database secret and the Zabbix resource")
			Expect(k8sClient.Create(ctx, newDatabaseSecret())).Should(Succeed())
			Expect(k8sClient.Create(ctx, newZabbix())).Should(Succeed())

			By("reconciling")
			reconciler := &ZabbixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("expecting the server statefulset and frontend deployment to exist, owned by the resource")
			serverSts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-server", Namespace: resourceNamespace}, serverSts)).Should(Succeed())
			Expect(serverSts.OwnerReferences).To(HaveLen(1))
			Expect(serverSts.OwnerReferences[0].Kind).To(Equal("Zabbix"))

			frontendDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-frontend", Namespace: resourceNamespace}, frontendDep)).Should(Succeed())

			for _, name := range []string{resourceName + "-server", resourceName + "-server-active", resourceName + "-frontend"} {
				svc := &corev1.Service{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: resourceNamespace}, svc)).Should(Succeed())
			}

			for _, name := range []string{resourceName + "-server", resourceName + "-frontend"} {
				pdb := &policyv1.PodDisruptionBudget{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: resourceNamespace}, pdb)).Should(Succeed())
				Expect(pdb.Spec.MaxUnavailable.IntValue()).To(Equal(1))
			}

			By("expecting the status to report the database resolved but components not ready (envtest runs no pods)")
			updated := &zabbixv1alpha1.Zabbix{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).Should(Succeed())
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))

			dbCond := condition(updated, zabbixv1alpha1.ConditionDatabaseReady)
			Expect(dbCond).NotTo(BeNil())
			Expect(dbCond.Status).To(Equal(metav1.ConditionTrue))

			serverCond := condition(updated, zabbixv1alpha1.ConditionServerReady)
			Expect(serverCond).NotTo(BeNil())
			Expect(serverCond.Status).To(Equal(metav1.ConditionFalse))

			Expect(condition(updated, zabbixv1alpha1.ConditionReady)).NotTo(BeNil())

			By("reconciling again without writes (idempotency — no apply storm)")
			serverRV := func() string {
				sts := &appsv1.StatefulSet{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-server", Namespace: resourceNamespace}, sts)).Should(Succeed())
				return sts.ResourceVersion
			}
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			before := serverRV()
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			// Repeated reconciles must not touch the converged objects:
			// server-side apply is a no-op when the configuration is
			// unchanged, and an unchanged status is not rewritten.
			Expect(serverRV()).To(Equal(before))
		})

		It("should requeue when the database secret is missing and report DatabaseReady=False", func() {
			nodb := newZabbix()
			nodb.Name = "test-zabbix-nodb"
			// A cluster name whose app secret does not exist anywhere.
			nodb.Spec.Database.CNPG.ClusterName = "zabbix-db-missing"
			Expect(k8sClient.Create(ctx, nodb)).Should(Succeed())

			reconciler := &ZabbixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: nodb.Name, Namespace: resourceNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			updated := &zabbixv1alpha1.Zabbix{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodb.Name, Namespace: resourceNamespace}, updated)).Should(Succeed())
			dbCond := condition(updated, zabbixv1alpha1.ConditionDatabaseReady)
			Expect(dbCond).NotTo(BeNil())
			Expect(dbCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(dbCond.Reason).To(Equal("DatabaseUnresolved"))
		})

		It("should ignore a resource that no longer exists", func() {
			reconciler := &ZabbixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: resourceNamespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When cluster-metrics ingestion is enabled", func() {
		const (
			cmName            = "test-zabbix-metrics"
			resourceNamespace = "default"
		)

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cmName, Namespace: resourceNamespace}}

		newZabbixWithMetrics := func() *zabbixv1alpha1.Zabbix {
			return &zabbixv1alpha1.Zabbix{
				ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: resourceNamespace},
				Spec: zabbixv1alpha1.ZabbixSpec{
					Version: haTestVersion,
					Database: zabbixv1alpha1.DatabaseSpec{
						CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: cnpgClusterName},
					},
					API: &zabbixv1alpha1.APISpec{
						TokenSecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: apiTokenSecretName},
							Key:                  haTestTokenKey,
						},
					},
					ClusterMetrics: &zabbixv1alpha1.ClusterMetricsSpec{
						ClusterName: cmClusterName,
					},
				},
			}
		}

		condition := func(z *zabbixv1alpha1.Zabbix, condType zabbixv1alpha1.ComponentConditionType) *metav1.Condition {
			for i := range z.Status.Conditions {
				if z.Status.Conditions[i].Type == string(condType) {
					return &z.Status.Conditions[i]
				}
			}
			return nil
		}

		It("should converge the endpoint, guard deletion, and wait for the monitor token", func() {
			By("creating the database secret, the API token secret and the Zabbix resource")
			createSecretIfMissing(dbSecretName, resourceNamespace, dbSecretData())
			createSecretIfMissing(apiTokenSecretName, resourceNamespace, map[string][]byte{haTestTokenKey: []byte("test-zabbix-token")})
			Expect(k8sClient.Create(ctx, newZabbixWithMetrics())).Should(Succeed())

			reconciler := &ZabbixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("expecting the namespaced cluster-metrics objects to exist, owned by the resource")
			ksmDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: zabbix.KubeStateMetricsName(cmName), Namespace: resourceNamespace}, ksmDep)).Should(Succeed())
			Expect(ksmDep.OwnerReferences[0].Kind).To(Equal("Zabbix"))
			Expect(ksmDep.Spec.Template.Spec.ServiceAccountName).To(Equal(zabbix.KubeStateMetricsName(cmName)))

			ksmSA := &corev1.ServiceAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: zabbix.KubeStateMetricsName(cmName), Namespace: resourceNamespace}, ksmSA)).Should(Succeed())

			ksmSvc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: zabbix.KubeStateMetricsName(cmName), Namespace: resourceNamespace}, ksmSvc)).Should(Succeed())
			Expect(ksmSvc.Name).To(HaveSuffix("kube-state-metrics"))

			monitorSA := &corev1.ServiceAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: zabbix.MonitorServiceAccountName(cmName), Namespace: resourceNamespace}, monitorSA)).Should(Succeed())

			tokenSecret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: zabbix.MonitorTokenSecretName(cmName), Namespace: resourceNamespace}, tokenSecret)).Should(Succeed())
			Expect(tokenSecret.Annotations).To(HaveKeyWithValue(corev1.ServiceAccountNameKey, zabbix.MonitorServiceAccountName(cmName)))

			By("expecting the cluster-scoped RBAC to exist")
			role := &rbacv1.ClusterRole{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: zabbix.ClusterMetricsRBACName(newZabbixWithMetrics())}, role)).Should(Succeed())
			binding := &rbacv1.ClusterRoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: zabbix.ClusterMetricsRBACName(newZabbixWithMetrics())}, binding)).Should(Succeed())

			By("expecting the cleanup finalizer on the resource")
			cm := &zabbixv1alpha1.Zabbix{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, cm)).Should(Succeed())
			Expect(controllerutil.ContainsFinalizer(cm, ClusterMetricsFinalizer)).To(BeTrue())

			By("expecting ClusterMetricsReady=False: the token controller has not filled the Secret (envtest runs no controllers)")
			updated := &zabbixv1alpha1.Zabbix{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).Should(Succeed())
			cmCond := condition(updated, zabbixv1alpha1.ConditionClusterMetricsReady)
			Expect(cmCond).NotTo(BeNil())
			Expect(cmCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cmCond.Reason).To(Equal("MonitorTokenPending"))
		})

		It("should clean up the cluster-scoped RBAC on deletion", func() {
			By("removing the API token secret (host cleanup is skipped without a token)")
			apiSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: apiTokenSecretName, Namespace: resourceNamespace}}
			err := k8sClient.Delete(ctx, apiSecret)
			Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue())

			By("deleting the resource (finalizer already present from the first spec)")
			cm := &zabbixv1alpha1.Zabbix{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: resourceNamespace}}
			Expect(k8sClient.Delete(ctx, cm)).Should(Succeed())

			reconciler := &ZabbixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("expecting the ClusterRole/Binding deleted (host deletion is skipped without an API endpoint)")
			role := &rbacv1.ClusterRole{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: zabbix.ClusterMetricsRBACName(newZabbixWithMetrics())}, role)
			Expect(err).To(HaveOccurred())

			By("expecting the resource gone (finalizer released)")
			Expect(k8sClient.Get(ctx, req.NamespacedName, cm)).To(HaveOccurred())
		})
	})

	Context("When cluster-metrics is disabled after being enabled", func() {
		const (
			cmName            = "test-zabbix-metrics-disable"
			resourceNamespace = "default"
		)

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: cmName, Namespace: resourceNamespace}}

		enabledZabbix := func() *zabbixv1alpha1.Zabbix {
			return &zabbixv1alpha1.Zabbix{
				ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: resourceNamespace},
				Spec: zabbixv1alpha1.ZabbixSpec{
					Version: haTestVersion,
					Database: zabbixv1alpha1.DatabaseSpec{
						CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: cnpgClusterName},
					},
					API: &zabbixv1alpha1.APISpec{
						TokenSecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: apiTokenSecretName},
							Key:                  haTestTokenKey,
						},
					},
					ClusterMetrics: &zabbixv1alpha1.ClusterMetricsSpec{
						ClusterName: cmClusterName,
					},
				},
			}
		}

		It("should remove every cluster-metrics object and release the finalizer", func() {
			By("enabling the feature and reconciling once")
			createSecretIfMissing(dbSecretName, resourceNamespace, dbSecretData())
			// The enable-side object assertions live in the other
			// Context; here the disable path is the subject.
			Expect(k8sClient.Create(ctx, enabledZabbix())).Should(Succeed())
			reconciler := &ZabbixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("disabling the feature flag")
			updated := &zabbixv1alpha1.Zabbix{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).Should(Succeed())
			updated.Spec.ClusterMetrics = nil
			updated.Spec.API = nil
			Expect(k8sClient.Update(ctx, updated)).Should(Succeed())

			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("expecting the namespaced cluster-metrics objects gone")
			ksmDep := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.KubeStateMetricsName(cmName), Namespace: resourceNamespace}, ksmDep)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "KSM deployment must be deleted on disable")
			monitorSA := &corev1.ServiceAccount{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.MonitorServiceAccountName(cmName), Namespace: resourceNamespace}, monitorSA)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "monitor service account must be deleted on disable")
			tokenSecret := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.MonitorTokenSecretName(cmName), Namespace: resourceNamespace}, tokenSecret)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "monitor token secret must be deleted on disable")

			By("expecting the cluster-scoped RBAC gone")
			role := &rbacv1.ClusterRole{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.ClusterMetricsRBACName(enabledZabbix())}, role)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ClusterRole must be deleted on disable")

			By("expecting the stale condition dropped and the finalizer released")
			final := &zabbixv1alpha1.Zabbix{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, final)).Should(Succeed())
			Expect(controllerutil.ContainsFinalizer(final, ClusterMetricsFinalizer)).To(BeFalse())
			for _, c := range final.Status.Conditions {
				Expect(c.Type).NotTo(Equal(string(zabbixv1alpha1.ConditionClusterMetricsReady)),
					"the stale ClusterMetricsReady condition must be dropped on disable")
			}
			Expect(final.Status.ClusterMetricsHostName).To(BeEmpty())
			Expect(final.Status.ClusterMetricsNamespace).To(BeEmpty())
			Expect(final.Status.ClusterMetricsNamePrefix).To(BeEmpty())
		})
	})

	Context("When clusterMetrics placement is overridden", func() {
		const (
			placementName      = "test-zabbix-metrics-place"
			resourceNamespace  = "default"
			overrideNamespace  = "cluster-metrics-place"
			overrideNamePrefix = "custom-ksm"
		)

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: placementName, Namespace: resourceNamespace}}

		It("should deploy into the override namespace with the override prefix and honor the record on disable", func() {
			By("creating the override namespace, the secrets and the resource")
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: overrideNamespace},
			})).Should(Succeed())
			createSecretIfMissing(dbSecretName, resourceNamespace, dbSecretData())
			createSecretIfMissing(apiTokenSecretName, resourceNamespace, map[string][]byte{haTestTokenKey: []byte("test-zabbix-token")})
			Expect(k8sClient.Create(ctx, &zabbixv1alpha1.Zabbix{
				ObjectMeta: metav1.ObjectMeta{Name: placementName, Namespace: resourceNamespace},
				Spec: zabbixv1alpha1.ZabbixSpec{
					Version: haTestVersion,
					Database: zabbixv1alpha1.DatabaseSpec{
						CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: cnpgClusterName},
					},
					API: &zabbixv1alpha1.APISpec{
						TokenSecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: apiTokenSecretName},
							Key:                  haTestTokenKey,
						},
					},
					ClusterMetrics: &zabbixv1alpha1.ClusterMetricsSpec{
						ClusterName: cmClusterName,
						Namespace:   overrideNamespace,
						NamePrefix:  overrideNamePrefix,
					},
				},
			})).Should(Succeed())

			reconciler := &ZabbixReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			By("expecting the objects in the override namespace under the override names")
			ksmDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.KubeStateMetricsName(overrideNamePrefix), Namespace: overrideNamespace}, ksmDep)).
				Should(Succeed(), "the KSM deployment must be placed in the override namespace")
			monitorSA := &corev1.ServiceAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.MonitorServiceAccountName(overrideNamePrefix), Namespace: overrideNamespace}, monitorSA)).
				Should(Succeed())
			tokenSecret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.MonitorTokenSecretName(overrideNamePrefix), Namespace: overrideNamespace}, tokenSecret)).
				Should(Succeed())

			By("expecting nothing created in the resource's own namespace")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.KubeStateMetricsName(overrideNamePrefix), Namespace: resourceNamespace},
				&appsv1.Deployment{})).To(HaveOccurred(), "default-placement lookups must fail")

			By("expecting no owner reference on cross-namespace objects (k8s rejects cross-namespace owners)")
			Expect(ksmDep.OwnerReferences).To(BeEmpty(),
				"objects outside the resource's namespace cannot carry owner references")

			By("expecting the cluster-scoped RBAC to embed the override placement")
			role := &rbacv1.ClusterRole{}
			placementZabbix := &zabbixv1alpha1.Zabbix{ObjectMeta: metav1.ObjectMeta{
				Name: placementName, Namespace: resourceNamespace}}
			placementZabbix.Spec.ClusterMetrics = &zabbixv1alpha1.ClusterMetricsSpec{
				Namespace: overrideNamespace, NamePrefix: overrideNamePrefix}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.ClusterMetricsRBACName(placementZabbix)}, role)).Should(Succeed())

			By("expecting the placement recorded on the status")
			updated := &zabbixv1alpha1.Zabbix{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).Should(Succeed())
			Expect(updated.Status.ClusterMetricsNamespace).To(Equal(overrideNamespace))
			Expect(updated.Status.ClusterMetricsNamePrefix).To(Equal(overrideNamePrefix))

			By("disabling the feature and expecting the override placement honored")
			updated.Spec.ClusterMetrics = nil
			updated.Spec.API = nil
			Expect(k8sClient.Update(ctx, updated)).Should(Succeed())
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.KubeStateMetricsName(overrideNamePrefix), Namespace: overrideNamespace},
				&appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the override-placed KSM deployment must be deleted")
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name: zabbix.ClusterMetricsRBACName(placementZabbix)}, role)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the override-named ClusterRole must be deleted")

			final := &zabbixv1alpha1.Zabbix{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, final)).Should(Succeed())
			Expect(controllerutil.ContainsFinalizer(final, ClusterMetricsFinalizer)).To(BeFalse())
			Expect(final.Status.ClusterMetricsNamespace).To(BeEmpty())
			Expect(final.Status.ClusterMetricsNamePrefix).To(BeEmpty())
		})
	})
})
