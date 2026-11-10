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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

var _ = Describe("ZabbixProxyGroup Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		zabbixproxygroup := &zabbixv1alpha1.ZabbixProxyGroup{}

		// Shared literals for the referenced-server wiring (constants so
		// repeated strings satisfy goconst).
		const (
			pgTestRefName    = "pg-test-zabbix"
			pgTestTokenName  = "pg-test-token"
			respPGEmpty      = `{"jsonrpc":"2.0","result":[],"id":1}`
			respPGMemberMake = `{"jsonrpc":"2.0","result":{"proxyids":["11"]},"id":1}`
		)

		BeforeEach(func() {
			// The referenced Zabbix resource is created here (not assumed
			// from other specs — Ginkgo shuffles spec order): its api
			// block wiring is required for the group maintenance pass.
			By("creating the referenced Zabbix resource with API wiring")
			refZabbix := &zabbixv1alpha1.Zabbix{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pgTestRefName,
					Namespace: resourceNamespace,
				},
				Spec: zabbixv1alpha1.ZabbixSpec{
					Version: haTestVersion,
					Database: zabbixv1alpha1.DatabaseSpec{
						CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: "pg-test-db"},
					},
					API: &zabbixv1alpha1.APISpec{
						TokenSecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: pgTestTokenName},
							Key:                  "token",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, refZabbix)).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: pgTestTokenName, Namespace: resourceNamespace},
				Data:       map[string][]byte{"token": []byte("test-token")},
			})).To(Succeed())

			By("creating the custom resource for the Kind ZabbixProxyGroup")
			err := k8sClient.Get(ctx, typeNamespacedName, zabbixproxygroup)
			if err != nil && errors.IsNotFound(err) {
				resource := &zabbixv1alpha1.ZabbixProxyGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: zabbixv1alpha1.ZabbixProxyGroupSpec{
						Version:   haTestVersion,
						GroupName: "test-group",
						ServerRef: pgTestRefName,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &zabbixv1alpha1.ZabbixProxyGroup{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ZabbixProxyGroup")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Cleaning up the referenced Zabbix resource and token Secret")
			refZabbix := &zabbixv1alpha1.Zabbix{ObjectMeta: metav1.ObjectMeta{
				Name: pgTestRefName, Namespace: resourceNamespace,
			}}
			Expect(k8sClient.Delete(ctx, refZabbix)).To(Succeed())
			tokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: pgTestTokenName, Namespace: resourceNamespace,
			}}
			Expect(k8sClient.Delete(ctx, tokenSecret)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("stubbing the frontend JSON-RPC endpoint")
			responses := map[string]string{
				"proxygroup.get":    respPGEmpty,
				"proxygroup.create": respPGGroupIDs,
				"proxy.get":         respPGEmpty,
				"proxy.create":      respPGMemberMake,
			}
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string `json:"method"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				resp, ok := responses[req.Method]
				if !ok {
					// An unexpected method makes the reconcile fail; the
					// error assertion below reports it.
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(resp))
			}))
			defer stub.Close()

			By("Reconciling the created resource")
			controllerReconciler := &ZabbixProxyGroupReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				// The API endpoint resolves an in-cluster DNS name;
				// envtest has no such Service — rewrite to the stub.
				HTTP: &http.Client{Transport: rewriteTransport{to: stub}},
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Checking the published conditions")
			stored := &zabbixv1alpha1.ZabbixProxyGroup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, stored)).To(Succeed())
			serverResolved := apimeta.FindStatusCondition(stored.Status.Conditions,
				string(zabbixv1alpha1.ConditionProxyGroupServerResolved))
			Expect(serverResolved).NotTo(BeNil())
			Expect(serverResolved.Status).To(Equal(metav1.ConditionTrue))
			apiReady := apimeta.FindStatusCondition(stored.Status.Conditions,
				string(zabbixv1alpha1.ConditionProxyGroupAPIReady))
			Expect(apiReady).NotTo(BeNil())
			Expect(apiReady.Status).To(Equal(metav1.ConditionTrue))
		})
	})
})
