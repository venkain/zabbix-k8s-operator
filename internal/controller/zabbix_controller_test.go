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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

var _ = Describe("Zabbix Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-zabbix"
			resourceNamespace = "default"
		)

		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}}

		newDatabaseSecret := func() *corev1.Secret {
			return &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "zabbix-db-app", Namespace: resourceNamespace},
				Data: map[string][]byte{
					"host":     []byte("zabbix-db-rw"),
					"port":     []byte("5432"),
					"user":     []byte("zabbix"),
					"password": []byte("secret"),
					"dbname":   []byte("zabbix"),
				},
			}
		}

		newZabbix := func() *zabbixv1alpha1.Zabbix {
			return &zabbixv1alpha1.Zabbix{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec: zabbixv1alpha1.ZabbixSpec{
					Version: "8.0",
					Database: zabbixv1alpha1.DatabaseSpec{
						CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: "zabbix-db"},
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

			By("expecting the server and frontend deployments to exist, owned by the resource")
			serverDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-server", Namespace: resourceNamespace}, serverDep)).Should(Succeed())
			Expect(serverDep.OwnerReferences).To(HaveLen(1))
			Expect(serverDep.OwnerReferences[0].Kind).To(Equal("Zabbix"))

			frontendDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-frontend", Namespace: resourceNamespace}, frontendDep)).Should(Succeed())

			for _, name := range []string{resourceName + "-server", resourceName + "-server-active", resourceName + "-frontend"} {
				svc := &corev1.Service{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: resourceNamespace}, svc)).Should(Succeed())
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

			By("reconciling again without changes (idempotency)")
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
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
})
