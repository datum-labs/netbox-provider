// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const rotatedToken = "synthetic-rotated-token-value"

// setToken replaces the token in the harness Secret, without touching the
// NetBoxSource.
func (h *harness) setToken(value string) {
	var secret corev1.Secret
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: "netbox-token"}, &secret)).To(Succeed())
	secret.Data = map[string][]byte{"token": []byte(value)}
	Expect(k8sClient.Update(ctx, &secret)).To(Succeed())
}

// labelSecret changes a Secret's metadata only.
func labelSecret(namespace, name, value string) {
	var secret corev1.Secret
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret)).To(Succeed())
	secret.Labels = map[string]string{"example.com/touched": value}
	Expect(k8sClient.Update(ctx, &secret)).To(Succeed())
}

// secretGet matches a get of one Secret.
func secretGet(namespace, name string) func(apiRequest) bool {
	return func(r apiRequest) bool {
		return r.Method == http.MethodGet && r.Path == "/api/v1/namespaces/"+namespace+"/secrets/"+name
	}
}

// settledRequests waits until the reconciles the manager queued at start have
// finished, and returns the NetBox requests made by then.
func settledRequests(h *harness) int {
	last := -1
	Eventually(func() bool {
		n := h.server.Requests()
		settled := n == last
		last = n
		return settled
	}, 20*time.Second, time.Second).Should(BeTrue())
	return last
}

var _ = Describe("token Secret watch", func() {
	It("picks up a rotated token without a spec edit or waiting for the interval", func() {
		h := newHarness().withSourceAndToken("")
		h.server.AcceptOnly("Bearer " + rotatedToken)
		h.startManager()

		Eventually(func() string { return condition(h.source(), ConditionReady).Reason }, 20*time.Second).Should(Equal(ReasonTokenRejected))
		generation := h.source().Generation

		h.setToken(rotatedToken)
		Eventually(func() metav1.ConditionStatus { return condition(h.source(), ConditionSourceObserved).Status }, 20*time.Second).Should(Equal(metav1.ConditionTrue))
		s := h.source()
		Expect(s.Generation).To(Equal(generation), "the spec was not edited")
		Expect(condition(s, ConditionConfigurationResolved)).To(SatisfyAll(HaveField("Status", metav1.ConditionTrue), HaveField("Reason", ReasonResolved)))
		Expect(condition(s, ConditionApplied).Reason).To(Equal(ReasonObserveOnly))
		Expect(h.server.Authorizations()[len(h.server.Authorizations())-1]).To(Equal("Bearer " + rotatedToken))
	})

	It("does not resend a rejected token when the Secret changes but its token does not", func() {
		h := newHarness().withSourceAndToken("")
		h.server.AcceptOnly("Bearer " + rotatedToken)
		h.startManager()

		Eventually(func() string { return condition(h.source(), ConditionReady).Reason }, 20*time.Second).Should(Equal(ReasonTokenRejected))
		requests := settledRequests(h)
		labelSecret(h.namespace, "netbox-token", "once")
		Consistently(h.server.Requests, 3*time.Second).Should(Equal(requests), "the rejected token waits for its retry time")
		Expect(condition(h.source(), ConditionReady).Reason).To(Equal(ReasonTokenRejected))
	})

	It("enqueues nothing for a Secret no source references, nor for a metadata edit that keeps the token, and the source when its token changes", func() {
		h := newHarness().withSourceAndToken("")
		unrelated := tokenSecret(h.namespace, "token", "synthetic-unrelated-value")
		unrelated.Name = "unrelated"
		Expect(k8sClient.Create(ctx, unrelated)).To(Succeed())
		h.startManager()

		Eventually(func() string { return condition(h.source(), ConditionApplied).Reason }, 20*time.Second).Should(Equal(ReasonObserveOnly))
		requests := settledRequests(h)

		labelSecret(h.namespace, "unrelated", "once")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(unrelated), unrelated)).To(Succeed())
		unrelated.Data = map[string][]byte{"token": []byte("synthetic-unrelated-rotated")}
		Expect(k8sClient.Update(ctx, unrelated)).To(Succeed())
		Consistently(h.server.Requests, 3*time.Second).Should(Equal(requests), "an unrelated Secret enqueues nothing")

		labelSecret(h.namespace, "netbox-token", "once")
		Consistently(h.server.Requests, 3*time.Second).Should(Equal(requests), "a metadata edit that keeps the token reads nothing from NetBox")
		Expect(h.api.Requests(secretGet(h.namespace, "netbox-token"))).NotTo(BeEmpty(), "the watch read the token to compare it")

		h.setToken(rotatedToken)
		Eventually(h.server.Requests, 20*time.Second).Should(BeNumerically(">", requests), "a new token enqueues its source")
		Expect(h.server.Authorizations()[len(h.server.Authorizations())-1]).To(Equal("Bearer " + rotatedToken))
	})

	It("lists and watches Secrets only as metadata, never as typed Secrets", func() {
		h := newHarness().withSourceAndToken("")
		mgr := h.startManager()
		cached := &metav1.PartialObjectMetadata{}
		cached.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		Eventually(func() error {
			return mgr.GetCache().Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: "netbox-token"}, cached)
		}, 20*time.Second).Should(Succeed())
		Eventually(func() string { return condition(h.source(), ConditionApplied).Reason }, 20*time.Second).Should(Equal(ReasonObserveOnly))

		// An informer lists and watches the collection. A metadata informer
		// asks for PartialObjectMetadata; a typed one would ask for Secrets.
		collection := h.api.Requests(func(r apiRequest) bool {
			return r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/secrets")
		})
		Expect(collection).NotTo(BeEmpty(), "the Secret informer listed and watched")
		Expect(collection).To(ContainElement(HaveField("Watch", true)))
		for _, r := range collection {
			Expect(r.Path).To(Equal("/api/v1/namespaces/"+h.namespace+"/secrets"), "Secrets are listed only in the manager's namespace")
			Expect(r.Accept).To(ContainSubstring("as=PartialObjectMetadata"), "a Secret list or watch asked for full Secrets: %+v", r)
		}
	})

	It("ignores Secret resyncs, which carry no change", func() {
		h := newHarness().withSourceAndToken("")
		h.startManager(200 * time.Millisecond)
		Eventually(func() string { return condition(h.source(), ConditionApplied).Reason }, 20*time.Second).Should(Equal(ReasonObserveOnly))
		settledRequests(h)
		// Each event for the referenced Secret makes the watch read it, so
		// resyncs that passed the predicate would show up here.
		gets := func() int { return len(h.api.Requests(secretGet(h.namespace, "netbox-token"))) }
		before := gets()
		Consistently(gets, 3*time.Second).Should(Equal(before), "a resync enqueued work")

		labelSecret(h.namespace, "netbox-token", "once")
		Eventually(gets, 10*time.Second).Should(BeNumerically(">", before), "a real change still reaches the watch")
	})

	It("caches Secret metadata only, without annotations or data", func() {
		h := newHarness()
		secret := tokenSecret(h.namespace, "token", testToken)
		// kubectl's client-side apply copies the whole object, data included,
		// into this annotation.
		secret.Annotations = map[string]string{corev1.LastAppliedConfigAnnotation: `{"data":{"token":"` + testToken + `"}}`}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		Expect(k8sClient.Create(ctx, newSource(h.namespace, h.server.RootURL(), ""))).To(Succeed())
		mgr := h.startManager()

		cached := &metav1.PartialObjectMetadata{}
		cached.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		Eventually(func() error {
			return mgr.GetCache().Get(ctx, client.ObjectKeyFromObject(secret), cached)
		}, 20*time.Second).Should(Succeed())
		Expect(cached.Annotations).To(BeEmpty())
		Expect(cached.ManagedFields).To(BeEmpty())
		Expect(cached.ResourceVersion).NotTo(BeEmpty())

		typed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: secret.Annotations}, Data: secret.Data}
		out, err := stripSecret(typed)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.(*corev1.Secret).Data).To(BeNil(), "a typed Secret never keeps its data in the cache")
		Expect(out.(*corev1.Secret).Annotations).To(BeNil())
	})
})

var _ = Describe("rejected token", func() {
	It("is not sent again for the same generation until its retry time, and a new token is", func() {
		h := newHarness().withSourceAndToken(netboxv1alpha1.SourceModeCreate)
		h.server.AcceptOnly("Bearer " + rotatedToken)
		_, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.server.Requests()).To(Equal(1))

		result, err := h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.server.Requests()).To(Equal(1), "the same token is not resent")
		Expect(result.RequeueAfter).To(Equal(rejectedCredentialsInterval))

		h.r.Now = func() time.Time { return testNow.Add(rejectedCredentialsInterval) }
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(h.server.Requests()).To(Equal(2), "the token is retried once its retry time has passed")

		h.setToken(rotatedToken)
		_, err = h.reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(condition(h.source(), ConditionSourceObserved).Status).To(Equal(metav1.ConditionTrue))
	})
})
