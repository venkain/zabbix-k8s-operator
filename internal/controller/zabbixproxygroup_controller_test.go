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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
	"github.com/venkain/zabbix-k8s-operator/internal/zabbix"
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

		// reconcileGroup runs one reconcile pass against a stubbed
		// JSON-RPC endpoint (empty server-side group and proxies; the
		// mutations are accepted). The API endpoint resolves an
		// in-cluster DNS name envtest cannot serve — the transport
		// rewrites it to the stub.
		reconcileGroup := func() error {
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string `json:"method"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				responses := map[string]string{
					"proxygroup.get":    respPGEmpty,
					"proxygroup.create": respPGGroupIDs,
					"proxy.get":         respPGEmpty,
					"proxy.create":      respPGMemberMake,
				}
				resp, ok := responses[req.Method]
				if !ok {
					// An unexpected method makes the reconcile fail;
					// the error assertion reports it.
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(resp))
			}))
			defer stub.Close()

			reconciler := &ZabbixProxyGroupReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				HTTP:   &http.Client{Transport: rewriteTransport{to: stub}},
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			return err
		}

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
			if err != nil && apierrors.IsNotFound(err) {
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
			if err != nil && apierrors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ZabbixProxyGroup")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// No manager runs in this suite: the cleanup finalizer is
			// released only by a reconcile pass, which needs a stub for
			// the server-side cleanup calls (the stub reports no group,
			// so the cleanup is a no-op and releases immediately).
			By("Releasing the cleanup finalizer through a reconcile pass")
			Eventually(func() error {
				if err := reconcileGroup(); err != nil {
					return err
				}
				stored := &zabbixv1alpha1.ZabbixProxyGroup{}
				err := k8sClient.Get(ctx, typeNamespacedName, stored)
				if err != nil && apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}, "10s", "100ms").To(Succeed())

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
			Expect(reconcileGroup()).To(Succeed())

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

		// The maxUnavailable wedge, as observed in the kind lab: a member
		// pod crash-looping on an older template revision is never Ready,
		// counts against the update budget, and cannot be replaced by the
		// StatefulSet controller — the rollout deadlocks on the stale
		// revision hash. The reconciler recycles such pods so the
		// controller can recreate them from the converged template.
		It("should recycle a stale not-Ready member pod mid-rollout", func() {
			currentRevision := zabbix.ProxyName(resourceName) + "-revgood"
			staleRevision := "revstale"

			Expect(reconcileGroup()).To(Succeed())
			By("Publishing rollout state on the member StatefulSet")
			memberSts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: resourceNamespace, Name: zabbix.ProxyName(resourceName),
			}, memberSts)).To(Succeed())
			memberSts.Status.UpdateRevision = currentRevision
			Expect(k8sClient.Status().Update(ctx, memberSts)).To(Succeed())

			By("Planting member pods: one stale and not-Ready, one current and not-Ready")
			plant := func(name, revision string) *corev1.Pod {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: resourceNamespace,
						Labels: map[string]string{
							zabbix.ComponentLabel:      zabbix.ComponentProxy,
							zabbix.InstanceLabel:       resourceName,
							"controller-revision-hash": revision,
						},
					},
					// Placeholder container: the API server requires a
					// container spec; envtest has no kubelet to run it.
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "placeholder",
							Image: "placeholder:latest",
						}},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				}
				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				pod.Status.Conditions = []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionUnknown,
				}}
				Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
				return pod
			}
			stalePod := plant(zabbix.ProxyName(resourceName)+"-0", staleRevision)
			// Real pods carry the FULL revision name, exactly like
			// Status.UpdateRevision.
			currentPod := plant(zabbix.ProxyName(resourceName)+"-1", currentRevision)

			By("Reconciling")
			Expect(reconcileGroup()).To(Succeed())

			// The stale pod starved its own replacement: recycle it. The
			// current-revision pod is simply unhealthy — the StatefulSet
			// controller owns that situation, not us.
			stalePodAfter := &corev1.Pod{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stalePod), stalePodAfter)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "stale pod should have been recycled")
			currentPodAfter := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(currentPod), currentPodAfter)).To(Succeed())
		})
	})
})
